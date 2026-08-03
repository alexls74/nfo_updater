// internal/mediaserver/emby.go
package mediaserver

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// embyLike — общая реализация для Emby и Jellyfin.
//
// Jellyfin — форк Emby, и эндпоинты, которые нужны нам, у него сегодня те же:
// GET /System/Info и POST /Library/Refresh. Расходится только способ передать
// ключ, поэтому он вынесен в поле-функцию. Конструктор Jellyfin живёт
// в jellyfin.go: форки со временем расходятся, и когда это случится, его
// реализации будет куда расти, не трогая Emby.
type embyLike struct {
	name    string
	baseURL string
	apiKey  string
	// setAuth ставит заголовок авторизации так, как его ждёт конкретный сервер
	setAuth func(req *http.Request, key string)
	http    *http.Client
}

// NewEmby: авторизация заголовком X-Emby-Token.
//
// Ключ намеренно НЕ передаётся query-параметром api_key, хотя Emby это
// поддерживает: полный URL с ключом попадал бы в лог сервера и в наши
// собственные сообщения об ошибках.
func NewEmby(rawURL, apiKey string, httpClient *http.Client) Server {
	return &embyLike{
		name:    "emby",
		baseURL: normalizeURL(rawURL),
		apiKey:  apiKey,
		setAuth: func(req *http.Request, key string) {
			req.Header.Set("X-Emby-Token", key)
		},
		http: httpClient,
	}
}

func (s *embyLike) Name() string { return s.name }

// Ping: /System/Info требует авторизации, поэтому одним запросом проверяются
// сразу и доступность сервера, и годность ключа.
func (s *embyLike) Ping(ctx context.Context) error {
	return s.do(ctx, http.MethodGet, "/System/Info")
}

// Refresh запускает сканирование всех библиотек сервера. Ответ — 204,
// сканирование идёт асинхронно: сервер только принимает задание.
func (s *embyLike) Refresh(ctx context.Context) error {
	return s.do(ctx, http.MethodPost, "/Library/Refresh")
}

func (s *embyLike) do(ctx context.Context, method, path string) error {
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", s.baseURL+path, err)
	}
	s.setAuth(req, s.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", s.baseURL, err)
	}
	defer resp.Body.Close()
	// Тело нам не нужно ни в одном из двух запросов, но дочитать его стоит:
	// иначе соединение не вернётся в пул keep-alive.
	_, _ = io.Copy(io.Discard, resp.Body)

	return checkStatus(resp.StatusCode)
}
