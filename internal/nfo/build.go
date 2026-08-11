// internal/nfo/build.go
package nfo

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	reRatingsBlockCut = regexp.MustCompile(`(?s)\n[ \t]*<ratings>.*?</ratings>[ \t]*\n`)
	reLegacyRatingCut = regexp.MustCompile(`(?s)\n[ \t]*<rating>\s*[0-9]+(?:\.[0-9]+)?\s*</rating>\s*<votes>\s*[0-9,]+\s*</votes>[ \t]*\n`)

	// Якоря вставки НЕ захватывают пробелы после тега — см. комментарий
	// к insertAfterAnchor.
	reOriginalTitleEnd = regexp.MustCompile(`(?i)</originaltitle>`)
	reTitleEnd         = regexp.MustCompile(`(?i)</title>`)

	// Отступ верхнего уровня определяем по <title>: он есть в любом .nfo
	// любого типа и всегда лежит непосредственно внутри корневого тега.
	// Скобка [ >] нужна, чтобы не поймать <originaltitle> и <sorttitle>.
	reTopLevelIndent = regexp.MustCompile(`(?m)^([ \t]*)<title[ >]`)
)

// defaultIndent — на случай файла без <title> (теоретически возможен,
// практически не встречался). Четыре пробела — то, что пишут и Kodi,
// и tinyMediaManager.
const defaultIndent = "    "

type providerMeta struct {
	KodiName string
	Max      string
	// NoVotes — для рейтингов критиков MDBList отдаёт в votes число
	// РЕЦЕНЗИЙ, а не голосов зрителей (metacritic: 36, tomatoes: 209).
	// Kodi покажет это как «голоса», что вводит в заблуждение,
	// поэтому <votes> для таких источников не пишем.
	NoVotes bool
}

var providerMetaMap = map[string]providerMeta{
	"imdb":       {KodiName: "imdb", Max: "10"},
	"tmdb":       {KodiName: "themoviedb", Max: "10"},
	"trakt":      {KodiName: "trakt", Max: "10"},
	"popcorn":    {KodiName: "tomatometerallaudience", Max: "100"},
	"metacritic": {KodiName: "metacritic", Max: "100", NoVotes: true},
	"tomatoes":   {KodiName: "tomatometerallcritics", Max: "100", NoVotes: true},
}

// ratingOrder задаёт две вещи сразу: порядок записей в блоке <ratings>
// и цепочку приоритета при выборе default="true", когда настроенный
// источник для конкретного тайтла недоступен.
//
// Порядок значим для Emby. Проверено живыми тестами на сервере:
//   - зрительский рейтинг Emby берёт из записи с default="true",
//     позиция записи в блоке роли не играет;
//   - ИСКЛЮЧЕНИЕ: если default="true" стоит на tomatometerallcritics,
//     в зрительский слот он не допускается, и Emby берёт ПЕРВУЮ запись
//     блока, кроме него;
//   - рейтинг критиков Emby берёт из tomatometerallcritics;
//   - все прочие записи Emby игнорирует;
//   - значения шкалы 100 Emby сам конвертирует в шкалу 10.
//
// Фиксированный порядок делает поведение в этом единственном пограничном
// случае предсказуемым, не требуя отдельной логики.
//
// Kodi на порядок не смотрит — он определяет главный рейтинг по атрибуту
// default. Официальный NFO-агент Plex читает этот же блок как есть.
var ratingOrder = []string{"imdb", "tmdb", "popcorn", "trakt", "metacritic", "tomatoes"}

// kodiNameToSource — обратный маппинг для разбора существующего блока.
var kodiNameToSource = buildReverseNameMap()

func buildReverseNameMap() map[string]string {
	out := make(map[string]string, len(providerMetaMap))
	for source, meta := range providerMetaMap {
		out[strings.ToLower(meta.KodiName)] = source
	}
	return out
}

// KnownRatingSources — используется config.Load() для валидации DEFAULT_RATING.
func KnownRatingSources() []string {
	out := make([]string, len(ratingOrder))
	copy(out, ratingOrder)
	return out
}

type RatingEntry struct {
	// Source — наш внутренний ключ (imdb, tmdb, tomatoes, ...).
	// Пусто у ЧУЖИХ записей: имена, которых нет в providerMetaMap,
	// оставлены другим инструментом (tinyMediaManager, Ember и т.п.).
	// Такие записи мы сохраняем дословно и никогда не трогаем.
	Source string

	// KodiName и Max заполняются только у чужих записей. Для известных
	// источников оба значения берутся из providerMetaMap.
	KodiName string
	Max      string

	Value   string
	Votes   int
	Default bool
}

