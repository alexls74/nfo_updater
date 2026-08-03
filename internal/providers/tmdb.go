// internal/providers/tmdb.go
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

const tmdbBaseURL = "https://api.themoviedb.org/3"

// TMDbProvider — единственный ключ, без пула (у TMDb нет сравнимого
// с OMDb/MDBList суточного лимита).
type TMDbProvider struct {
	apiKey string
	http   *http.Client
}

func NewTMDbProvider(apiKey string, httpClient *http.Client) *TMDbProvider {
	return &TMDbProvider{apiKey: apiKey, http: httpClient}
}

func (p *TMDbProvider) Name() string { return "tmdb" }

func (p *TMDbProvider) Supports(ids IDs, mediaType string) bool {
	return !ids.Empty()
}

func tmdbMediaPath(mediaType string) string {
	if mediaType == MediaTypeShow || mediaType == MediaTypeEpisode {
		return "tv"
	}
	return "movie"
}

type tmdbTitleResponse struct {
	ID          int     `json:"id"`
	VoteAverage float64 `json:"vote_average"`
	VoteCount   int     `json:"vote_count"`
}

// tmdbFindResponse — ответ /find поддерживает несколько external_source,
// включая tvdb_id (не только imdb_id).
type tmdbFindResponse struct {
	MovieResults []tmdbTitleResponse `json:"movie_results"`
	TVResults    []tmdbTitleResponse `json:"tv_results"`
}

// checkKey проверяет ключ через endpoint /authentication — он существует
// ровно для этого и ничего не расходует: суточной квоты у TMDb нет,
// ограничение только по частоте запросов с одного IP.
//
// Проверено живыми запросами: с v3-шным API Key в query-параметре endpoint
// отвечает 200 и {"success":true}, с негодным ключом — 401. Заголовок
// Authorization: Bearer, который показан в официальном примере, нужен
// только для API Read Access Token (авторизация v4); мы пользуемся v3,
// поэтому он неприменим.
func (p *TMDbProvider) checkKey(ctx context.Context) error {
	_, err := p.get(ctx, tmdbBaseURL+"/authentication")
	return err
}

func (p *TMDbProvider) FetchRatings(ctx context.Context, ids IDs, mediaType string) (FetchResult, error) {
	mediaPath := tmdbMediaPath(mediaType)

	var title tmdbTitleResponse
	var found bool

	switch {
	case ids.TMDb != "":
		url := fmt.Sprintf("%s/%s/%s", tmdbBaseURL, mediaPath, ids.TMDb)
		t, err := p.doRequest(ctx, url)
		if err != nil {
			return nil, err
		}
		title, found = t, t.ID != 0

	case ids.IMDb != "":
		title, found, err := p.findByExternalID(ctx, "imdb_id", ids.IMDb, mediaPath)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrNotFound
		}
		return toFetchResult(title), nil

	case ids.TVDb != "":
		title, found, err := p.findByExternalID(ctx, "tvdb_id", ids.TVDb, mediaPath)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrNotFound
		}
		return toFetchResult(title), nil

	default:
		return nil, ErrUnsupportedID
	}

	if !found || title.VoteAverage == 0 {
		return nil, ErrNotFound
	}
	return toFetchResult(title), nil
}

// findByExternalID ищет через /find по любому внешнему ID (imdb_id или tvdb_id).
func (p *TMDbProvider) findByExternalID(ctx context.Context, source, id, mediaPath string) (tmdbTitleResponse, bool, error) {
	url := fmt.Sprintf("%s/find/%s?external_source=%s", tmdbBaseURL, id, source)
	body, err := p.get(ctx, url)
	if err != nil {
		return tmdbTitleResponse{}, false, err
	}
	var data tmdbFindResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return tmdbTitleResponse{}, false, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("decode find response: %w", err)}
	}
	if mediaPath == "tv" && len(data.TVResults) > 0 {
		return data.TVResults[0], true, nil
	}
	if len(data.MovieResults) > 0 {
		return data.MovieResults[0], true, nil
	}
	if len(data.TVResults) > 0 {
		return data.TVResults[0], true, nil
	}
	return tmdbTitleResponse{}, false, nil
}

