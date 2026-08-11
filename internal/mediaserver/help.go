// internal/mediaserver/help.go
package mediaserver

import "strings"

// ServerHelp — где включается сервер и где у него берут ключ.
type ServerHelp struct {
	Name     string // как его возвращает Server.Name(): emby, jellyfin, plex
	Display  string // человекочитаемое имя
	Settings string // имена параметров в config.conf
	Where    string // где взять ключ: пункт меню или адрес страницы
}

// serverHelps — слайс, а не map, ради стабильного порядка обхода: мастер
// спрашивает про серверы в этом же порядке.
var serverHelps = []ServerHelp{
	{
		Name:     "emby",
		Display:  "Emby",
		Settings: "EMBY_ENABLED, EMBY_URL, EMBY_API_KEY",
		Where:    "Emby dashboard, Advanced - API Keys",
	},
	{
		Name:     "jellyfin",
		Display:  "Jellyfin",
		Settings: "JELLYFIN_ENABLED, JELLYFIN_URL, JELLYFIN_API_KEY",
		Where:    "Jellyfin dashboard, API Keys",
	},
	{
		Name:     "plex",
		Display:  "Plex",
		Settings: "PLEX_ENABLED, PLEX_URL, PLEX_TOKEN, PLEX_SECTION_IDS",
		Where: "https://support.plex.tv/articles/204059436-" +
			"finding-an-authentication-token-x-plex-token/",
	},
}

// ServerHelps возвращает справку по всем серверам в порядке опроса.
func ServerHelps() []ServerHelp {
	out := make([]ServerHelp, len(serverHelps))
	copy(out, serverHelps)
	return out
}

// ServerHelpFor находит справку по имени сервера.
func ServerHelpFor(name string) (ServerHelp, bool) {
	for _, h := range serverHelps {
		if strings.EqualFold(h.Name, name) {
			return h, true
		}
	}
	return ServerHelp{}, false
}
