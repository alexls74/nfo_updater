// internal/config/config.go
package config

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"nfo_updater/internal/nfo"
	"nfo_updater/internal/scheduler"
)

type Config struct {
	// Система
	DatabasePath  string
	LogEnabled    bool
	LogVerbose    bool // включает Detail-уровень логов (см. internal/logging) — по умолчанию выключен
	LogDir        string
	LogLimit      int // сколько последних файлов лога хранить (0 = безлимитно)
	BackupEnabled bool
	BackupDir     string
	BackupLimit   int    // сколько последних архивов хранить на категорию (0 = безлимитно)
	Schedule      string // cron-выражение ("0 3 * * 1") или "" — см. DefaultDaemonSchedule

	// Circuit breaker
	CircuitBreakerFailures       int
	CircuitBreakerBackoffSeconds int

	// Какие рейтинги искать
	IMDbRating       bool
	TMDbRating       bool
	TraktRating      bool
	TomatoesRating   bool
	PopcornRating    bool
	MetacriticRating bool
	DefaultRating    string // всегда одно из nfo.KnownRatingSources()

	// Правки .nfo, не связанные с рейтингами.
	//
	// CrewOrderFix намеренно НЕ привязан к EmbyEnabled, хотя порядок тегов
	// требуется именно Emby. Секция медиасерверов необязательна: человек
	// может пользоваться Emby и не заполнять адрес с ключом вовсе — ему
	// хватает планового сканирования самого сервера.
	CrewOrderFix bool

	// Источники. Пути уже нормализованы (абсолютные, Clean, без дублей) —
	// это делает Load(); остальной код может сравнивать их как строки.
	MoviesPaths  []string
	TVShowsPaths []string

	// API
	OMDbAPIKeys             []string
	OMDbDailyLimitPerKey    int
	MDBListAPIKeys          []string
	MDBListDailyLimitPerKey int
	TMDbAPIKey              string

	// Медиасерверы
	EmbyEnabled     bool
	EmbyURL         string
	EmbyAPIKey      string
	JellyfinEnabled bool
	JellyfinURL     string
	JellyfinAPIKey  string
	PlexEnabled     bool
	PlexURL         string
	PlexToken       string
	PlexSectionIDs  []string
}

// DefaultDaemonSchedule — применяется в main.go, когда запущено в режиме
// демона (-d), а SCHEDULE в конфиге пуст.
const DefaultDaemonSchedule = "0 3 * * 1"

// appName — имя подкаталога во всех путях по умолчанию. Совпадает с именем
// бинарника и с именем systemd-юнита.
const appName = "nfo_updater"

// ErrMissingAPIKeys — конфиг не заполнен ключами. Отдельный нужен,
// чтобы main.go мог отличить именно этот случай и дописать к сообщению
// справку о том, где ключи берут: пользователь, впервые открывший конфиг,
// иначе получает только список пустых параметров и никакой подсказки.
var ErrMissingAPIKeys = errors.New("all rating providers must be configured")

// ----------------------------------------------------------------------------
// Пути по умолчанию
//
// Всё живёт в домашнем каталоге того пользователя, от имени которого запущена
// программа, а не в /var и /etc. Причина: NFO Updater работает от обычной
// учётной записи — той, у которой есть права на запись в медиатеку.
//
// База, бэкапы и логи сведены в ОДНО дерево.
// ----------------------------------------------------------------------------
//
// homeDir возвращает домашний каталог текущего пользователя или "".
//
// os.UserHomeDir() смотрит только на $HOME.
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return ""
}

// DefaultConfigPath — путь к конфигу по умолчанию:
// ~/.config/nfo_updater/config.conf
//
// Возвращает "" если домашний каталог определить не удалось.
func DefaultConfigPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, appName, "config.conf")
	}
	if h := homeDir(); h != "" {
		return filepath.Join(h, ".config", appName, "config.conf")
	}
	return ""
}

// DefaultDataDir — корень для базы, бэкапов и логов:
// ~/.local/share/nfo_updater
//
// Экспортирована ради пакета lock: файл блокировки должен лежать там же,
// и вычисляться он обязан ровно так же, иначе ручной запуск и сервис возьмут
// разные пути и перестанут видеть друг друга.
func DefaultDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, appName)
	}
	if h := homeDir(); h != "" {
		return filepath.Join(h, ".local", "share", appName)
	}
	return ""
}

