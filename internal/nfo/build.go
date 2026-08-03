// internal/nfo/build.go
package nfo

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reRatingsBlockCut  = regexp.MustCompile(`(?s)\n[ \t]*<ratings>.*?</ratings>[ \t]*\n`)
	reLegacyRatingCut  = regexp.MustCompile(`(?s)\n[ \t]*<rating>\s*[0-9]+(?:\.[0-9]+)?\s*</rating>\s*<votes>\s*[0-9,]+\s*</votes>[ \t]*\n`)
	reOriginalTitleEnd = regexp.MustCompile(`(?i)</originaltitle>\s*`)
	reTitleEnd         = regexp.MustCompile(`(?i)</title>\s*`)
	reTopLevelIndent   = regexp.MustCompile(`(?m)^([ \t]*)<title>`)
)

type providerMeta struct {
	KodiName string
	Max      string
}

var providerMetaMap = map[string]providerMeta{
	"imdb":       {"imdb", "10"},
	"tmdb":       {"themoviedb", "10"},
	"trakt":      {"trakt", "10"},
	"tomatoes":   {"tomatometerallcritics", "100"},
	"popcorn":    {"tomatometerallaudience", "100"},
	"metacritic": {"metacritic", "100"},
}

// defaultRatingPriority — порядок fallback для default="true".
var defaultRatingPriority = []string{"imdb", "tmdb", "trakt", "popcorn", "tomatoes", "metacritic"}

// KnownRatingSources — используется config.Load() для валидации DEFAULT_RATING.
func KnownRatingSources() []string {
	out := make([]string, len(defaultRatingPriority))
	copy(out, defaultRatingPriority)
	return out
}

type RatingEntry struct {
	Source  string
	Value   string
	Votes   int
	Default bool
}

func BuildRatingsBlock(entries []RatingEntry, indent string) (string, error) {
	if len(entries) == 0 {
		return "", fmt.Errorf("build ratings block: no entries")
	}
	inner := indent + "  "

	var b strings.Builder
	b.WriteString(indent)
	b.WriteString("<ratings>\n")
	for _, e := range entries {
		meta, ok := providerMetaMap[e.Source]
		if !ok {
			return "", fmt.Errorf("build ratings block: unknown source %q", e.Source)
		}
		defaultAttr := ""
		if e.Default {
			defaultAttr = ` default="true"`
		}
		b.WriteString(inner)
		b.WriteString(fmt.Sprintf("<rating name=%q max=%q%s>\n", meta.KodiName, meta.Max, defaultAttr))
		b.WriteString(inner)
		b.WriteString("  <value>")
		b.WriteString(e.Value)
		b.WriteString("</value>\n")
		if e.Votes > 0 {
			b.WriteString(inner)
			b.WriteString(fmt.Sprintf("  <votes>%d</votes>\n", e.Votes))
		}
		b.WriteString(inner)
		b.WriteString("</rating>\n")
	}
	b.WriteString(indent)
	b.WriteString("</ratings>")
	return b.String(), nil
}

func DetectIndent(content string) string {
	if m := reTopLevelIndent.FindStringSubmatch(content); m != nil {
		return m[1]
	}
	return "  "
}

func insertAfterAnchor(content, block string) string {
	insertion := "\n" + block + "\n"

	if loc := reOriginalTitleEnd.FindStringIndex(content); loc != nil {
		return content[:loc[1]] + strings.TrimLeft(insertion, "\n") + content[loc[1]:]
	}
	if loc := reTitleEnd.FindStringIndex(content); loc != nil {
		return content[:loc[1]] + strings.TrimLeft(insertion, "\n") + content[loc[1]:]
	}
	return strings.TrimRight(content, "\n") + insertion
}

func ReplaceOrInsertRatings(content, newBlock string) string {
	content = reRatingsBlockCut.ReplaceAllString(content, "\n")
	content = reLegacyRatingCut.ReplaceAllString(content, "\n")
	return insertAfterAnchor(content, newBlock)
}

func ApplyRatings(content, newBlock string) (updated string, changed bool) {
	updated = ReplaceOrInsertRatings(content, newBlock)
	return updated, updated != content
}

// EnsureEmptyUserRating — аварийный случай: ни один рейтинг не найден.
// Вставляет <userrating>0</userrating>, только если тега ещё нет.
func EnsureEmptyUserRating(content string) (updated string, changed bool) {
	if HasUserRating(content) {
		return content, false
	}
	indent := DetectIndent(content)
	tag := indent + "<userrating>0</userrating>"
	return insertAfterAnchor(content, tag), true
}

type DefaultSelection struct {
	Source     string
	Overridden bool
	Reason     string
}

// ChooseDefaultRating решает, какой источник получит default="true",
// на основе того, ЧТО реально найдено (entries), а не через какой ID.
func ChooseDefaultRating(entries []RatingEntry, configuredDefault string) DefaultSelection {
	if len(entries) == 0 {
		return DefaultSelection{}
	}
	has := func(source string) bool {
		for _, e := range entries {
			if e.Source == source {
				return true
			}
		}
		return false
	}

	cfg := strings.ToLower(configuredDefault)
	if has(cfg) {
		return DefaultSelection{Source: cfg}
	}

	for _, source := range defaultRatingPriority {
		if has(source) {
			return DefaultSelection{
				Source:     source,
				Overridden: true,
				Reason:     fmt.Sprintf("configured default %q not available for this title, falling back to %q by priority", configuredDefault, source),
			}
		}
	}

	return DefaultSelection{}
}
