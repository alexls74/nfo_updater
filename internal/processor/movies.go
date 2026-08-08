// internal/processor/movies.go
package processor

import (
	"context"
	"os"
	"time"

	"nfo_updater/internal/config"
	"nfo_updater/internal/db"
	"nfo_updater/internal/logging"
	"nfo_updater/internal/nfo"
	"nfo_updater/internal/providers"
)

// cacheTTL — сколько кэшированные значения считаются достаточно свежими,
// чтобы не ходить к провайдерам вовсе. Рейтинги меняются медленно, а вот
// суточная квота кончается быстро, поэтому сутки — разумный компромисс.
// Величина зафиксирована в коде намеренно: это не то, что пользователю
// стоит крутить, а ещё один параметр в конфиг ради этого не нужен.
const cacheTTL = 24 * time.Hour

// Deps — общие зависимости, нужные и обработке фильмов, и сериалов.
type Deps struct {
	Config    *config.Config
	DB        *db.DB
	Providers []providers.Provider // порядок важен: см. NewDeps — сначала MDBList (дешевле), потом остальные
	TMDb      *providers.TMDbProvider
	Breaker   *providers.CircuitBreaker
	Logger    *logging.Logger
	Stats     *RunStats

	quotaWarned map[string]bool
}

// fetchOutcome — ПОЧЕМУ рейтингов не оказалось. Раньше все причины
// сливались в одно сообщение "no rating found from any provider", и это
// дорого обошлось: неверный ID в файле выглядел точно так же, как отказ
// сети, и заставлял искать неисправность там, где её не было.
//
// Разница между исходами — не в оттенках формулировки, а в том, что
// пользователю с этим делать: чинить файл, ждать следующего прогона или
// не делать ничего вовсе.
type fetchOutcome int

const (
	fetchOK          fetchOutcome = iota // значения получены
	fetchUnavailable                     // провайдеры не ответили: сеть, квота, circuit breaker
	fetchIDUnknown                       // провайдеры ответили: тайтла с таким ID у них нет
	fetchNoRatings                       // тайтл есть, но рейтингов у него нет
	fetchNoProvider                      // ни один настроенный провайдер не умеет искать по этим ID
)

// describe возвращает текст для лога и признак, надо ли записывать тайтл
// в таблицу pending.
//
// При fetchUnavailable запись НЕ делается: провайдеры молчали по причинам,
// к файлу отношения не имеющим, и заносить тайтл в список безнадёжных
// значило бы записать временный сбой сети как свойство медиатеки.
func (o fetchOutcome) describe() (reason string, persist bool) {
	switch o {
	case fetchIDUnknown:
		return "no provider knows this id — it is most likely wrong in the file, check it against the title page on imdb.com", true
	case fetchNoRatings:
		return "the title is known to the providers but has no ratings yet — nothing to fix, this is normal for new releases and short films", true
	case fetchNoProvider:
		return "none of the configured providers can look up this combination of ids", true
	default:
		return "providers could not be reached during this run — nothing to fix in the file, it will be retried next run", false
	}
}

func enabledSources(cfg *config.Config) map[string]bool {
	return map[string]bool{
		"imdb":       cfg.IMDbRating,
		"tmdb":       cfg.TMDbRating,
		"trakt":      cfg.TraktRating,
		"tomatoes":   cfg.TomatoesRating,
		"popcorn":    cfg.PopcornRating,
		"metacritic": cfg.MetacriticRating,
	}
}

// idsLabel — короткое обозначение тайтла для сообщений об ошибках
// провайдеров. Пути файла на этом уровне нет (fetchRatingsWithFallback
// работает с ID, а не с файлами), а без опознавательного знака строка
// "mdblist: timeout" не даёт понять, на каком тайтле споткнулись.
func idsLabel(ids providers.IDs) string {
	switch {
	case ids.IMDb != "":
		return ids.IMDb
	case ids.TMDb != "":
		return "tmdb:" + ids.TMDb
	case ids.TVDb != "":
		return "tvdb:" + ids.TVDb
	default:
		return "unknown title"
	}
}

