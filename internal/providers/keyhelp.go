// internal/providers/keyhelp.go
package providers

import (
	"fmt"
	"strings"
)

// KeyHelp — справка о том, где взять ключ конкретного сервиса.
//
// Живёт здесь, а не в тексте -h и не в config.conf, намеренно: одна и та же
// справка нужна в четырёх местах (помощь, ошибка конфига, лог прогона при
// протухшем ключе, мастер настройки), и разъехавшиеся формулировки со
// временем неизбежны, если держать их порознь.
type KeyHelp struct {
	Provider string // как его возвращает Provider.Name() — по этому полю идёт поиск
	Display  string // человекочитаемое имя сервиса
	Setting  string // имя параметра в config.conf
	URL      string // страница, где ключ выдают

	// DailyLimit — суточная квота бесплатного тарифа. Ноль означает, что
	// суточного лимита у сервиса нет вовсе (TMDb).
	DailyLimit int

	// Multi — сервис допускает несколько ключей: они расходуются по очереди,
	// когда квота предыдущего выбрана.
	Multi bool

	// Note — оговорки, верные в любом контексте. Здесь НЕ должно быть ничего
	// про формат записи в конфиге: мастер спрашивает ключи по одному, и
	// фраза про перечисление через запятую сбивала бы его пользователя
	// с толку. Такой текст собирается отдельно, см. fileNote.
	Note []string
}

// keyHelps — слайс, а не map, ради стабильного порядка вывода.
var keyHelps = []KeyHelp{
	{
		Provider:   "omdb",
		Display:    "OMDb",
		Setting:    "OMDB_API_KEYS",
		URL:        "https://www.omdbapi.com/apikey.aspx",
		DailyLimit: 1000,
		Multi:      true,
		Note: []string{
			"The key arrives by email and has to be activated by the link in it.",
			"Until that is done the service answers as if the key were wrong.",
		},
	},
	{
		Provider:   "mdblist",
		Display:    "MDBList",
		Setting:    "MDBLIST_API_KEYS",
		URL:        "https://mdblist.com/preferences/",
		DailyLimit: 1000,
		Multi:      true,
	},
	{
		Provider: "tmdb",
		Display:  "TMDb",
		Setting:  "TMDB_API_KEY",
		URL:      "https://www.themoviedb.org/settings/api",
		Note: []string{
			`Use the "API Key" value: 32 characters, digits and lowercase a-f.`,
			`Do NOT use the "API Read Access Token" shown next to it on the same`,
			"page. It belongs to another authentication scheme, and will be rejected.",
		},
	},
}

// KeyHelpFor находит справку по имени провайдера.
func KeyHelpFor(provider string) (KeyHelp, bool) {
	for _, h := range keyHelps {
		if strings.EqualFold(h.Provider, provider) {
			return h, true
		}
	}
	return KeyHelp{}, false
}

// QuotaNote — одна строка про суточную квоту, годная везде. Пусто, если
// суточного лимита у сервиса нет.
func (h KeyHelp) QuotaNote() string {
	if h.DailyLimit == 0 {
		return ""
	}
	return fmt.Sprintf("The free tier allows %d requests per day.", h.DailyLimit)
}

// fileNote — оговорка, осмысленная ТОЛЬКО там, где ключи вписывают руками
// в конфиг: в -h, в сообщении о незаполненных ключах, в логе прогона.
// Мастер её не показывает — он спрашивает ключи по одному и сам собирает
// список, а предложение перечислить их через запятую в поле ввода привело бы
// к тому, что вставленная строка ушла бы на проверку целиком и была бы
// отвергнута.
func (h KeyHelp) fileNote() string {
	if !h.Multi {
		return ""
	}
	return "Several keys may be listed comma-separated: they are used in turn."
}

// notesForFile — полный набор оговорок для контекста «правим конфиг руками».
func (h KeyHelp) notesForFile() []string {
	out := make([]string, 0, len(h.Note)+2)
	if s := h.QuotaNote(); s != "" {
		out = append(out, s)
	}
	if s := h.fileNote(); s != "" {
		out = append(out, s)
	}
	return append(out, h.Note...)
}

// Text — компактная форма для лога прогона: первая строка самодостаточна,
// оговорки идут ниже с отступом.
func (h KeyHelp) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: get a key at %s and put it into %s", h.Display, h.URL, h.Setting)
	for _, n := range h.notesForFile() {
		fmt.Fprintf(&b, "\n    %s", n)
	}
	return b.String()
}

// FormatKeyHelp — блок по всем сервисам для -h и для сообщения об
// отсутствующих ключах. indent добавляется к каждой строке.
func FormatKeyHelp(indent string) string {
	var b strings.Builder
	for i, h := range keyHelps {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s%-8s %s\n", indent, h.Display, h.Setting)
		fmt.Fprintf(&b, "%s%-8s %s\n", indent, "", h.URL)
		for _, n := range h.notesForFile() {
			fmt.Fprintf(&b, "%s%-8s %s\n", indent, "", n)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
