package nfo

import (
	"fmt"
	"regexp"
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
)

// ParseIDs извлекает imdb_id и tmdb_id из содержимого .nfo-файла.
// Приоритет — современные <uniqueid type="...">. Легаси <id> проверяется
// на формат ^tt\d{7,9}$ — мусор в него не попадёт как валидный ID.
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