// ProcessMovieFile обрабатывает один .nfo фильма целиком.
func ProcessMovieFile(ctx context.Context, deps *Deps, path string) error {
	// Проверка прав ДО чтения и до любых сетевых запросов: файл, который
	// мы всё равно не сможем перезаписать, не стоит ни запроса к API,
	// ни места в статистике обработанных.
	if err := checkWritableFile(path); err != nil {
		deps.Stats.IncNoAccess()
		deps.Logger.Event("[NO_ACCESS] %s: %v, skipped", path, err)
		return nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		deps.Stats.IncError()
		deps.Logger.Event("[ERROR] read %s: %v", path, err)
		return err
	}
	content := string(raw)
	deps.Stats.IncMovie()

	imdbID, tmdbID := nfo.ParseIDs(content)
	ids := providers.IDs{IMDb: imdbID, TMDb: tmdbID}

	if ids.Empty() {
		deps.Stats.IncNoID()
		deps.Logger.Event("[NO_ID] %s: no imdb_id or tmdb_id found in file", path)
	}

	changed := false
	newContent := content

	if !ids.Empty() {
		combined, fresh, outcome := fetchRatingsWithFallback(ctx, deps, ids, providers.MediaTypeMovie)

		var ratingsChanged bool
		if len(combined) == 0 {
			reason, persist := outcome.describe()
			deps.Stats.IncPending()
			if persist {
				_ = deps.DB.MarkPending(imdbID, tmdbID, "", reason)
			}
			deps.Logger.Event("[PENDING] %s: %s", path, reason)
			newContent, ratingsChanged = nfo.EnsureEmptyUserRating(newContent)
		} else {
			_ = deps.DB.ClearPending(imdbID, tmdbID, "")

			fetched := make([]nfo.RatingEntry, 0, len(combined))
			for source, v := range combined {
				fetched = append(fetched, nfo.RatingEntry{Source: source, Value: v.Value, Votes: v.Votes})
			}

			// Слияние со старым блоком: рейтинги, которые в этом прогоне
			// получить не удалось, сохраняют прежние значения, а записи
			// чужих инструментов переносятся дословно. Порядок задаёт
			// MergeRatings — обход combined по map даёт случайный порядок
			// и заставил бы переписывать файл на каждом прогоне.
			entries := nfo.MergeRatings(fetched, nfo.ParseRatingsBlock(newContent), enabledSources(deps.Config))

			sel := nfo.ChooseDefaultRating(entries, deps.Config.DefaultRating)
			if sel.Overridden {
				deps.Stats.IncDefaultOverridden()
				deps.Logger.Event("[DEFAULT_RATING_OVERRIDE] %s: %s", path, sel.Reason)
			}
			nfo.SetDefaultRating(entries, sel.Source)

			var buildErr error
			newContent, ratingsChanged, buildErr = nfo.ApplyRatingEntries(newContent, entries)
			if buildErr != nil {
				deps.Stats.IncError()
				deps.Logger.Event("[ERROR] %s: build ratings block: %v", path, buildErr)
				return buildErr
			}

			for source, v := range fresh {
				_ = deps.DB.UpsertRating(imdbID, tmdbID, "", source, v.Value, v.Votes)
			}
		}
		changed = changed || ratingsChanged

		var idsChanged bool
		newContent, idsChanged = nfo.FixLegacyUniqueIDs(newContent, imdbID, tmdbID)
		changed = changed || idsChanged

		if deps.Config.TMDbAPIKey != "" && !nfo.HasPremiered(newContent) {
			date, err := deps.TMDb.FetchReleaseDate(ctx, ids, providers.MediaTypeMovie)
			if err == nil && date != "" {
				var premieredChanged bool
				newContent, premieredChanged = nfo.FixMissingPremiered(newContent, date)
				changed = changed || premieredChanged
			} else if !providers.IsNetworkError(err) {
				deps.Logger.Detail("[SKIP_PREMIERED] %s: %v", path, err)
			}
		}
	}

	// Порядок <credits>/<director> относительно <actor> — вопрос вида
	// карточки в Emby, а не корректности данных, поэтому решает отдельный
	// флаг, а не настройка медиасервера: см. nfo.FixEmbyActorOrder.
	//
	// Правка стоит ВНЕ ветки !ids.Empty(): она не требует ни ID, ни сети,
	// и файл без опознавательных знаков в порядке тегов нуждается ровно
	// так же, как любой другой.
	if deps.Config.CrewOrderFix {
		var crewChanged bool
		newContent, crewChanged = nfo.FixEmbyActorOrder(newContent)
		changed = changed || crewChanged
	}

	return finishFile(deps, path, raw, newContent, changed)
}

// cachedRatings загружает кэш по тайтлу и сообщает, свежий ли он целиком.
//
// Свежесть считается по МАКСИМАЛЬНОМУ updated_at среди всех источников
// тайтла, а не по минимальному: источники дописываются в разное время,
// и по минимуму запись почти никогда не выглядела бы свежей.
func cachedRatings(deps *Deps, ids providers.IDs) (map[string]db.Rating, bool) {
	cached, err := deps.DB.GetRatings(ids.IMDb, ids.TMDb, ids.TVDb)
	if err != nil || len(cached) == 0 {
		return nil, false
	}
	var newest time.Time
	for _, r := range cached {
		if r.UpdatedAt.After(newest) {
			newest = r.UpdatedAt
		}
	}
	return cached, time.Since(newest) < cacheTTL
}

