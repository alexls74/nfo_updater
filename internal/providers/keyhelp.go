// internal/providers/keyhelp.go
package providers

import (
	"fmt"
	"strings"
)

// KeyHelp — справка о том, где взять ключ конкретного сервиса.
//
// Живёт здесь, а не в тексте -h и не в config.conf, намеренно: одна и та же
// справка нужна в трёх местах (помощь, ошибка конфига, лог прогона при
// протухшем ключе), и разъехавшиеся формулировки со временем неизбежны,
// если держать их порознь.
type KeyHelp struct {
	Provider string   // как его возвращает Provider.Name() — по этому полю идёт поиск
	Display  string   // человекочитаемое имя сервиса
	Setting  string   // имя параметра в config.conf
	URL      string   // страница, где ключ выдают
	Note     []string // оговорки; уже разбиты на строки, переносить по ширине терминала мы не умеем
}

// keyHelps — слайс, а не map, ради стабильного порядка вывода.
var keyHelps = []KeyHelp{
	{
		Provider: "omdb",
		Display:  "OMDb",
		Setting:  "OMDB_API_KEYS",
		URL:      "https://www.omdbapi.com/apikey.aspx",
		Note: []string{
			"The free tier allows 1000 requests per day. Several keys may be",
			"listed comma-separated: they are used in turn as each one runs out.",
		},
	},
	{
		Provider: "mdblist",
		Display:  "MDBList",
		Setting:  "MDBLIST_API_KEYS",
		URL:      "https://mdblist.com/preferences/",
		Note: []string{
			"The free tier allows 1000 requests per day. Several keys may be",
			"listed comma-separated: they are used in turn as each one runs out.",
		},
	},
	{
		Provider: "tmdb",
		Display:  "TMDb",
		Setting:  "TMDB_API_KEY",
		URL:      "https://www.themoviedb.org/settings/api",
		Note: []string{
			`Use the "API Key" value: 32 characters, digits and lowercase a-f.`,
			`Do NOT use the "API Read Access Token" shown next to it on the same`,
			"page. it belongs to another authentication scheme, and will be rejected.",
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

// Text — компактная форма для лога прогона: первая строка самодостаточна,
// оговорки идут ниже с отступом.
func (h KeyHelp) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: get a key at %s and put it into %s", h.Display, h.URL, h.Setting)
	for _, n := range h.Note {
		fmt.Fprintf(&b, "\n    %s", n)
	}
	return b.String()
}

// FormatKeyHelp — блок по всем трём сервисам для -h и для сообщения об
// отсутствующих ключах. indent добавляется к каждой строке.
func FormatKeyHelp(indent string) string {
	var b strings.Builder
	for i, h := range keyHelps {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s%-8s %s\n", indent, h.Display, h.Setting)
		fmt.Fprintf(&b, "%s%-8s %s\n", indent, "", h.URL)
		for _, n := range h.Note {
			fmt.Fprintf(&b, "%s%-8s %s\n", indent, "", n)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