// Foreign — запись принадлежит чужому инструменту, мы её только переносим.
func (e RatingEntry) Foreign() bool { return e.Source == "" }

// resolve отдаёт атрибуты, с которыми запись будет выписана.
func (e RatingEntry) resolve() (name, max string, noVotes bool, err error) {
	if e.Foreign() {
		if e.KodiName == "" {
			return "", "", false, fmt.Errorf("rating entry has neither source nor name")
		}
		return e.KodiName, e.Max, false, nil
	}
	meta, ok := providerMetaMap[e.Source]
	if !ok {
		return "", "", false, fmt.Errorf("unknown rating source %q", e.Source)
	}
	return meta.KodiName, meta.Max, meta.NoVotes, nil
}

// normalizeValue убирает бессмысленный хвост ".0" у процентных рейтингов:
// FormatFloat отдаёт "83.0", а в NFO уместнее "83". Значения шкалы 10
// и значения чужих записей не трогаем.
func normalizeValue(value, max string) string {
	if max != "100" {
		return value
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return value
	}
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return value
}

// NormalizeEntry приводит запись к тому виду, в котором она реально попадёт
// в файл. Нужна не только при записи, но и при сравнении: иначе разница
// между "83.0" и "83" или лишний <votes> у критиков будут выглядеть как
// изменение и заставят переписать файл впустую.
func NormalizeEntry(e RatingEntry) RatingEntry {
	if e.Foreign() {
		return e
	}
	meta, ok := providerMetaMap[e.Source]
	if !ok {
		return e
	}
	e.Value = normalizeValue(e.Value, meta.Max)
	if meta.NoVotes {
		e.Votes = 0
	}
	e.KodiName = ""
	e.Max = ""
	return e
}

// BuildRatingsBlock собирает блок <ratings>. Вложенность строится из того
// же indent, что найден в файле: если файл сделан двумя пробелами, блок
// тоже будет по два, а не по четыре.
func BuildRatingsBlock(entries []RatingEntry, indent string) (string, error) {
	if len(entries) == 0 {
		return "", fmt.Errorf("build ratings block: no entries")
	}
	inner := indent + indent
	deep := inner + indent

	var b strings.Builder
	b.WriteString(indent)
	b.WriteString("<ratings>\n")
	for _, e := range entries {
		name, max, noVotes, err := e.resolve()
		if err != nil {
			return "", fmt.Errorf("build ratings block: %w", err)
		}

		attrs := fmt.Sprintf("name=%q", name)
		if max != "" {
			attrs += fmt.Sprintf(" max=%q", max)
		}
		if e.Default {
			attrs += ` default="true"`
		}

		b.WriteString(inner)
		b.WriteString(fmt.Sprintf("<rating %s>\n", attrs))
		b.WriteString(deep)
		b.WriteString("<value>")
		b.WriteString(normalizeValue(e.Value, max))
		b.WriteString("</value>\n")
		if e.Votes > 0 && !noVotes {
			b.WriteString(deep)
			b.WriteString(fmt.Sprintf("<votes>%d</votes>\n", e.Votes))
		}
		b.WriteString(inner)
		b.WriteString("</rating>\n")
	}
	b.WriteString(indent)
	b.WriteString("</ratings>")
	return b.String(), nil
}

