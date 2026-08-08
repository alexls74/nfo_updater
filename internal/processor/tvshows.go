// internal/processor/tvshows.go
package processor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nfo_updater/internal/backup"
	"nfo_updater/internal/nfo"
	"nfo_updater/internal/providers"
)

// ProcessTVShowsPath обходит один корень TVSHOWS_PATH рекурсивно, находя
// папки сериалов по стандартному Kodi-маркеру — файлу tvshow.nfo (имя
// файла, официальное требование Kodi к структуре сериала). Папки сериалов
// могут быть на любой глубине вложенности внутри root.
func ProcessTVShowsPath(ctx context.Context, deps *Deps, root string) error {
	return walkForShows(ctx, deps, root)
}

// walkForShows рекурсивно ищет папки, содержащие tvshow.nfo. Как только
// такая папка найдена — она обрабатывается целиком (сериал + все серии
// внутри неё), и дальше эта ветка дерева не сканируется повторно.
func walkForShows(ctx context.Context, deps *Deps, dir string) error {
	if err := checkWritableDir(dir); err != nil {
		deps.Stats.IncNoAccess()
		deps.Logger.Event("[NO_ACCESS] %s: %v, subtree skipped", dir, err)
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dir, err)
	}

	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(e.Name(), "tvshow.nfo") {
			return processShowDir(ctx, deps, dir, filepath.Join(dir, e.Name()))
		}
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := walkForShows(ctx, deps, filepath.Join(dir, e.Name())); err != nil {
			deps.Stats.IncError()
			deps.Logger.Event("[ERROR] %s: %v", filepath.Join(dir, e.Name()), err)
		}
	}
	return nil
}

// processShowDir обрабатывает найденную папку сериала: сначала сам
// tvshow.nfo, затем рекурсивно все файлы серий внутри этой же папки.
func processShowDir(ctx context.Context, deps *Deps, showDir, showNFOPath string) error {
	ids, episodeRatings, err := processShowNFO(ctx, deps, showNFOPath)
	if err != nil {
		return fmt.Errorf("process tvshow.nfo: %w", err)
	}

	return filepath.Walk(showDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || path == showNFOPath {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			deps.Stats.IncError()
			deps.Logger.Event("[ERROR] %s: %v", path, err)
			return nil
		}
		if nfo.DetectFileType(string(raw)) != nfo.FileTypeEpisode {
			return nil
		}
		if err := processEpisodeFile(ctx, deps, path, raw, ids, episodeRatings); err != nil {
			deps.Stats.IncError()
			deps.Logger.Event("[ERROR] %s: %v", path, err)
		}
		return nil
	})
}

