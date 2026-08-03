// internal/processor/processor.go
package processor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"nfo_updater/internal/backup"
	"nfo_updater/internal/config"
	"nfo_updater/internal/db"
	"nfo_updater/internal/lock"
	"nfo_updater/internal/logging"
	"nfo_updater/internal/mediaserver"
	"nfo_updater/internal/providers"
	"nfo_updater/internal/version"
)

// httpTimeout — общий таймаут на один запрос к любому провайдеру.
// Отдельно от circuit breaker: таймаут защищает от "висящего" соединения,
// breaker — от повторяющихся сбоев.
const httpTimeout = 20 * time.Second

// mediaServerTimeout — на всё общение с медиасерверами в конце прогона.
// Меньше обычного: сервер стоит в локальной сети, а запрос всего лишь
// ставит ему задание на сканирование.
const mediaServerTimeout = 30 * time.Second

// Runner держит долгоживущие ресурсы (БД, http-клиент) между прогонами
// демона и переживает hot-reload конфига.
//
// bootLogger — "загрузочный" логгер, пишущий только в консоль. Им
// пользуются сообщения, которым файл лога прогона ещё или уже не положен:
// занятый lock, неудача при открытии самого файла лога. Логгер прогона
// создаётся внутри Run() и живёт ровно столько же, сколько прогон.
//
// Deps собирается ЗАНОВО на каждый прогон — в нём живёт состояние одного
// прогона (RunStats, CircuitBreaker, quotaWarned, отключённые ключи
// в пулах провайдеров), которое обязано обнуляться между прогонами:
// провайдер, отвалившийся в прошлый понедельник, не должен считаться
// отключённым в этот.
type Runner struct {
	mu         sync.Mutex
	cfg        *config.Config
	bootLogger *logging.Logger

	configPath string
	db         *db.DB
	http       *http.Client
}

func NewRunner(cfg *config.Config, database *db.DB, configPath string, bootLogger *logging.Logger) *Runner {
	return &Runner{
		cfg:        cfg,
		bootLogger: bootLogger,
		configPath: configPath,
		db:         database,
		http:       &http.Client{Timeout: httpTimeout},
	}
}

// Reload подменяет конфиг и загрузочный логгер между прогонами (вызывается
// по SIGHUP из main.go после успешной перечитки файла конфига). Демон
// гарантирует, что reload не приходит в середине прогона, но мьютекс всё
// равно нужен: форсированный прогон по SIGUSR1 идёт из другой горутины.
func (r *Runner) Reload(cfg *config.Config, bootLogger *logging.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
	r.bootLogger = bootLogger
}

func (r *Runner) current() (*config.Config, *logging.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg, r.bootLogger
}

// NewDeps собирает зависимости одного прогона.
//
// Порядок Providers имеет значение: fetchRatingsWithFallback идёт по списку
// сверху вниз и останавливается, как только собраны все запрошенные в
// конфиге источники. MDBList ставится ПЕРВЫМ (если настроен), потому что
// один его запрос закрывает сразу imdb/tmdb/trakt/tomatoes/popcorn/
// metacritic, тогда как OMDb отдаёт меньше, а TMDb — только свой
// собственный рейтинг. Так расходуется меньше суточной квоты.
func NewDeps(cfg *config.Config, database *db.DB, logger *logging.Logger, httpClient *http.Client) *Deps {
	var list []providers.Provider

	if len(cfg.MDBListAPIKeys) > 0 {
		list = append(list, providers.NewMDBListProvider(
			cfg.MDBListAPIKeys, cfg.MDBListDailyLimitPerKey, database, logger, httpClient))
	}
	if len(cfg.OMDbAPIKeys) > 0 {
		list = append(list, providers.NewOMDbProvider(
			cfg.OMDbAPIKeys, cfg.OMDbDailyLimitPerKey, database, logger, httpClient))
	}

	// TMDb создаётся всегда, когда есть ключ: он нужен не только как
	// провайдер рейтинга, но и как ЕДИНСТВЕННЫЙ источник даты для фикса
	// <premiered> (FetchReleaseDate) — даже если TMDB_RATING=no.
	var tmdb *providers.TMDbProvider
	if cfg.TMDbAPIKey != "" {
		tmdb = providers.NewTMDbProvider(cfg.TMDbAPIKey, httpClient)
		if cfg.TMDbRating {
			list = append(list, tmdb)
		}
	}

	return &Deps{
		Config:    cfg,
		DB:        database,
		Providers: list,
		TMDb:      tmdb,
		Breaker: providers.NewCircuitBreaker(
			cfg.CircuitBreakerFailures,
			time.Duration(cfg.CircuitBreakerBackoffSeconds)*time.Second),
		Logger:      logger,
		Stats:       NewRunStats(),
		quotaWarned: make(map[string]bool),
	}
}

