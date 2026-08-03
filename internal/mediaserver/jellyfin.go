// internal/mediaserver/jellyfin.go
package mediaserver

import "net/http"

// Jellyfin — форк Emby, и на сегодня оба нужных нам эндпоинта у него те же,
// поэтому работа делается общей реализацией embyLike (см. emby.go).
// Различие ровно одно — схема авторизации.
//
// Файл заведён отдельно намеренно, хотя кода в нём почти нет: проекты
// разошлись уже давно и продолжают расходиться, так что рано или поздно
// Jellyfin потребует собственных запросов. Когда это случится, менять
// придётся только этот файл, а Emby останется нетронутым.
func NewJellyfin(rawURL, apiKey string, httpClient *http.Client) Server {
	return &embyLike{
		name:    "jellyfin",
		baseURL: normalizeURL(rawURL),
		apiKey:  apiKey,
		// Заголовок Authorization — то, что документировано сейчас.
		// X-Emby-Token Jellyfin тоже принимает, но это наследие
		// совместимости, которое однажды уберут.
		setAuth: func(req *http.Request, key string) {
			req.Header.Set("Authorization", `MediaBrowser Token="`+key+`"`)
		},
		http: httpClient,
	}
}