// processShowNFO обрабатывает tvshow.nfo — резолвинг ID, рейтинг сериала
// целиком, фиксы, бэкап+запись. Возвращает ids и таблицу рейтингов серий
// (если MDBList их предоставил).
//
// Отсутствие прав на сам tvshow.nfo не отменяет обработку серий: ids
// и таблица рейтингов серий возвращаются как обычно, поэтому серии внутри
// этой папки будут обработаны, если их файлы записываемы.
func processShowNFO(ctx context.Context, deps *Deps, path string) (providers.IDs, map[string]providers.EpisodeRating, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		deps.Stats.IncError()
		deps.Logger.Event("[ERROR] read %s: %v", path, err)
		return providers.IDs{}, nil, err
	}
	content := string(raw)
	deps.Stats.IncTVShow()

	imdbID, tmdbID, tvdbID := nfo.ParseTVShowIDs(content)
	ids := providers.IDs{IMDb: imdbID, TMDb: tmdbID, TVDb: tvdbID}

	if ids.Empty() {
		deps.Stats.IncNoID()
		deps.Logger.Event("[NO_ID] %s: no imdb_id, tmdb_id or tvdb_id found in file", path)
		return ids, nil, nil
	}

	writable := checkWritableFile(path)
	if writable != nil {
		deps.Stats.IncNoAccess()
		deps.Logger.Event("[NO_ACCESS] %s: %v, show file skipped (episodes will still be processed)", path, writable)
	}

	showRatings, freshRatings, episodeRatings, outcome := fetchShowRatings(ctx, deps, ids)

	// Запрос к сериалу нужен в любом случае — из него берутся рейтинги
	// серий, — но правку и запись самого tvshow.nfo при отсутствии прав
	// пропускаем.
	if writable != nil {
		return ids, episodeRatings, nil
	}

	newContent := content
	var changed bool

	if len(showRatings) == 0 {
		reason, persist := outcome.describe()
		deps.Stats.IncPending()
		if persist {
			_ = deps.DB.MarkPending(imdbID, tmdbID, tvdbID, reason)
		}
		deps.Logger.Event("[PENDING] %s: %s", path, reason)
		var c bool
		newContent, c = nfo.EnsureEmptyUserRating(newContent)
		changed = changed || c
	} else {
		_ = deps.DB.ClearPending(imdbID, tmdbID, tvdbID)

		fetched := make([]nfo.RatingEntry, 0, len(showRatings))
		for source, v := range showRatings {
			fetched = append(fetched, nfo.RatingEntry{Source: source, Value: v.Value, Votes: v.Votes})
		}

		// См. комментарий в movies.go: слияние сохраняет недополученные
		// и чужие записи и задаёт стабильный порядок блока.
		entries := nfo.MergeRatings(fetched, nfo.ParseRatingsBlock(newContent), enabledSources(deps.Config))

		sel := nfo.ChooseDefaultRating(entries, deps.Config.DefaultRating)
		if sel.Overridden {
			deps.Stats.IncDefaultOverridden()
			deps.Logger.Event("[DEFAULT_RATING_OVERRIDE] %s: %s", path, sel.Reason)
		}
		nfo.SetDefaultRating(entries, sel.Source)

		var c bool
		var buildErr error
		newContent, c, buildErr = nfo.ApplyRatingEntries(newContent, entries)
		if buildErr != nil {
			deps.Stats.IncError()
			deps.Logger.Event("[ERROR] %s: build ratings block: %v", path, buildErr)
			return ids, episodeRatings, buildErr
		}
		changed = changed || c

		// В кэш идёт только то, что пришло от провайдеров: перезапись
		// кэшированного значения самим же собой обновила бы updated_at
		// и сделала бы его вечно "свежим" для кэш-гейта.
		for source, v := range freshRatings {
			_ = deps.DB.UpsertRating(imdbID, tmdbID, tvdbID, source, v.Value, v.Votes)
		}
	}

	var idsChanged bool
	newContent, idsChanged = nfo.FixLegacyUniqueIDs(newContent, imdbID, tmdbID)
	changed = changed || idsChanged

	if deps.Config.TMDbAPIKey != "" && !nfo.HasPremiered(newContent) {
		date, err := deps.TMDb.FetchReleaseDate(ctx, ids, providers.MediaTypeShow)
		if err == nil && date != "" {
			var c bool
			newContent, c = nfo.FixMissingPremiered(newContent, date)
			changed = changed || c
		} else if !providers.IsNetworkError(err) {
			deps.Logger.Detail("[SKIP_PREMIERED] %s: %v", path, err)
		}
	}

	// Порядок съёмочной группы относительно актёров — под своим флагом,
	// см. nfo.FixEmbyActorOrder и CREW_ORDER_FIX в config.conf.
	if deps.Config.CrewOrderFix {
		var c bool
		newContent, c = nfo.FixEmbyActorOrder(newContent)
		changed = changed || c
	}

	if err := finishFile(deps, path, raw, newContent, changed); err != nil {
		return ids, episodeRatings, err
	}
	return ids, episodeRatings, nil
}