// Run выполняет один полный прогон. Сигнатура совпадает с daemon.RunFunc,
// так что годится и для демона, и для одноразового запуска.
//
// Порядок первых двух шагов важен. Сначала берётся flock, и только потом
// открывается файл лога: иначе запуск, отклонённый из-за занятой
// блокировки, успевал бы создать пустой лог, а ротация списала бы за него
// слот из LOG_LIMIT — десяток параллельных попыток вытеснил бы всю
// историю логов пустышками.
func (r *Runner) Run(ctx context.Context) error {
	cfg, bootLogger := r.current()

	lk, err := lock.Acquire()
	if err != nil {
		if errors.Is(err, lock.ErrBusy) {
			bootLogger.Event("[RUN_REJECTED] %v", err)
		} else {
			bootLogger.Event("[ERROR] cannot acquire lock: %v", err)
		}
		return err
	}
	defer func() {
		if err := lk.Release(); err != nil {
			bootLogger.Event("[ERROR] cannot release lock: %v", err)
		}
	}()

	logger := bootLogger
	if cfg.LogEnabled {
		runLog, err := logging.OpenRunLog(cfg.LogDir, cfg.LogLimit, time.Now(), cfg.Describe(r.configPath))
		if err != nil {
			// Неудача ротации возвращает открытый лог вместе с ошибкой —
			// тогда пишем в него же и жалобу, а прогон продолжаем.
			bootLogger.Event("[ERROR] log file: %v", err)
		}
		if runLog != nil {
			defer runLog.Close()
			logger = logging.New(runLog.Writer(), os.Stdout, cfg.LogVerbose)
			bootLogger.Event("[LOG_FILE] %s", runLog.Path())
		}
	}

	deps := NewDeps(cfg, r.db, logger, r.http)

	logger.Event("[RUN_START] nfo_updater %s (built %s)", version.Version, version.BuildDate)

	if len(deps.Providers) == 0 {
		logger.Event("[ERROR] no rating providers configured, nothing to do")
		return fmt.Errorf("no rating providers configured")
	}

	if err := checkKeys(ctx, deps); err != nil {
		return err
	}

	// Медиасерверы опрашиваются на старте вместе с ключами: неверный токен
	// или опечатка в адресе выяснятся сразу, а не через час после прогона,
	// когда библиотека молча не обновилась. Недоступность прогон не
	// отменяет — см. mediaserver.CheckAll.
	mediaserver.CheckAll(ctx, mediaserver.FromConfig(cfg, r.http), logger)

	// Рабочая папка чистится только ПОСЛЕ успешной проверки ключей: если
	// прогон не начался, недоупакованный остаток прошлого прогона (если тот
	// упал) тоже не трогаем — он может пригодиться для разбора.
	if cfg.BackupEnabled {
		if err := backup.ResetWorkDir(cfg.BackupDir); err != nil {
			logger.Event("[ERROR] cannot prepare backup work dir: %v", err)
			return err
		}
	}

	for _, root := range cfg.MoviesPaths {
		if err := ctx.Err(); err != nil {
			return r.finish(deps, err)
		}
		// Корень проверяется целиком до обхода: если раздел отмонтирован,
		// смонтирован read-only или у нас нет прав войти внутрь, лучше
		// узнать это одной строкой в логе, чем тысячей отказов по файлам.
		// Счётчик здесь именно Errors, а не NoAccess: это сломанная
		// настройка, которую пользователь должен починить, тогда как
		// NoAccess (см. walkForShows) — про отдельные участки медиатеки.
		if err := checkWritableDir(root); err != nil {
			deps.Stats.IncError()
			logger.Event("[ERROR] MOVIES_PATH %s: %v, skipped", root, err)
			continue
		}
		logger.Event("[SCAN] movies: %s", root)
		if err := processMoviesPath(ctx, deps, root); err != nil {
			deps.Stats.IncError()
			logger.Event("[ERROR] movies root %s: %v", root, err)
		}
	}

	for _, root := range cfg.TVShowsPaths {
		if err := ctx.Err(); err != nil {
			return r.finish(deps, err)
		}
		if err := checkWritableDir(root); err != nil {
			deps.Stats.IncError()
			logger.Event("[ERROR] TVSHOWS_PATH %s: %v, skipped", root, err)
			continue
		}
		logger.Event("[SCAN] tv shows: %s", root)
		if err := ProcessTVShowsPath(ctx, deps, root); err != nil {
			deps.Stats.IncError()
			logger.Event("[ERROR] tv shows root %s: %v", root, err)
		}
	}

	return r.finish(deps, nil)
}

