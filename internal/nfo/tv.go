// internal/nfo/tv.go
package nfo

import (
	"regexp"
	"strconv"
	"strings"
)

// FileType — тип .nfo файла, определяется по корневому XML-тегу, а не по
// имени файла (имена у разных скрейперов отличаются, см. обсуждение).
type FileType string

const (
	FileTypeMovie   FileType = "movie"
	FileTypeTVShow  FileType = "tvshow"
	FileTypeEpisode FileType = "episode"
	FileTypeUnknown FileType = ""
)

var reRootTag = regexp.MustCompile(`(?s)^\s*(?:<\?xml[^>]*\?>\s*)?<(movie|tvshow|episodedetails)\b`)

// DetectFileType определяет тип файла по корневому тегу. Возвращает
// FileTypeUnknown, если файл не похож ни на один из трёх известных типов
// (например, season.nfo или что-то ещё не поддерживаемое).
func DetectFileType(content string) FileType {
	m := reRootTag.FindStringSubmatch(content)
	if m == nil {
		return FileTypeUnknown
	}
	switch m[1] {
	case "movie":
		return FileTypeMovie
	case "tvshow":
		return FileTypeTVShow
	case "episodedetails":
		return FileTypeEpisode
	default:
		return FileTypeUnknown
	}
}

var (
	// у tvshow.nfo <id> означает tvdb, а не imdb (в отличие от movie!) —
	// поэтому для сериалов imdb ищем ТОЛЬКО через <imdbid> и современный
	// <uniqueid type="imdb">, никогда через <id>.
	reImdbIDTag = regexp.MustCompile(`<imdbid>([^<]*)</imdbid>`)
	reTvdbIDTag = regexp.MustCompile(`<tvdbid>(\d+)</tvdbid>`)

	reSeasonTag  = regexp.MustCompile(`<season>(-?\d+)</season>`)
	reEpisodeTag = regexp.MustCompile(`<episode>(-?\d+)</episode>`)
)

// ParseTVShowIDs извлекает imdb/tmdb/tvdb id из tvshow.nfo. Приоритет —
// современные <uniqueid type="...">, как и у фильмов; легаси — <imdbid>
// (не <id> — см. предупреждение выше) и <tvdbid>.
func ParseTVShowIDs(content string) (imdbID, tmdbID, tvdbID string) {
	if m := reUniqueIDImdb.FindStringSubmatch(content); m != nil {
		imdbID = m[1]
	} else if m := reImdbIDTag.FindStringSubmatch(content); m != nil {
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

	if m := reTvdbIDTag.FindStringSubmatch(content); m != nil {
		tvdbID = m[1]
	}
	return imdbID, tmdbID, tvdbID
}

// ParseEpisodeInfo извлекает imdb id конкретной серии (свой, отдельный от
// id сериала — см. обсуждение) и номера сезона/эпизода, нужные для
// сопоставления с данными, полученными по сериалу целиком через MDBList.
// ok == false, если не удалось распознать номер сезона ИЛИ эпизода —
// без них файл серии нельзя сопоставить с чем-либо, обрабатывать нечего.
func ParseEpisodeInfo(content string) (imdbID string, season, episode int, ok bool) {
	if m := reUniqueIDImdb.FindStringSubmatch(content); m != nil {
		imdbID = m[1]
	} else if m := reImdbIDTag.FindStringSubmatch(content); m != nil {
		candidate := strings.TrimSpace(m[1])
		if reImdbFormat.MatchString(candidate) {
			imdbID = candidate
		}
	}

	sm := reSeasonTag.FindStringSubmatch(content)
	em := reEpisodeTag.FindStringSubmatch(content)
	if sm == nil || em == nil {
		return imdbID, 0, 0, false
	}
	season, errS := strconv.Atoi(sm[1])
	episode, errE := strconv.Atoi(em[1])
	if errS != nil || errE != nil {
		return imdbID, 0, 0, false
	}
	return imdbID, season, episode, true
}