// processEpisodeFile обрабатывает один файл серии. Если для её season:episode
// есть значение в episodeRatings (бесплатно получено из ответа по сериалу
// целиком через MDBList) — используем его без сетевого запроса. Иначе,
// если у серии есть собственный imdb id — запрашиваем её отдельно.
// Рейтинг серии — всегда только imdb, в шкале Х.Х.
func processEpisodeFile(ctx context.Context, deps *Deps, path string, raw []byte, showIDs providers.IDs, episodeRatings map[string]providers.EpisodeRating) error {
	if err := checkWritableFile(path); err != nil {
		deps.Stats.IncNoAccess()
		deps.Logger.Event("[NO_ACCESS] %s: %v, skipped", path, err)
		return nil
	}

	content := string(raw)
	deps.Stats.IncEpisode()

	episodeIMDbID, season, episode, ok := nfo.ParseEpisodeInfo(content)
	if !ok {
		deps.Stats.IncNoID()
		deps.Logger.Event("[NO_ID] %s: missing <season>/<episode>, cannot match episode-level rating", path)
		return nil
	}

	if !deps.Config.IMDbRating {
		deps.Stats.IncSkipped()
		deps.Logger.Detail("[SKIP] %s: IMDb rating is the only source at episode level and IMDB_RATING is disabled", path)
		return nil
	}

	var value string
	var votes int
	var found bool

	// Исход по умолчанию — на случай, когда серии нет ни в таблице от
	// MDBList, ни собственного imdb id у неё нет: спрашивать оказалось
	// некого, и это не сбой провайдеров.
	outcome := fetchNoProvider

	key := fmt.Sprintf("%d:%d", season, episode)
	if r, ok := episodeRatings[key]; ok {
		value, votes, found = r.Value, r.Votes, true
		// Значение пришло из ответа по сериалу, то есть от провайдера —
		// кэшируем его наравне с результатом прямого запроса.
		if episodeIMDbID != "" {
			_ = deps.DB.UpsertRating(episodeIMDbID, "", "", "imdb", value, votes)
		}
	} else if episodeIMDbID != "" {
		ids := providers.IDs{IMDb: episodeIMDbID}
		res, fresh, o := fetchRatingsWithFallback(ctx, deps, ids, providers.MediaTypeEpisode)
		outcome = o
		if r, ok := res["imdb"]; ok {
			value, votes, found = r.Value, r.Votes, true
		}
		if r, ok := fresh["imdb"]; ok {
			_ = deps.DB.UpsertRating(episodeIMDbID, "", "", "imdb", r.Value, r.Votes)
		}
	}

	newContent := content
	var changed bool

	if !found {
		reason, persist := outcome.describe()
		deps.Stats.IncPending()
		if persist && episodeIMDbID != "" {
			_ = deps.DB.MarkPending(episodeIMDbID, "", "", reason)
		}
		deps.Logger.Event("[PENDING] %s: %s", path, reason)
		var c bool
		newContent, c = nfo.EnsureEmptyUserRating(newContent)
		changed = changed || c
	} else {
		// На уровне серии существует только IMDb, но слияние всё равно
		// нужно: в файле могут лежать записи, оставленные другим
		// инструментом, и затирать их мы не вправе.
		fetched := []nfo.RatingEntry{{Source: "imdb", Value: value, Votes: votes}}
		entries := nfo.MergeRatings(fetched, nfo.ParseRatingsBlock(newContent), enabledSources(deps.Config))
		nfo.SetDefaultRating(entries, "imdb")

		var c bool
		var buildErr error
		newContent, c, buildErr = nfo.ApplyRatingEntries(newContent, entries)
		if buildErr != nil {
			deps.Stats.IncError()
			deps.Logger.Event("[ERROR] %s: build ratings block: %v", path, buildErr)
			return buildErr
		}
		changed = changed || c

		if episodeIMDbID != "" {
			_ = deps.DB.ClearPending(episodeIMDbID, "", "")
		}
	}

	if episodeIMDbID != "" {
		var idsChanged bool
		newContent, idsChanged = nfo.FixLegacyUniqueIDs(newContent, episodeIMDbID, "")
		changed = changed || idsChanged
	}
	if deps.Config.CrewOrderFix {
		var c bool
		newContent, c = nfo.FixEmbyActorOrder(newContent)
		changed = changed || c
	}

	return finishFile(deps, path, raw, newContent, changed)
}