// CheckKeys проверяет ключи и ничего больше — это режим --check-config.
// Блокировка здесь не берётся: прогон не начинается, файлы не трогаются,
// и запрещать проверку конфига во время работающего демона незачем.
func (r *Runner) CheckKeys(ctx context.Context) error {
	cfg, logger := r.current()
	deps := NewDeps(cfg, r.db, logger, r.http)
	if len(deps.Providers) == 0 {
		return fmt.Errorf("no rating providers configured")
	}
	return checkKeys(ctx, deps)
}

// CheckMediaServers опрашивает настроенные медиасерверы. Отдельно от
// CheckKeys: провайдеры рейтингов и медиасерверы — разные подсистемы
// с разной ценой отказа, и сваливать их в один метод значило бы соврать
// его именем.
func (r *Runner) CheckMediaServers(ctx context.Context) {
	cfg, logger := r.current()
	servers := mediaserver.FromConfig(cfg, r.http)
	if len(servers) == 0 {
		logger.Event("[MEDIASERVER] none enabled")
		return
	}
	mediaserver.CheckAll(ctx, servers, logger)
}

// finish — общий хвост прогона (в том числе прерванного по контексту):
// упаковка бэкапов, обновление библиотек и печать сводки. Бэкапы
// упаковываются даже при прерывании — файлы уже изменены, их оригиналы
// терять нельзя.
func (r *Runner) finish(deps *Deps, runErr error) error {
	finalizeBackups(deps)
	r.notifyMediaServers(deps)

	deps.Stats.Finish()
	deps.Logger.Event("\n%s", deps.Stats.Summary())

	if runErr != nil {
		deps.Logger.Event("[RUN_ABORTED] %v", runErr)
		return runErr
	}
	deps.Logger.Event("[RUN_END] finished")
	return nil
}

// notifyMediaServers просит сервер пересканировать библиотеку.
//
// Контекст здесь свой, не прогонный, и это намеренно. finish() вызывается
// и для прерванного прогона, когда исходный контекст уже отменён, — а файлы
// к этому моменту изменены, и сказать об этом серверу надо тем более.
// Срок короткий, так что остановку демона это не задержит.
//
// Ничего не делается, если за прогон не изменился ни один файл: сканирование
// библиотеки — не бесплатная операция, и запускать его еженедельно вхолостую
// незачем.
func (r *Runner) notifyMediaServers(deps *Deps) {
	servers := mediaserver.FromConfig(deps.Config, r.http)
	if len(servers) == 0 {
		return
	}
	if deps.Stats.UpdatedCount() == 0 {
		deps.Logger.Detail("[MEDIASERVER] no files changed, library refresh not requested")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), mediaServerTimeout)
	defer cancel()
	mediaserver.RefreshAll(ctx, servers, deps.Logger)
}