func defaults() map[string]string {
	// Раскладка внутри каталога данных описана в DataPathsUnder (exported.go):
	// те же имена нужны мастеру настройки, когда человек назначает свой корень.
	dbPath, logDir, backupDir := DataPathsUnder(DefaultDataDir())

	return map[string]string{
		"DATABASE_PATH":                   dbPath,
		"LOG_DIR":                         logDir,
		"LOG_LIMIT":                       "10",
		"BACKUP_DIR":                      backupDir,
		"BACKUP_LIMIT":                    "10",
		"CIRCUIT_BREAKER_FAILURES":        "5",
		"CIRCUIT_BREAKER_BACKOFF_SECONDS": "5",
		"OMDB_DAILY_LIMIT_PER_KEY":        "1000",
		"MDBLIST_DAILY_LIMIT_PER_KEY":     "1000",
		"DEFAULT_RATING":                  "imdb",
	}
}

func parseBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "1", "on":
		return true
	case "false", "no", "0", "off":
		return false
	default:
		return def
	}
}

// Load читает файл конфига, нормализует пути и валидирует результат.
// Конфиг возвращается даже при ошибке валидации — вызывающий код может
// показать пользователю то, что реально было прочитано.
func Load(path string) (*Config, error) {
	raw, err := readEnvFile(path)
	if err != nil {
		return nil, err
	}
	cfg := fromRaw(raw)
	if err := cfg.normalizePaths(); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

// Defaults — конфиг, каким он получится из пустого файла: только системные
// пути и умолчания, без медиатеки и ключей.
//
// Нужен флагу -v, когда конфига ещё нет на диске. Заводить файл ради показа
// версии нельзя (-v не должен ничего менять), а показать, куда программа
// будет складывать базу, логи и бэкапы после установки, полезно.
//
// Валидация здесь не выполняется намеренно.
func Defaults() *Config {
	return fromRaw(nil)
}

// fromRaw раскладывает прочитанные пары ключ-значение по полям Config,
// подставляя умолчания. Работает и с nil-картой — см. Defaults().
func fromRaw(raw map[string]string) *Config {
	d := defaults()
	get := func(key string) string {
		if v, ok := raw[key]; ok && v != "" {
			return v
		}
		return d[key]
	}
	getBool := func(key string, def bool) bool {
		return parseBool(raw[key], def)
	}
	getInt := func(key string) int {
		v := get(key)
		n, _ := strconv.Atoi(v)
		return n
	}
	splitList := func(key string) []string {
		v := raw[key]
		if v == "" {
			return nil
		}
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}

	return &Config{
		DatabasePath:  get("DATABASE_PATH"),
		LogEnabled:    getBool("LOG_ENABLED", true),
		LogVerbose:    getBool("LOG_VERBOSE", false),
		LogDir:        get("LOG_DIR"),
		LogLimit:      getInt("LOG_LIMIT"),
		BackupEnabled: getBool("BACKUP_ENABLED", true),
		BackupDir:     get("BACKUP_DIR"),
		BackupLimit:   getInt("BACKUP_LIMIT"),
		Schedule:      strings.TrimSpace(raw["SCHEDULE"]),

		CircuitBreakerFailures:       getInt("CIRCUIT_BREAKER_FAILURES"),
		CircuitBreakerBackoffSeconds: getInt("CIRCUIT_BREAKER_BACKOFF_SECONDS"),

		IMDbRating:       getBool("IMDB_RATING", true),
		TMDbRating:       getBool("TMDB_RATING", false),
		TraktRating:      getBool("TRAKT_RATING", false),
		TomatoesRating:   getBool("TOMATOES_RATING", true),
		PopcornRating:    getBool("POPCORN_RATING", false),
		MetacriticRating: getBool("METACRITIC_RATING", false),
		DefaultRating:    strings.ToLower(get("DEFAULT_RATING")),

		// Умолчание true, и пустое значение даёт то же самое: parseBool
		// возвращает def на любой нераспознанной строке, включая пустую.
		// Это соответствует поведению остальных булевых параметров и
		// заявленному в config.conf правилу "empty = yes".
		CrewOrderFix: getBool("CREW_ORDER_FIX", true),

		MoviesPaths:  splitList("MOVIES_PATH"),
		TVShowsPaths: splitList("TVSHOWS_PATH"),

		OMDbAPIKeys:             splitList("OMDB_API_KEYS"),
		OMDbDailyLimitPerKey:    getInt("OMDB_DAILY_LIMIT_PER_KEY"),
		MDBListAPIKeys:          splitList("MDBLIST_API_KEYS"),
		MDBListDailyLimitPerKey: getInt("MDBLIST_DAILY_LIMIT_PER_KEY"),
		TMDbAPIKey:              raw["TMDB_API_KEY"],

		EmbyEnabled:     getBool("EMBY_ENABLED", false),
		EmbyURL:         raw["EMBY_URL"],
		EmbyAPIKey:      raw["EMBY_API_KEY"],
		JellyfinEnabled: getBool("JELLYFIN_ENABLED", false),
		JellyfinURL:     raw["JELLYFIN_URL"],
		JellyfinAPIKey:  raw["JELLYFIN_API_KEY"],
		PlexEnabled:     getBool("PLEX_ENABLED", false),
		PlexURL:         raw["PLEX_URL"],
		PlexToken:       raw["PLEX_TOKEN"],
		PlexSectionIDs:  splitList("PLEX_SECTION_IDS"),
	}
}

// EnabledRatings — какие источники рейтингов включены пользователем.
// Ключи совпадают с nfo.KnownRatingSources().
func (c *Config) EnabledRatings() map[string]bool {
	return map[string]bool{
		"imdb":       c.IMDbRating,
		"tmdb":       c.TMDbRating,
		"trakt":      c.TraktRating,
		"tomatoes":   c.TomatoesRating,
		"popcorn":    c.PopcornRating,
		"metacritic": c.MetacriticRating,
	}
}

func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		out[key] = val
	}
	return out, sc.Err()
}

