// internal/nfo/parse.go
package nfo

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	reUniqueIDImdb = regexp.MustCompile(`<uniqueid[^>]*type="imdb"[^>]*>(tt\d{7,9})</uniqueid>`)
	reUniqueIDTmdb = regexp.MustCompile(`<uniqueid[^>]*type="tmdb"[^>]*>(\d+)</uniqueid>`)

	reLegacyID     = regexp.MustCompile(`<id>([^<]*)</id>`)
	reLegacyTmdbID = regexp.MustCompile(`<tmdbid>(\d+)</tmdbid>`)

	reImdbFormat = regexp.MustCompile(`^tt\d{7,9}$`)

	reYear       = regexp.MustCompile(`<year>(\d{4})</year>`)
	rePremiered  = regexp.MustCompile(`<premiered>([^<]*)</premiered>`)
	reUserRating = regexp.MustCompile(`<userrating>([^<]*)</userrating>`)

	// Разбор существующего блока <ratings>.
	reRatingsBlockFind = regexp.MustCompile(`(?s)<ratings>(.*?)</ratings>`)
	reRatingEntryFind  = regexp.MustCompile(`(?s)<rating\b([^>]*)>(.*?)</rating>`)
	reAttrName         = regexp.MustCompile(`(?i)\bname\s*=\s*"([^"]*)"`)
	reAttrMax          = regexp.MustCompile(`(?i)\bmax\s*=\s*"([^"]*)"`)
	reAttrDefault      = regexp.MustCompile(`(?i)\bdefault\s*=\s*"([^"]*)"`)
	reValueTag         = regexp.MustCompile(`(?s)<value>\s*([^<]*?)\s*</value>`)
	reVotesTag         = regexp.MustCompile(`(?s)<votes>\s*([0-9,]+)\s*</votes>`)
)

// ParseIDs извлекает imdb_id и tmdb_id из содержимого .nfo-файла ФИЛЬМА.
// Приоритет — современные <uniqueid type="...">. Легаси <id> проверяется
// на формат ^tt\d{7,9}$ — мусор в него не попадёт как валидный ID.
// ВНИМАНИЕ: для сериалов/серий используется ParseTVShowIDs/ParseEpisodeInfo
// из tv.go — там <id> может означать другое (tvdb, не imdb).
func ParseIDs(content string) (imdbID, tmdbID string) {
	if m := reUniqueIDImdb.FindStringSubmatch(content); m != nil {
		imdbID = m[1]
	} else if m := reLegacyID.FindStringSubmatch(content); m != nil {
		candidate := strings.TrimSpace(m[1])
		if reImdbFormat.MatchString(candidate) {
			imdbID = candidate
		}
	}

	if m := reUniqueIDTmdb.FindStringSubmatch(content); m != nil {
		tmdbID = m[1]
	} else if m := reLegacyTmdbID.FindStringSubmatch(content); m != nil {
		tmdbID = m[1]
	}
	return imdbID, tmdbID
}

// HasUniqueID проверяет, есть ли уже <uniqueid type="idType">.
func HasUniqueID(content, idType string) bool {
	re := regexp.MustCompile(fmt.Sprintf(`<uniqueid[^>]*type="%s"[^>]*>`, regexp.QuoteMeta(idType)))
	return re.MatchString(content)
}

// ParseYear возвращает значение <year>, если есть, иначе "".
func ParseYear(content string) string {
	if m := reYear.FindStringSubmatch(content); m != nil {
		return m[1]
	}
	return ""
}

// HasPremiered проверяет, есть ли уже тег <premiered>.
func HasPremiered(content string) bool {
	return rePremiered.MatchString(content)
}

// HasUserRating проверяет, есть ли уже тег <userrating> — вне зависимости
// от значения внутри. Используется перед аварийной вставкой
// <userrating>0</userrating>, чтобы не затирать личную оценку пользователя.
func HasUserRating(content string) bool {
	return reUserRating.MatchString(content)
}

// ParseRatingsBlock разбирает существующий блок <ratings> в записи.
// Функция, обратная BuildRatingsBlock: нужна и для сохранения рейтингов,
// которые не удалось обновить в этом прогоне, и для семантического
// сравнения «изменился ли файл на самом деле».
//
// Записи с неизвестным нам name возвращаются как чужие: Source пуст,
// а KodiName и Max сохранены дословно, чтобы выписать их без потерь.
// Блок вне <ratings> (устаревший <rating> верхнего уровня) не разбирается.
func ParseRatingsBlock(content string) []RatingEntry {
	block := reRatingsBlockFind.FindStringSubmatch(content)
	if block == nil {
		return nil
	}

	matches := reRatingEntryFind.FindAllStringSubmatch(block[1], -1)
	if matches == nil {
		return nil
	}

	out := make([]RatingEntry, 0, len(matches))
	for _, m := range matches {
		attrs, body := m[1], m[2]

		// Самозакрывающийся <rating ... /> смысла не несёт — пропускаем.
		if strings.HasSuffix(strings.TrimSpace(attrs), "/") {
			continue
		}

		name := ""
		if a := reAttrName.FindStringSubmatch(attrs); a != nil {
			name = strings.TrimSpace(a[1])
		}
		if name == "" {
			continue
		}

		value := ""
		if v := reValueTag.FindStringSubmatch(body); v != nil {
			value = v[1]
		}
		if value == "" {
			continue
		}

		votes := 0
		if v := reVotesTag.FindStringSubmatch(body); v != nil {
			if n, err := strconv.Atoi(strings.ReplaceAll(v[1], ",", "")); err == nil {
				votes = n
			}
		}

		isDefault := false
		if d := reAttrDefault.FindStringSubmatch(attrs); d != nil {
			isDefault = strings.EqualFold(strings.TrimSpace(d[1]), "true")
		}

		entry := RatingEntry{
			Value:   value,
			Votes:   votes,
			Default: isDefault,
		}

		if source, known := kodiNameToSource[strings.ToLower(name)]; known {
			entry.Source = source
		} else {
			entry.KodiName = name
			if a := reAttrMax.FindStringSubmatch(attrs); a != nil {
				entry.Max = strings.TrimSpace(a[1])
			}
		}

		out = append(out, NormalizeEntry(entry))
	}

	if len(out) == 0 {
		return nil
	}
	return out
}