// fetchShowRatings запрашивает рейтинг сериала целиком. Если среди
// deps.Providers есть MDBList — используем FetchShowWithEpisodes, чтобы
// получить рейтинг сериала и таблицу рейтингов серий одним запросом.
//
// Кэш-гейт здесь применяется ТОЛЬКО к рейтингу самого сериала и только
// в ветке fetchRatingsWithFallback. Пропустить запрос к MDBList из-за
// свежего кэша нельзя: в кэше лежат рейтинги сериала, но не таблица
// рейтингов серий, а без неё каждую серию пришлось бы запрашивать
// отдельно — то есть экономия одного запроса обернулась бы десятками.
//
// Возвращает show (всё, что есть, для записи в файл), fresh (только
// полученное от провайдеров, для записи в кэш), таблицу серий и исход.
func fetchShowRatings(ctx context.Context, deps *Deps, ids providers.IDs) (show, fresh providers.FetchResult, episodes map[string]providers.EpisodeRating, outcome fetchOutcome) {
	for _, p := range deps.Providers {
		mdb, isMDBList := p.(*providers.MDBListProvider)
		if !isMDBList || !deps.Breaker.Allowed(p.Name()) {
			continue
		}
		showRes, episodesRes, err := mdb.FetchShowWithEpisodes(ctx, ids)
		switch {
		case err == nil:
			deps.Breaker.RecordSuccess(p.Name())
			deps.Stats.IncProviderRequest(p.Name())
			wanted := enabledSources(deps.Config)
			filtered := make(providers.FetchResult)
			for source, v := range showRes {
				if wanted[source] {
					filtered[source] = v
				}
			}
			// Всё пришло по сети, поэтому show и fresh здесь совпадают.
			return filtered, filtered, episodesRes, fetchOK
		case err == providers.ErrNotFound, err == providers.ErrNoRatings:
			deps.Breaker.RecordSuccess(p.Name())
			deps.Stats.IncProviderRequest(p.Name())
		case err == providers.ErrQuotaExhausted:
			if !deps.quotaWarned[p.Name()] {
				deps.quotaWarned[p.Name()] = true
				deps.Logger.Event("[QUOTA_EXHAUSTED] %s: daily quota exhausted, falling back to cached values for the rest of this run", p.Name())
			}
		case providers.IsNetworkError(err):
			// Та же запись, что и в fetchRatingsWithFallback, и по той же
			// причине. Без неё сбой на этой ветке не оставлял в логе следа
			// вовсе: сериал молча уходил на общий путь через OMDb, а прогон
			// при этом стоил лишних двадцати секунд. Обнаружился он только
			// по сдвигу ключей в round-robin — цена молчания в этом месте
			// оказалась выше, чем казалось.
			deps.Logger.Event("[PROVIDER_ERROR] %s (%s): %v", p.Name(), idsLabel(ids), err)
			if deps.Breaker.RecordNetworkFailure(p.Name()) {
				deps.Logger.Event("[PROVIDER_DOWN] %s: tripped after repeated network failures, disabled for the rest of this run", p.Name())
			}
			time.Sleep(deps.Breaker.Backoff())
		}
		break
	}

	// Сюда попадаем, если MDBList не дал результата. Общий путь опросит
	// провайдеров ещё раз и сам классифицирует исход — таблицы серий
	// на этом пути уже не будет, серии придётся добирать поштучно.
	show, fresh, outcome = fetchRatingsWithFallback(ctx, deps, ids, providers.MediaTypeShow)
	return show, fresh, nil, outcome
}

// finishFile — общая для фильмов, tvshow.nfo и файлов серий финальная часть:
// бэкап (если что-то изменилось) и запись на диск.
func finishFile(deps *Deps, path string, raw []byte, newContent string, changed bool) error {
	if !changed {
		deps.Stats.IncUnchanged()
		deps.Logger.Detail("[NO_CHANGE] %s", path)
		return nil
	}

	if deps.Config.BackupEnabled {
		category, archivePath, ok := backupCategory(deps.Config, path)
		if !ok {
			deps.Stats.IncError()
			deps.Logger.Event("[ERROR] %s: file is outside configured MOVIES_PATH/TVSHOWS_PATH roots, cannot backup", path)
			return fmt.Errorf("file outside configured roots: %s", path)
		}
		if err := backup.Save(deps.Config.BackupDir, category, archivePath, raw); err != nil {
			deps.Stats.IncError()
			deps.Logger.Event("[ERROR] %s: backup failed: %v", path, err)
			return err
		}
		deps.Stats.IncBackup()
	}

	if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
		deps.Stats.IncError()
		deps.Logger.Event("[ERROR] %s: write failed: %v", path, err)
		return err
	}
	deps.Stats.IncUpdated()
	deps.Logger.Detail("[UPDATED] %s", path)
	return nil
}