// normalizePaths приводит MOVIES_PATH/TVSHOWS_PATH к каноничному виду.
// Делается ОДИН раз при загрузке, чтобы дальше по коду (backupCategory,
// проверка пересечений) можно было сравнивать пути как обычные строки,
// не думая про "/media/movies" против "/media/movies/".
func (c *Config) normalizePaths() error {
	var err error
	if c.MoviesPaths, err = normalizePathList("MOVIES_PATH", c.MoviesPaths); err != nil {
		return err
	}
	if c.TVShowsPaths, err = normalizePathList("TVSHOWS_PATH", c.TVShowsPaths); err != nil {
		return err
	}
	return nil
}

// normalizePathList требует абсолютных путей и убирает точные дубли.
//
// Относительные пути осознанно запрещены, а не резолвятся через Abs():
// рабочий каталог у systemd-юнита и у ручного запуска из шелла разный,
// так что один и тот же конфиг вёл бы себя по-разному — это ловушка,
// которую лучше поймать на старте явным сообщением.
//
// Точный дубль пути — очевидная опечатка (обойти одну и ту же папку дважды
// никому не нужно), поэтому он молча схлопывается, а не роняет запуск.
func normalizePathList(setting string, paths []string) ([]string, error) {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return nil, pathError(setting, p)
		}
		clean := filepath.Clean(p)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out, nil
}

// pathError — общее сообщение о неабсолютном пути, с отдельной веткой на
// тильду.
func pathError(setting, path string) error {
	if strings.HasPrefix(path, "~") {
		return fmt.Errorf("%s: %q starts with ~, which is not expanded here — write the path out in full", setting, path)
	}
	return fmt.Errorf("%s: %q is not an absolute path", setting, path)
}

// checkSystemPath проверяет один из служебных путей (база, логи, бэкапы).
//
// Пустое значение здесь означает не "выключено", а "умолчание не удалось
// вычислить": домашний каталог неизвестен. Сообщение должно вести к решению,
// а не констатировать пустоту.
func checkSystemPath(setting, path string) error {
	p := strings.TrimSpace(path)
	if p == "" {
		return fmt.Errorf("%s is empty and the default could not be determined "+
			"(the home directory is unknown): set %s to an absolute path", setting, setting)
	}
	if !filepath.IsAbs(p) {
		return pathError(setting, p)
	}
	return nil
}

// pathRef — путь вместе с именем параметра, из которого он пришёл, чтобы
// в сообщении об ошибке было видно, какие именно строки конфига конфликтуют.
type pathRef struct {
	setting string
	path    string
}