// checkKeys проверяет ВСЕ ключи ВСЕХ провайдеров до того, как будет тронут
// первый файл, и решает, начинать ли прогон.
//
// Заменила прежний preflight, который делал по одному настоящему запросу
// на провайдера и страдал сразу тремя болезнями: проверял только один ключ
// из пула, тратил квоту зря и сваливал в "unreachable" вообще любую ошибку,
// включая безобидный ErrNotFound.
//
// Логика решения:
//   - все ключи упали с сетевой ошибкой -> прогон отменяется, файлы целы;
//   - хоть один ключ рабочий -> идём, негодные уже выведены из оборота;
//   - рабочих ключей нет, но сеть отвечает (все невалидны или исчерпаны) ->
//     идём, но предупреждаем: прогон отработает на кэше. Это не бессмысленно:
//     файлы, до которых раньше не доходили руки, будут заполнены из БД.
func checkKeys(ctx context.Context, deps *Deps) error {
	checkers := make([]providers.KeyChecker, 0, len(deps.Providers)+1)
	seen := make(map[string]bool)
	for _, p := range deps.Providers {
		c, ok := p.(providers.KeyChecker)
		if !ok {
			continue
		}
		checkers = append(checkers, c)
		seen[c.Name()] = true
	}
	// TMDb проверяем даже при TMDB_RATING=no: он остаётся единственным
	// источником даты для <premiered>, и раньше его негодный ключ вообще
	// ничем себя не проявлял, кроме молчаливой строки в Detail-логе.
	if deps.TMDb != nil && !seen[deps.TMDb.Name()] {
		checkers = append(checkers, deps.TMDb)
	}

	statuses := providers.CheckAll(ctx, checkers)
	if len(statuses) == 0 {
		return nil
	}

	usableByProvider := make(map[string]int)
	checkedProviders := make(map[string]bool)
	// helpShown — справка о получении ключа печатается один раз на провайдера.
	// Пять протухших ключей OMDb в пуле не должны дать пять одинаковых
	// абзацев со ссылкой.
	helpShown := make(map[string]bool)
	networkFailures := 0

	for _, st := range statuses {
		checkedProviders[st.Provider] = true
		if st.OK {
			usableByProvider[st.Provider]++
			continue
		}
		if providers.IsNetworkError(st.Err) {
			networkFailures++
			deps.Logger.Event("[KEY_CHECK] %s key #%d: service did not respond: %v",
				st.Provider, st.KeyIndex+1, st.Err)
			continue
		}
		// Сюда попадают ошибки уровня ключа. Текст KeyError уже содержит
		// и провайдера, и номер ключа, и причину.
		deps.Logger.Event("[KEY_CHECK] %v", st.Err)

		// Подсказка, где взять новый ключ. Только для KeyInvalid: при
		// KeyExhausted ключ исправен и завтра снова заработает, посылать
		// человека получать новый — сбивать с толку. Пишется через Event,
		// то есть попадает и в файл лога: демон может крутиться месяцами,
		// и лог — единственное место, где владелец увидит, что ключ протух
		// и что с этим делать.
		if ke, ok := providers.AsKeyError(st.Err); ok && ke.Kind == providers.KeyInvalid && !helpShown[st.Provider] {
			helpShown[st.Provider] = true
			if help, found := providers.KeyHelpFor(st.Provider); found {
				deps.Logger.Event("[KEY_HELP] %s", help.Text())
			}
		}
	}

	if networkFailures == len(statuses) {
		err := fmt.Errorf("no provider responded during key check")
		deps.Logger.Event("[KEY_CHECK_FAILED] %v — aborting run, no files touched", err)
		return err
	}

	var usable, unusable []string
	for name := range checkedProviders {
		if usableByProvider[name] > 0 {
			usable = append(usable, fmt.Sprintf("%s (%d)", name, usableByProvider[name]))
			continue
		}
		unusable = append(unusable, name)
	}
	sort.Strings(usable)
	sort.Strings(unusable)

	if len(usable) == 0 {
		deps.Logger.Event("[KEY_CHECK_WARNING] no usable keys left (%s) — running on cached values only",
			strings.Join(unusable, ", "))
		return nil
	}

	deps.Logger.Event("[KEY_CHECK] usable keys: %s", strings.Join(usable, ", "))
	if len(unusable) > 0 {
		deps.Logger.Event("[KEY_CHECK_WARNING] no usable keys for: %s (continuing with the rest)",
			strings.Join(unusable, ", "))
	}
	return nil
}

// processMoviesPath обходит один корень MOVIES_PATH. В отличие от сериалов,
// здесь нет никакой обязательной структуры папок: любой .nfo под этим
// корнем считается файлом фильма (MOVIES_PATH и TVSHOWS_PATH обязаны быть
// раздельными — это требование конфига, а не рекомендация).
func processMoviesPath(ctx context.Context, deps *Deps, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			deps.Stats.IncError()
			deps.Logger.Event("[ERROR] walk %s: %v", path, err)
			return nil // не прерываем весь обход из-за одной нечитаемой папки
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if info.IsDir() || !isNFO(path) {
			return nil
		}
		// Ошибка одного файла уже посчитана и залогирована внутри
		// ProcessMovieFile — обход продолжается.
		_ = ProcessMovieFile(ctx, deps, path)
		return nil
	})
}

// finalizeBackups упаковывает рабочую папку в архивы — РАЗДЕЛЬНО по
// категориям, у каждой своя ротация по BACKUP_LIMIT. Если за прогон в
// категории ничего не менялось, Finalize вернёт пустой путь и архив не
// создаётся (пустые архивы каждую неделю никому не нужны).
func finalizeBackups(deps *Deps) {
	if !deps.Config.BackupEnabled {
		return
	}
	now := time.Now()
	for _, category := range []string{backup.CategoryMovies, backup.CategoryTVShows} {
		archivePath, err := backup.Finalize(deps.Config.BackupDir, category, deps.Config.BackupLimit, now)
		if err != nil {
			deps.Stats.IncError()
			deps.Logger.Event("[ERROR] finalize %s backup: %v", category, err)
			continue
		}
		if archivePath != "" {
			deps.Logger.Event("[BACKUP_ARCHIVED] %s: %s", category, archivePath)
		}
	}
}

func isNFO(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".nfo")
}