// MergeRatings склеивает свежие данные с тем, что уже лежит в файле.
//
// Правила:
//   - источник получен в этом прогоне -> берём свежее значение;
//   - источник включён, но данных нет -> сохраняем старое из файла,
//     чтобы неудачный прогон (например, выбыли ключи MDBList) не стирал
//     рейтинги, собранные ранее;
//   - источник отключён в конфиге -> удаляем, это явное указание
//     пользователя, а не отсутствие данных;
//   - запись чужая и нам неизвестна -> переносим дословно в хвост.
//
// Атрибут default здесь снимается со всех записей: его расставляет
// ChooseDefaultRating уже после слияния.
func MergeRatings(fresh, existing []RatingEntry, enabled map[string]bool) []RatingEntry {
	freshBySource := make(map[string]RatingEntry, len(fresh))
	for _, e := range fresh {
		if e.Foreign() {
			continue
		}
		freshBySource[e.Source] = e
	}

	existingBySource := make(map[string]RatingEntry, len(existing))
	for _, e := range existing {
		if e.Foreign() {
			continue
		}
		existingBySource[e.Source] = e
	}

	out := make([]RatingEntry, 0, len(ratingOrder)+len(existing))
	for _, source := range ratingOrder {
		if !enabled[source] {
			continue
		}
		e, ok := freshBySource[source]
		if !ok {
			e, ok = existingBySource[source]
		}
		if !ok || strings.TrimSpace(e.Value) == "" {
			continue
		}
		e.Default = false
		out = append(out, NormalizeEntry(e))
	}

	for _, e := range existing {
		if !e.Foreign() {
			continue
		}
		e.Default = false
		out = append(out, e)
	}
	return out
}

// RatingsEqual сравнивает наборы записей по смыслу, а не побайтово.
// Отступы, порядок атрибутов и косметические различия значений
// изменением не считаются.
func RatingsEqual(a, b []RatingEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x := NormalizeEntry(a[i])
		y := NormalizeEntry(b[i])
		if x.Source != y.Source ||
			!strings.EqualFold(x.KodiName, y.KodiName) ||
			x.Max != y.Max ||
			x.Value != y.Value ||
			x.Votes != y.Votes ||
			x.Default != y.Default {
			return false
		}
	}
	return true
}

// DetectIndent определяет отступ верхнего уровня по строке с <title>.
//
// Именно по <title>, а не по первой попавшейся строке: он гарантированно
// лежит непосредственно внутри корневого тега (<movie>, <tvshow>,
// <episodedetails>), поэтому его отступ — это ровно один уровень
// вложенности, тот самый, на который встают все наши вставки.
func DetectIndent(content string) string {
	if m := reTopLevelIndent.FindStringSubmatch(content); m != nil {
		return m[1]
	}
	return defaultIndent
}

// insertAfterAnchor вставляет готовый блок сразу после </originaltitle>
// (или после </title>, если первого в файле нет).
func insertAfterAnchor(content, block string) string {
	if loc := reOriginalTitleEnd.FindStringIndex(content); loc != nil {
		return content[:loc[1]] + "\n" + block + content[loc[1]:]
	}
	if loc := reTitleEnd.FindStringIndex(content); loc != nil {
		return content[:loc[1]] + "\n" + block + content[loc[1]:]
	}
	return strings.TrimRight(content, "\n") + "\n" + block + "\n"
}

func ReplaceOrInsertRatings(content, newBlock string) string {
	content = reRatingsBlockCut.ReplaceAllString(content, "\n")
	content = reLegacyRatingCut.ReplaceAllString(content, "\n")
	return insertAfterAnchor(content, newBlock)
}

// ApplyRatingEntries — основной путь записи рейтингов.
// Файл не переписывается, если новый набор семантически совпадает
// с тем, что уже в нём лежит: это снимает пустые бэкапы и мусор в логе.
func ApplyRatingEntries(content string, entries []RatingEntry) (updated string, changed bool, err error) {
	if len(entries) == 0 {
		return content, false, nil
	}
	if RatingsEqual(ParseRatingsBlock(content), entries) {
		return content, false, nil
	}
	block, err := BuildRatingsBlock(entries, DetectIndent(content))
	if err != nil {
		return content, false, err
	}
	return ReplaceOrInsertRatings(content, block), true, nil
}

// ApplyRatings — низкоуровневый вариант для готового блока.
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
// Чужие записи в выборе не участвуют.
func ChooseDefaultRating(entries []RatingEntry, configuredDefault string) DefaultSelection {
	if len(entries) == 0 {
		return DefaultSelection{}
	}
	has := func(source string) bool {
		for _, e := range entries {
			if !e.Foreign() && e.Source == source {
				return true
			}
		}
		return false
	}

	cfg := strings.ToLower(strings.TrimSpace(configuredDefault))
	if has(cfg) {
		return DefaultSelection{Source: cfg}
	}

	for _, source := range ratingOrder {
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

// SetDefaultRating проставляет default="true" выбранному источнику
// и снимает его со всех остальных записей.
func SetDefaultRating(entries []RatingEntry, source string) {
	for i := range entries {
		entries[i].Default = !entries[i].Foreign() && entries[i].Source == source
	}
}