// checkPathOverlaps запрещает совпадающие и вложенные друг в друга корни.
//
// Причина жёсткости: категория файла и его путь внутри архива бэкапа
// определяются тем, под каким корнем файл найден (см. processor/paths.go).
// Если корни пересекаются, файл сериала уедет в архив фильмов, а часть
// библиотеки будет обработана дважды.
func checkPathOverlaps(refs []pathRef) error {
	for i := 0; i < len(refs); i++ {
		for j := i + 1; j < len(refs); j++ {
			a, b := refs[i], refs[j]

			if a.path == b.path {
				if a.setting == b.setting {
					continue // дубли внутри одного параметра уже отсеяны нормализацией
				}
				return fmt.Errorf(
					"%s and %s point to the same directory %s: movies and TV shows must be stored in separate trees",
					a.setting, b.setting, a.path)
			}

			parent, child := a, b
			if isInside(a.path, b.path) {
				parent, child = b, a
			} else if !isInside(b.path, a.path) {
				continue
			}

			if parent.setting == child.setting {
				return fmt.Errorf(
					"%s: %s is nested inside %s, each path must be a separate tree",
					parent.setting, child.path, parent.path)
			}
			return fmt.Errorf(
				"%s path %s is nested inside %s path %s: movies and TV shows must be stored in separate trees",
				child.setting, child.path, parent.setting, parent.path)
		}
	}
	return nil
}

// isInside сообщает, лежит ли child строго внутри parent.
func isInside(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// mediaServerRef — один медиасервер для проверки. Собирается в срез, чтобы
// три одинаковых блока проверок не были написаны трижды.
type mediaServerRef struct {
	enabled       bool
	enabledOption string
	urlSetting    string
	url           string
	keySetting    string
	key           string
}

// validateMediaServers проверяет только то, что видно без обращения к сети:
// заполненность и синтаксис. Доступность сервера и годность ключа проверяются
// уже по сети (см. mediaserver.CheckAll) и фатальными НЕ считаются: выключенный
// на ночь сервер — не повод не обновлять рейтинги. А вот включённый сервер
// с пустым адресом — это опечатка в конфиге, и молчать о ней нельзя.
func (c *Config) validateMediaServers() error {
	servers := []mediaServerRef{
		{c.EmbyEnabled, "EMBY_ENABLED", "EMBY_URL", c.EmbyURL, "EMBY_API_KEY", c.EmbyAPIKey},
		{c.JellyfinEnabled, "JELLYFIN_ENABLED", "JELLYFIN_URL", c.JellyfinURL, "JELLYFIN_API_KEY", c.JellyfinAPIKey},
		{c.PlexEnabled, "PLEX_ENABLED", "PLEX_URL", c.PlexURL, "PLEX_TOKEN", c.PlexToken},
	}

	for _, s := range servers {
		if !s.enabled {
			continue
		}
		if strings.TrimSpace(s.url) == "" {
			return fmt.Errorf("%s is set but %s is empty", s.enabledOption, s.urlSetting)
		}
		if err := checkServerURL(s.urlSetting, s.url); err != nil {
			return err
		}
		if strings.TrimSpace(s.key) == "" {
			return fmt.Errorf("%s is set but %s is empty", s.enabledOption, s.keySetting)
		}
	}

	// Секции Plex — числа, и только они попадают в URL сканирования.
	// Название библиотеки вместо номера — самая вероятная ошибка здесь.
	if c.PlexEnabled {
		if err := CheckPlexSectionIDs(c.PlexSectionIDs); err != nil {
			return err
		}
	}
	return nil
}

// sectionIDError — общее сообщение о непохожей на номер секции Plex.
// Вынесено отдельно, потому что тот же текст показывает мастер настройки
// через CheckPlexSectionIDs (см. exported.go).
func sectionIDError(id string) error {
	return fmt.Errorf("PLEX_SECTION_IDS: %q is not a section number "+
		"(the section id is the number shown in the Plex web address, not the library name)", id)
}

// checkServerURL требует адрес вида http://host[:port][/path].
//
// Самая частая ошибка — адрес без схемы ("192.168.1.10:8096"): net/url такой
// разберёт, но host окажется пустым, и запрос уйдёт в никуда с невнятной
// ошибкой уже во время прогона.
func checkServerURL(setting, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%s: %q is not a valid address: %w", setting, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: %q must start with http:// or https://", setting, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: %q has no host part", setting, raw)
	}
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// validateRatings проверяет набор включённых рейтингов и выбор главного.
//
// Проверка "DEFAULT_RATING включён" нужна потому, что иначе несоответствие
// проявляется только в логе: ChooseDefaultRating молча уйдёт на следующий
// доступный источник и напишет [DEFAULT_RATING_OVERRIDE] для КАЖДОГО файла
// библиотеки. Формально это рабочее поведение, но пользователь получит
// тысячи строк вместо одной внятной ошибки при старте.
func (c *Config) validateRatings() error {
	valid := nfo.KnownRatingSources()
	known := false
	for _, v := range valid {
		if c.DefaultRating == v {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("invalid DEFAULT_RATING %q: must be one of %s", c.DefaultRating, strings.Join(valid, ", "))
	}

	enabled := c.EnabledRatings()

	// Отдельным сообщением: при пустом наборе жалоба на DEFAULT_RATING
	// увела бы пользователя не туда.
	any := false
	for _, on := range enabled {
		if on {
			any = true
			break
		}
	}
	if !any {
		return fmt.Errorf("no rating providers are enabled: set at least one of %s to yes",
			strings.Join(upperRatingOptions(valid), ", "))
	}

	if !enabled[c.DefaultRating] {
		return fmt.Errorf("DEFAULT_RATING is %q, but that provider is disabled: "+
			"either enable it or pick one of the enabled providers", c.DefaultRating)
	}
	return nil
}

// upperRatingOptions превращает ключи источников в имена параметров конфига,
// чтобы в сообщении об ошибке стояло IMDB_RATING, а не imdb.
func upperRatingOptions(sources []string) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, strings.ToUpper(s)+"_RATING")
	}
	return out
}

