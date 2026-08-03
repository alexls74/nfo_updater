// internal/mediaserver/help.go
package mediaserver

import (
	"fmt"
	"strings"
)

// serverHelp — где включается сервер и где у него берут ключ.
//
// Живёт здесь, рядом с реализацией, а не в тексте -h: когда у Jellyfin
// в очередной раз переедет страница выдачи ключей, поправить придётся
// один файл, и это будет тот же файл, где лежит код обращения к нему.
type serverHelp struct {
	display  string
	settings string
	where    string // где взять ключ: пункт меню или адрес страницы
}

var serverHelps = []serverHelp{
	{
		display:  "Emby",
		settings: "EMBY_ENABLED, EMBY_URL, EMBY_API_KEY",
		where:    "key: Emby dashboard, Advanced - API Keys",
	},
	{
		display:  "Jellyfin",
		settings: "JELLYFIN_ENABLED, JELLYFIN_URL, JELLYFIN_API_KEY",
		where:    "key: Jellyfin dashboard, API Keys",
	},
	{
		display:  "Plex",
		settings: "PLEX_ENABLED, PLEX_URL, PLEX_TOKEN, PLEX_SECTION_IDS",
		where: "token: https://support.plex.tv/articles/204059436-" +
			"finding-an-authentication-token-x-plex-token/",
	},
}

// FormatHelp — блок для -h. indent добавляется к каждой строке.
func FormatHelp(indent string) string {
	var b strings.Builder
	for i, h := range serverHelps {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s%-9s %s\n", indent, h.display, h.settings)
		fmt.Fprintf(&b, "%s%-9s %s\n", indent, "", h.where)
	}
	return strings.TrimRight(b.String(), "\n")
}
