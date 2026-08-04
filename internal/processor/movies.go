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
		combined, fresh := fetchRatingsWithFallback(ctx, deps, ids, providers.MediaTypeMovie)

		var ratingsChanged bool
		if len(combined) == 0 {
			deps.Stats.IncPending()
			_ = deps.DB.MarkPending(imdbID, tmdbID, "", "no provider returned a value and no cached value available")
			deps.Logger.Event("[PENDING] %s: no rating found from any provider, no cached value either", path)
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

	if deps.Config.EmbyEnabled {
		var embyChanged bool
		newContent, embyChanged = nfo.FixEmbyActorOrder(newContent)
		changed = changed || embyChanged
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
// Возвращает ДВА набора:
//   - combined — всё, что удалось собрать, для записи в .nfo;
//   - fresh — только пришедшее от провайдеров, для записи в кэш.
//
// Разделение принципиально: обратная запись кэшированного значения обновила
// бы updated_at, и запись выглядела бы вечно свежей для гейта из пункта 1.
func fetchRatingsWithFallback(ctx context.Context, deps *Deps, ids providers.IDs, mediaType string) (combined, fresh providers.FetchResult) {
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
		return combined, fresh
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
			answered = true
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
			tripped := deps.Breaker.RecordNetworkFailure(p.Name())
			if tripped {
				deps.Logger.Event("[PROVIDER_DOWN] %s: tripped after repeated network failures, disabled for the rest of this run", p.Name())
			}
			time.Sleep(deps.Breaker.Backoff())
		}
	}

	if conclusive && answered {
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
	return combined, fresh
}