// Validate проверяет конфиг целиком. Вызывается из Load(), из --check-config
// и при SIGHUP-reload. Ожидает уже нормализованные пути (см. normalizePaths).
//
// CREW_ORDER_FIX здесь не проверяется, и проверять нечего: это булев
// параметр, у которого любое нераспознанное значение означает умолчание.
func (c *Config) Validate() error {
	// Служебные пути идут первыми: без базы не запустится ничего, и жаловаться
	// на медиатеку раньше, чем на неё, было бы не по порядку важности.
	// Логи и бэкапы проверяются только когда включены: выключенной подсистеме
	// путь не нужен, и пустой LOG_DIR при LOG_ENABLED=no — не ошибка.
	if err := checkSystemPath("DATABASE_PATH", c.DatabasePath); err != nil {
		return err
	}
	if c.LogEnabled {
		if err := checkSystemPath("LOG_DIR", c.LogDir); err != nil {
			return err
		}
	}
	if c.BackupEnabled {
		if err := checkSystemPath("BACKUP_DIR", c.BackupDir); err != nil {
			return err
		}
	}

	if len(c.MoviesPaths) == 0 && len(c.TVShowsPaths) == 0 {
		return fmt.Errorf("at least one of MOVIES_PATH or TVSHOWS_PATH must be set")
	}

	// Через CheckMediaPaths, а не сборкой pathRef на месте: ту же проверку
	// теми же словами делает мастер настройки после каждого введённого пути.
	if err := CheckMediaPaths(c.MoviesPaths, c.TVShowsPaths); err != nil {
		return err
	}

	// Требуем все три набора ключей сразу.
	var missing []string
	if len(c.OMDbAPIKeys) == 0 {
		missing = append(missing, "OMDB_API_KEYS")
	}
	if len(c.MDBListAPIKeys) == 0 {
		missing = append(missing, "MDBLIST_API_KEYS")
	}
	if c.TMDbAPIKey == "" {
		missing = append(missing, "TMDB_API_KEY")
	}
	if len(missing) > 0 {
		// %w, а не %v: main.go ловит этот случай через errors.Is и дописывает
		// справку о получении ключей.
		return fmt.Errorf("%w, missing: %s", ErrMissingAPIKeys, strings.Join(missing, ", "))
	}

	if err := c.validateRatings(); err != nil {
		return err
	}

	if err := c.validateMediaServers(); err != nil {
		return err
	}

	if c.Schedule != "" {
		if _, err := scheduler.ParseCron(c.Schedule); err != nil {
			return fmt.Errorf("invalid SCHEDULE: %w", err)
		}
	}

	if c.LogLimit < 0 {
		return fmt.Errorf("LOG_LIMIT must be 0 or greater (0 = unlimited)")
	}
	if c.BackupLimit < 0 {
		return fmt.Errorf("BACKUP_LIMIT must be 0 or greater (0 = unlimited)")
	}
	if c.OMDbDailyLimitPerKey < 1 {
		return fmt.Errorf("OMDB_DAILY_LIMIT_PER_KEY must be at least 1")
	}
	if c.MDBListDailyLimitPerKey < 1 {
		return fmt.Errorf("MDBLIST_DAILY_LIMIT_PER_KEY must be at least 1")
	}
	if c.CircuitBreakerFailures < 1 {
		return fmt.Errorf("CIRCUIT_BREAKER_FAILURES must be at least 1")
	}
	if c.CircuitBreakerBackoffSeconds < 0 {
		return fmt.Errorf("CIRCUIT_BREAKER_BACKOFF_SECONDS must be 0 or greater")
	}

	return nil
}
