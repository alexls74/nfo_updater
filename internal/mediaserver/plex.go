// internal/mediaserver/plex.go
package mediaserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Общего «обнови всё» у Plex нет: сканирование запускается посекционно,
// GET /library/sections/{id}/refresh. Поэтому секции либо перечислены
// в конфиге, либо мы спрашиваем их у самого сервера.
//
// ВНИМАНИЕ: код написан по документации API и на живом Plex не проверялся —
// у автора нет доступа к серверу. Эндпоинты простые и стабильные много лет,
// но если что-то пойдёт не так, начинать разбор стоит отсюда.
type plexServer struct {
	baseURL    string
	token      string
	sectionIDs []string
	http       *http.Client
}

func NewPlex(rawURL, token string, sectionIDs []string, httpClient *http.Client) Server {
	return &plexServer{
		baseURL:    normalizeURL(rawURL),
		token:      token,
		sectionIDs: sectionIDs,
		http:       httpClient,
	}
}

func (p *plexServer) Name() string { return "plex" }

// Ping запрашивает список секций, а не /identity: /identity отвечает и без
// токена, то есть проверял бы только доступность сервера. Список секций
// требует авторизации — значит, заодно проверяется и токен.
func (p *plexServer) Ping(ctx context.Context) error {
	_, err := p.librarySections(ctx)
	return err
}

// Refresh запускает сканирование каждой секции по отдельности.
//
// Ошибка на одной секции не отменяет остальные: секцию могли удалить,
// а ID остаться в конфиге — это не повод не обновить всё прочее.
func (p *plexServer) Refresh(ctx context.Context) error {
	ids := p.sectionIDs
	if len(ids) == 0 {
		sections, err := p.librarySections(ctx)
		if err != nil {
			return err
		}
		ids = mediaSectionIDs(sections)
		if len(ids) == 0 {
			return errors.New("server reports no movie or TV show sections")
		}
	}

	var failures []string
	for _, id := range ids {
		if err := p.get(ctx, "/library/sections/"+id+"/refresh", nil); err != nil {
			failures = append(failures, fmt.Sprintf("section %s: %v", id, err))
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// plexSection — одна библиотека Plex. key — это и есть её числовой ID,
// который идёт в URL сканирования.
type plexSection struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

type plexSectionsResponse struct {
	MediaContainer struct {
		Directory []plexSection `json:"Directory"`
	} `json:"MediaContainer"`
}

func (p *plexServer) librarySections(ctx context.Context) ([]plexSection, error) {
	var data plexSectionsResponse
	if err := p.get(ctx, "/library/sections", &data); err != nil {
		return nil, err
	}
	return data.MediaContainer.Directory, nil
}

// mediaSectionIDs отбирает только секции фильмов и сериалов.
//
// Когда PLEX_SECTION_IDS не заполнен, мы обновляем библиотеку сами, и гонять
// сканирование по музыке и фотографиям, которых наши .nfo не касаются,
// незачем. Если пользователю нужно иначе — он перечислит ID явно.
func mediaSectionIDs(sections []plexSection) []string {
	var out []string
	for _, s := range sections {
		if s.Type == "movie" || s.Type == "show" {
			out = append(out, s.Key)
		}
	}
	return out
}

// get выполняет запрос к Plex. Если out не nil — разбирает тело как JSON.
func (p *plexServer) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", p.baseURL+path, err)
	}
	// Токен заголовком, а не в query: тот же довод, что и у Emby — полный
	// URL с токеном не должен попадать в логи.
	req.Header.Set("X-Plex-Token", p.token)
	// Без этого заголовка Plex отвечает XML.
	req.Header.Set("Accept", "application/json")
	// Plex ожидает, что клиент представится. Не обязательно для запросов
	// с токеном, но так наши обращения различимы в его собственных логах.
	req.Header.Set("X-Plex-Product", "nfo_updater")
	req.Header.Set("X-Plex-Client-Identifier", "nfo_updater")

	resp, err := p.http.Do(req)
	if err != nil {
		return requestError(p.baseURL, err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp.StatusCode); err != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return err
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
