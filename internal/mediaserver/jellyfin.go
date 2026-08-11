// internal/mediaserver/jellyfin.go
package mediaserver

import "net/http"

// Jellyfin — форк Emby.
// Различие ровно одно — схема авторизации.
//
// Файл заведён отдельно намеренно.
func NewJellyfin(rawURL, apiKey string, httpClient *http.Client) Server {
	return &embyLike{
		name:    "jellyfin",
		baseURL: normalizeURL(rawURL),
		apiKey:  apiKey,
		// Заголовок Authorization — то, что документировано сейчас.
		// X-Emby-Token Jellyfin тоже принимает, но это наследие
		// совместимости, которое однажды могут убрать.
		setAuth: func(req *http.Request, key string) {
			req.Header.Set("Authorization", `MediaBrowser Token="`+key+`"`)
		},
		http: httpClient,
	}
}
