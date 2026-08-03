package nfo

import (
	"regexp"
	"strings"
)

var (
	reActorEnd          = regexp.MustCompile(`(?i)</actor>\s*`)
	reCreditsOrDirector = regexp.MustCompile(`(?is)[ \t]*<(?:credits|director)[^>]*>.*?</(?:credits|director)>[ \t]*\r?\n?`)
)

func FixLegacyUniqueIDs(content, imdbID, tmdbID string) (updated string, changed bool) {
	indent := DetectIndent(content)

	if imdbID != "" && !HasUniqueID(content, "imdb") {
		tag := indent + `<uniqueid type="imdb" default="true">` + imdbID + `</uniqueid>`
		content = insertAfterAnchor(content, tag)
		changed = true
	}
	if tmdbID != "" && !HasUniqueID(content, "tmdb") {
		defaultAttr := ""
		if imdbID == "" {
			defaultAttr = ` default="true"`
		}
		tag := indent + `<uniqueid type="tmdb"` + defaultAttr + `>` + tmdbID + `</uniqueid>`
		content = insertAfterAnchor(content, tag)
		changed = true
	}
	return content, changed
}

func FixMissingPremiered(content, premieredDate string) (updated string, changed bool) {
	if premieredDate == "" || HasPremiered(content) {
		return content, false
	}
	indent := DetectIndent(content)
	tag := indent + "<premiered>" + premieredDate + "</premiered>"
	return insertAfterAnchor(content, tag), true
}

func FixEmbyActorOrder(content string) (updated string, changed bool) {
	actorEnds := reActorEnd.FindAllStringIndex(content, -1)
	if len(actorEnds) == 0 {
		return content, false
	}
	lastActorEnd := actorEnds[len(actorEnds)-1][1]

	before := content[:lastActorEnd]
	after := content[lastActorEnd:]

	matches := reCreditsOrDirector.FindAllString(before, -1)
	if len(matches) == 0 {
		return content, false
	}

	newBefore := reCreditsOrDirector.ReplaceAllString(before, "")

	var moved strings.Builder
	for _, m := range matches {
		moved.WriteString(strings.TrimRight(m, "\r\n \t"))
		moved.WriteString("\n")
	}

	return newBefore + moved.String() + after, true
}