func toFetchResult(title tmdbTitleResponse) FetchResult {
	return FetchResult{
		"tmdb": RatingValue{
			Value: strconv.FormatFloat(title.VoteAverage, 'f', 1, 64),
			Votes: title.VoteCount,
		},
	}
}

func (p *TMDbProvider) doRequest(ctx context.Context, url string) (tmdbTitleResponse, error) {
	body, err := p.get(ctx, url)
	if err != nil {
		return tmdbTitleResponse{}, err
	}
	var t tmdbTitleResponse
	if err := json.Unmarshal(body, &t); err != nil {
		return tmdbTitleResponse{}, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("decode response: %w", err)}
	}
	return t, nil
}

func (p *TMDbProvider) get(ctx context.Context, url string) ([]byte, error) {
	sep := "?"
	if containsQuery(url) {
		sep = "&"
	}
	fullURL := fmt.Sprintf("%s%sapi_key=%s", url, sep, p.apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, &NetworkError{Provider: p.Name(), Err: err}
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, &NetworkError{Provider: p.Name(), Err: err}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		// У TMDb 401 означает именно негодный ключ: суточной квоты нет,
		// так что спутать с исчерпанным лимитом невозможно. Подтверждено
		// живым запросом с заведомо неверным ключом.
		return nil, &KeyError{
			Provider: p.Name(),
			KeyIndex: 0,
			Kind:     KeyInvalid,
			Detail:   "http status 401",
		}
	case resp.StatusCode == http.StatusTooManyRequests:
		// А вот 429 у TMDb — это мгновенный троттлинг по частоте, а не
		// суточный лимит. Ключ ни в чём не виноват, лечится повтором.
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("rate limited, http status %d", resp.StatusCode)}
	case resp.StatusCode >= 500:
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("http status %d", resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("unexpected http status %d", resp.StatusCode)}
	}

	return io.ReadAll(resp.Body)
}

func containsQuery(url string) bool {
	for _, c := range url {
		if c == '?' {
			return true
		}
	}
	return false
}

// FetchReleaseDate возвращает дату премьеры: release_date для фильма,
// first_air_date для сериала. Единственный источник этих данных в системе —
// фикс <premiered> не работает без TMDB_API_KEY.
func (p *TMDbProvider) FetchReleaseDate(ctx context.Context, ids IDs, mediaType string) (string, error) {
	mediaPath := tmdbMediaPath(mediaType)
	var tmdbID string

	switch {
	case ids.TMDb != "":
		tmdbID = ids.TMDb
	case ids.IMDb != "":
		title, found, err := p.findByExternalID(ctx, "imdb_id", ids.IMDb, mediaPath)
		if err != nil {
			return "", err
		}
		if !found {
			return "", ErrNotFound
		}
		tmdbID = strconv.Itoa(title.ID)
	case ids.TVDb != "":
		title, found, err := p.findByExternalID(ctx, "tvdb_id", ids.TVDb, mediaPath)
		if err != nil {
			return "", err
		}
		if !found {
			return "", ErrNotFound
		}
		tmdbID = strconv.Itoa(title.ID)
	default:
		return "", ErrUnsupportedID
	}

	url := fmt.Sprintf("%s/%s/%s", tmdbBaseURL, mediaPath, tmdbID)
	body, err := p.get(ctx, url)
	if err != nil {
		return "", err
	}
	var data struct {
		ReleaseDate  string `json:"release_date"`
		FirstAirDate string `json:"first_air_date"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", &NetworkError{Provider: p.Name(), Err: fmt.Errorf("decode release date: %w", err)}
	}
	if mediaPath == "tv" {
		if data.FirstAirDate == "" {
			return "", ErrNotFound
		}
		return data.FirstAirDate, nil
	}
	if data.ReleaseDate == "" {
		return "", ErrNotFound
	}
	return data.ReleaseDate, nil
}