// coversWanted сообщает, покрывает ли кэш ВСЕ включённые в конфиге источники.
//
// Покрытием считается и негативная пометка (Found == false): "мы уже
// спрашивали, этого рейтинга у провайдеров нет" — такой же законченный
// ответ, как и само значение. Без этого гейт не срабатывал бы никогда для
// тайтлов, у которых какого-то источника не существует в принципе (скажем,
// нет Metacritic у старого фильма), и мы ходили бы за ним в сеть вечно.
//
// Требование полного покрытия заодно защищает от ситуации "пользователь
// включил новый источник": частичного кэша достаточно для заполнения дыр,
// но не для того, чтобы пропустить поход к провайдерам.
func coversWanted(cached map[string]db.Rating, wanted map[string]bool) bool {
	for source, on := range wanted {
		if !on {
			continue
		}
		r, ok := cached[source]
		if !ok {
			return false
		}
		if r.Found && r.Value == "" {
			return false
		}
	}
	return true
}

// recordMissing ставит негативные пометки на источники, которых провайдеры
// не дали. Вызывается ТОЛЬКО после результативного опроса (см. conclusive
// в fetchRatingsWithFallback) и ДО добора значений из кэша — иначе значение
// из кэша выглядело бы как ответ провайдера.
//
// Реально существующее в кэше значение негативной пометкой не затирается:
// провайдер мог отдать неполный ответ, и терять из-за этого ранее
// полученный рейтинг незачем.
func recordMissing(deps *Deps, ids providers.IDs, wanted map[string]bool, combined providers.FetchResult, cached map[string]db.Rating) {
	for source, on := range wanted {
		if !on || combined[source].Value != "" {
			continue
		}
		if r, ok := cached[source]; ok && r.Found && r.Value != "" {
			continue
		}
		_ = deps.DB.UpsertMissingRating(ids.IMDb, ids.TMDb, ids.TVDb, source)
	}
}

// classifyFetch определяет исход опроса провайдеров.
//
// Порядок веток важен. Недоступность проверяется раньше всего: если хоть
// что-то помешало опросу, судить о существовании тайтла мы не вправе.
// fetchNoRatings идёт раньше fetchIDUnknown потому, что "один провайдер
// не знает тайтл, другой знает, но рейтингов не имеет" означает, что тайтл
// всё-таки существует, и посылать человека проверять ID было бы враньём.
func classifyFetch(conclusive, answered, sawNotFound, sawNoRatings, gotValues bool) fetchOutcome {
	switch {
	case gotValues:
		return fetchOK
	case !conclusive:
		return fetchUnavailable
	case sawNoRatings:
		return fetchNoRatings
	case sawNotFound:
		return fetchIDUnknown
	case !answered:
		return fetchNoProvider
	default:
		return fetchNoRatings
	}
}

