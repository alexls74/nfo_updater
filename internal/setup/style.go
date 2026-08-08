// internal/setup/style.go
package setup

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// lineWidth — ширина, под которую свёрстан весь остальной вывод программы:
// текст -h, комментарии config.conf, сводка прогона.
//
// Ширину терминала намеренно НЕ спрашиваем. Единственный способ узнать её —
// ioctl(TIOCGWINSZ), а это unsafe.Pointer ради одного числа; мастер при этом
// не показывает ничего, что не влезло бы в 78 колонок, кроме разве что очень
// длинного пути в умолчании — но перенос такой строки терминалом ничего
// не портит.
const lineWidth = 78

// Коды SGR. Набор намеренно скудный: чем меньше цветов, тем меньше шансов
// попасть в тему, где какой-то из них нечитаем.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiAccent = "\x1b[36m" // голубой — единственный акцентный цвет, только на приглашении ввода
	ansiGreen  = "\x1b[32m"
	ansiRed    = "\x1b[31m"
)

// Style печатает оформленный текст либо, если цвет отключён, тот же текст
// без единого управляющего символа. Ни один метод не меняет саму строку:
// вывод без цвета обязан читаться так же, как с цветом, иначе лог сессии,
// снятый через script или скопированный в баг-репорт, будет отличаться
// от того, что человек видел на экране.
type Style struct {
	enabled bool
}

func NewStyle(enabled bool) *Style {
	return &Style{enabled: enabled}
}

// colorEnabled решает, красить ли вывод в этот файл.
//
// Три причины отказаться, все три общеприняты:
//   - переменная NO_COLOR (по спецификации no-color.org — задана и непуста);
//   - TERM=dumb или пустой TERM: терминал управляющих последовательностей
//     не понимает и покажет их как мусор;
//   - вывод не в терминал, а в файл или канал.
//
// Последняя проверка для мастера почти всегда истинна (он пишет в /dev/tty),
// но остаётся на случай, если Style когда-нибудь применят к обычному stdout.
func colorEnabled(f *os.File) bool {
	if v := os.Getenv("NO_COLOR"); v != "" {
		return false
	}
	switch os.Getenv("TERM") {
	case "", "dumb":
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func (s *Style) wrap(code, text string) string {
	if !s.enabled || text == "" {
		return text
	}
	return code + text + ansiReset
}

// Bold — вопросы и заголовки секций.
func (s *Style) Bold(text string) string { return s.wrap(ansiBold, text) }

// Dim — подсказки, умолчания в скобках, линии рамок.
func (s *Style) Dim(text string) string { return s.wrap(ansiDim, text) }

// Accent — только приглашение ввода. Одно место на весь мастер: акцент,
// который стоит везде, перестаёт быть акцентом.
func (s *Style) Accent(text string) string { return s.wrap(ansiAccent, text) }

// Green и Red применяются исключительно к маркерам результата проверки.
// Красить ими сам текст сообщения не нужно: маркер уже сказал всё, что
// нужно, а красная строка целиком читается как авария.
func (s *Style) Green(text string) string { return s.wrap(ansiGreen, text) }
func (s *Style) Red(text string) string   { return s.wrap(ansiRed, text) }

// Section — заголовок секции во всю ширину:
//
//	── MEDIA LIBRARY ────────────────────────────────────────────────────────
//
// Пустая строка перед заголовком входит в результат: секции всегда отделены
// друг от друга, и забыть про отступ в вызывающем коде нельзя.
func (s *Style) Section(title string) string {
	const prefix = "── "

	// Хвост считается по рунам, а не по байтам: одна ─ занимает три байта,
	// и len() дал бы линию втрое короче нужной.
	tail := lineWidth - utf8.RuneCountInString(prefix) - utf8.RuneCountInString(title) - 1
	if tail < 1 {
		tail = 1
	}
	return "\n" + s.Dim(prefix) + s.Bold(title) + " " + s.Dim(strings.Repeat("─", tail)) + "\n"
}

// Rule — разделительная линия без заголовка. Нужна перед итоговой сводкой.
func (s *Style) Rule() string {
	return s.Dim(strings.Repeat("─", lineWidth))
}

// Marker — знак результата проверки. Без эмодзи: ✓ и ✗ есть в любом
// моноширинном шрифте и занимают ровно одну колонку, в отличие от ✅ и ❌,
// которые в половине терминалов рисуются двойной ширины и ломают выравнивание.
func (s *Style) Marker(ok bool) string {
	if ok {
		return s.Green("✓")
	}
	return s.Red("✗")
}

// Result — строка результата проверки: маркер, пробел, текст.
func (s *Style) Result(ok bool, format string, a ...any) string {
	return s.Marker(ok) + " " + fmt.Sprintf(format, a...)
}
