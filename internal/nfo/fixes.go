// internal/nfo/fixes.go
package nfo

import (
	"regexp"
	"strings"
)

var (
	// Как и якоря вставки в build.go, этот НЕ захватывает пробелы после
	// тега: иначе следующий за последним актёром тег терял свой отступ.
	reActorEnd = regexp.MustCompile(`(?i)</actor>`)

	// Отступ для переносимых <credits>/<director> берём по <actor>,
	// а не по <title>. Переносим мы их именно в актёрскую часть файла,
	// и логично, чтобы они встали вровень с соседями по месту, а не
	// вровень с тегами верхнего уровня. Практически в .nfo это один
	// и тот же уровень, но опираться правильнее на то, рядом с чем
	// блок реально окажется.
	reActorIndent = regexp.MustCompile(`(?m)^([ \t]*)<actor[ >]`)

	reCreditsOrDirector = regexp.MustCompile(`(?is)[ \t]*<(?:credits|director)[^>]*>.*?</(?:credits|director)>[ \t]*\r?\n?`)
)

// DetectActorIndent — отступ строки <actor>. Если актёров в файле нет,
// откатывается на общий отступ верхнего уровня.
func DetectActorIndent(content string) string {
	if m := reActorIndent.FindStringSubmatch(content); m != nil {
		return m[1]
	}
	return DetectIndent(content)
}

// FixLegacyUniqueIDs добавляет современные <uniqueid type="imdb">/<uniqueid
// type="tmdb"> для уже известных ID, но только если такого <uniqueid> ещё
// нет. Устаревшие теги не удаляются.
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

// FixMissingPremiered добавляет <premiered>, если его ещё нет. premieredDate
// уже должен быть получен вызывающим кодом (через TMDb API). Пустая
// premieredDate означает "не смогли узнать дату" — ничего не делаем.
func FixMissingPremiered(content, premieredDate string) (updated string, changed bool) {
	if premieredDate == "" || HasPremiered(content) {
		return content, false
	}
	indent := DetectIndent(content)
	tag := indent + "<premiered>" + premieredDate + "</premiered>"
	return insertAfterAnchor(content, tag), true
}

// FixEmbyActorOrder переносит <credits>/<director>, стоящие выше последнего
// <actor>, на позицию сразу после него.
//
// Речь о порядке ОТОБРАЖЕНИЯ, а не о потере данных. Раньше Emby строила
// карточку сама: сначала актёры, потом съёмочная группа — как принято
// в каталогах, — независимо от того, как написан .nfo. После одного из
// обновлений разбор .nfo сменился: теперь блоки выводятся строго в том
// порядке, в каком лежат в файле, и там, где съёмочная группа записана
// выше актёров, она первой и показывается. Фикс возвращает привычный вид.
//
// Отсюда и отдельный флаг CREW_ORDER_FIX. Правка косметическая, вкусовая,
// и решать за пользователя, какой порядок ему приятнее.
//
// Флаг намеренно НЕ привязан к EMBY_ENABLED. Секция медиасервера в конфиге
// необязательна: пользователь Emby вполне может не заполнять адрес и ключ,
// полагаясь на плановое сканирование сервера, — и при такой привязке фикс
// до него не доходил бы, хотя карточку он видит ту же самую. Название
// функции сохранено: поведение специфично именно для Emby, какой бы
// настройкой оно ни включалось.
//
// Это ЕДИНСТВЕННАЯ правка во всём проекте, которая переставляет уже
// существующие теги, а не дописывает новые. Обратной операции нет:
// исходный порядок после перезаписи восстанавливается только из бэкапа.
//
// Отступ переносимых блоков задаётся заново по <actor>, а не сохраняется
// исходный: блок мог лежать на другом уровне вложенности, и перенос его
// «как был» оставил бы файл разъехавшимся.
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
	indent := DetectActorIndent(content)

	var moved strings.Builder
	for _, m := range matches {
		moved.WriteString("\n")
		moved.WriteString(indent)
		// TrimSpace снимает старый отступ и хвостовой перевод строки —
		// вместо них ставим свои.
		moved.WriteString(strings.TrimSpace(m))
	}

	return newBefore + moved.String() + after, true
}