// fetchRatingsWithFallback собирает значения для включённых в конфиге
// источников. Порядок действий:
//
//  1. Кэш-гейт: если в кэше есть ВСЕ нужные источники (значением или
//     негативной пометкой) и он свежее cacheTTL — возвращаем его и не делаем
//     ни одного сетевого запроса. Это главный механизм экономии квоты:
//     повторный прогон в тот же день практически бесплатен.
//  2. Иначе обходим провайдеров по порядку.
//  3. Если опрос получился результативным — помечаем ненайденное.
//  4. Оставшиеся дыры добираем из того же (уже загруженного) кэша,
//     независимо от его возраста: устаревшее значение лучше пустого.
//
// Возвращает ТРИ значения:
//   - combined — всё, что удалось собрать, для записи в .nfo;
//   - fresh — только пришедшее от провайдеров, для записи в кэш;
//   - outcome — почему combined пуст, если он пуст.
//
// Разделение combined и fresh принципиально: обратная запись кэшированного
// значения обновила бы updated_at, и запись выглядела бы вечно свежей для
// гейта из пункта 1.
func fetchRatingsWithFallback(ctx context.Context, deps *Deps, ids providers.IDs, mediaType string) (combined, fresh providers.FetchResult, outcome fetchOutcome) {
	wanted := enabledSources(deps.Config)
	combined = make(providers.FetchResult)
	fresh = make(providers.FetchResult)

	cached, cacheFresh := cachedRatings(deps, ids)

	if cacheFresh && coversWanted(cached, wanted) {
		for source, on := range wanted {
			if !on {
				continue
			}
			if r := cached[source]; r.Found && r.Value != "" {
				combined[source] = providers.RatingValue{Value: r.Value, Votes: r.Votes}
			}
		}
		deps.Stats.IncFromCache()
		deps.Logger.Detail("[CACHE_HIT] all requested sources are cached and fresh, skipping providers")
		if len(combined) == 0 {
			// Кэш покрыт целиком негативными пометками: мы уже спрашивали,
			// значений нет. Это знание о тайтле, а не о сбое.
			return combined, fresh, fetchNoRatings
		}
		return combined, fresh, fetchOK
	}

	remaining := func() bool {
		for source, on := range wanted {
			if on && combined[source].Value == "" {
				return true
			}
		}
		return false
	}

	// conclusive — опрос прошёл без помех: никого не отключил circuit
	// breaker, ни у кого не кончилась квота, никто не отвалился по сети.
	// answered — хотя бы один провайдер реально ответил. Негативные пометки
	// ставим только при обоих условиях: иначе временный сбой был бы записан
	// как "рейтинга не существует".
	conclusive := true
	answered := false
	sawNotFound := false
	sawNoRatings := false

	for _, p := range deps.Providers {
		if !remaining() {
			break
		}
		if !p.Supports(ids, mediaType) {
			continue
		}
		if !deps.Breaker.Allowed(p.Name()) {
			conclusive = false
			continue
		}

		res, err := p.FetchRatings(ctx, ids, mediaType)
		switch {
		case err == nil:
			answered = true
			deps.Breaker.RecordSuccess(p.Name())
			deps.Stats.IncProviderRequest(p.Name())
			for source, v := range res {
				if wanted[source] && combined[source].Value == "" {
					combined[source] = v
					fresh[source] = v
				}
			}
		case err == providers.ErrNotFound:
			// Сервис прямо сказал: тайтла с таким ID у него нет.
			answered = true
			sawNotFound = true
			deps.Breaker.RecordSuccess(p.Name())
			deps.Stats.IncProviderRequest(p.Name())
		case err == providers.ErrNoRatings:
			// Тайтл есть, оценок к нему нет. Ответ такой же полноценный,
			// как значение, — для breaker'а и счётчика это успех.
			answered = true
			sawNoRatings = true
			deps.Breaker.RecordSuccess(p.Name())
			deps.Stats.IncProviderRequest(p.Name())
		case err == providers.ErrQuotaExhausted:
			conclusive = false
			if !deps.quotaWarned[p.Name()] {
				deps.quotaWarned[p.Name()] = true
				deps.Logger.Event("[QUOTA_EXHAUSTED] %s: daily quota exhausted, falling back to cached values for the rest of this run", p.Name())
			}
		case err == providers.ErrUnsupportedID:
			// этот провайдер просто не годится для данной комбинации ID, не сбой
		case providers.IsNetworkError(err):
			conclusive = false
			// Текст ошибки пишем ВСЕГДА и на уровне Event. Раньше здесь
			// не было ни одной записи, и молчаливый переход на следующего
			// провайдера выглядел в логе как нормальная работа: узнать
			// о сбое можно было только по счётчику запросов в сводке,
			// а причину — вообще никак, она нигде не сохранялась.
			//
			// Утопить лог это не может: после пяти отказов подряд circuit
			// breaker гасит провайдера, Allowed() возвращает false, и до
			// конца прогона запросов к нему больше не делается.
			deps.Logger.Event("[PROVIDER_ERROR] %s (%s): %v", p.Name(), idsLabel(ids), err)
			tripped := deps.Breaker.RecordNetworkFailure(p.Name())
			if tripped {
				deps.Logger.Event("[PROVIDER_DOWN] %s: tripped after repeated network failures, disabled for the rest of this run", p.Name())
			}
			time.Sleep(deps.Breaker.Backoff())
		}
	}

	// Классифицируем ДО добора из кэша: исход описывает, что ответили
	// провайдеры, а не что в итоге удалось наскрести.
	outcome = classifyFetch(conclusive, answered, sawNotFound, sawNoRatings, len(combined) > 0)

	// Негативные пометки при неизвестном ID не ставим. Толку от них нет —
	// тайтла не существует, — а вреда много: на следующем прогоне кэш-гейт
	// счёл бы такой тайтл полностью покрытым, провайдеров бы не спросил,
	// и подсказка про неверный ID из лога исчезла бы навсегда.
	if outcome != fetchIDUnknown && conclusive && answered {
		recordMissing(deps, ids, wanted, combined, cached)
	}

	if remaining() {
		for source, r := range cached {
			if !r.Found || r.Value == "" {
				continue
			}
			if wanted[source] && combined[source].Value == "" {
				combined[source] = providers.RatingValue{Value: r.Value, Votes: r.Votes}
				deps.Logger.Detail("[FROM_CACHE] %s: %s (cached %s)", source, r.Value, r.UpdatedAt.Format(time.RFC3339))
			}
		}
	}

	// Кэш мог закрыть дыру уже после классификации.
	if len(combined) > 0 {
		outcome = fetchOK
	}
	return combined, fresh, outcome
}
