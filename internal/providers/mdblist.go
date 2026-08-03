// internal/providers/mdblist.go
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"nfo_updater/internal/logging"
)

// mdblistBaseURL — реальный рабочий путь: https://api.mdblist.com/{provider}/{mediatype}/{id}/
// (подтверждено живыми запросами, включая tvdb как provider).
const mdblistBaseURL = "https://api.mdblist.com"

type MDBListProvider struct {
	pool *keyPool
	http *http.Client
}

func NewMDBListProvider(keys []string, dailyLimit int, store usageStore, logger *logging.Logger, httpClient *http.Client) *MDBListProvider {
	return &MDBListProvider{
		pool: newKeyPool("mdblist", keys, dailyLimit, store, logger),
		http: httpClient,
	}
}

func (p *MDBListProvider) Name() string { return "mdblist" }

func (p *MDBListProvider) Supports(ids IDs, mediaType string) bool {
	return !ids.Empty()
}

// pickID выбирает, каким ID пользоваться, по приоритету: IMDb всегда первый.
// Дальше — для сериалов/серий приоритет TVDb выше TMDb, для фильмов наоборот.
func pickID(ids IDs, mediaType string) (provider, id string) {
	if ids.IMDb != "" {
		return "imdb", ids.IMDb
	}
	if mediaType == MediaTypeShow || mediaType == MediaTypeEpisode {
		if ids.TVDb != "" {
			return "tvdb", ids.TVDb
		}
		if ids.TMDb != "" {
			return "tmdb", ids.TMDb
		}
	} else {
		if ids.TMDb != "" {
			return "tmdb", ids.TMDb
		}
		if ids.TVDb != "" {
			return "tvdb", ids.TVDb
		}
	}
	return "", ""
}

type ratingMeta struct {
	OurSource string
	Divide10  bool
}

var sourceMetaMap = map[string]ratingMeta{
	"imdb":       {"imdb", true},
	"tmdb":       {"tmdb", true},
	"trakt":      {"trakt", true},
	"tomatoes":   {"tomatoes", false},
	"popcorn":    {"popcorn", false},
	"metacritic": {"metacritic", false},
}

type mdblistEpisode struct {
	EpisodeNumber int  `json:"episode_number"`
	Rating        *int `json:"rating"`
	Votes         *int `json:"votes"`
}

type mdblistSeason struct {
	SeasonNumber int              `json:"season_number"`
	Episodes     []mdblistEpisode `json:"episodes"`
}

type mdblistResponse struct {
	Error   string `json:"error"`
	Ratings []struct {
		Source string   `json:"source"`
		Score  *float64 `json:"score"`
		Votes  *int     `json:"votes"`
	} `json:"ratings"`
	Seasons []mdblistSeason `json:"seasons"`
}

func (p *MDBListProvider) FetchRatings(ctx context.Context, ids IDs, mediaType string) (FetchResult, error) {
	idProvider, id := pickID(ids, mediaType)
	if id == "" {
		return nil, ErrUnsupportedID
	}
	if mediaType == MediaTypeEpisode {
		// Отдельных запросов по сериям MDBList не обслуживает: рейтинги
		// серий приходят внутри ответа по сериалу целиком.
		return nil, ErrUnsupportedID
	}
	mdbMediaType := mediaType
	if mdbMediaType == "" {
		mdbMediaType = MediaTypeMovie
	}

	url := fmt.Sprintf("%s/%s/%s/%s/", mdblistBaseURL, idProvider, mdbMediaType, id)
	resp, err := p.getWithRetry(ctx, url)
	if err != nil {
		return nil, err
	}

	out := make(FetchResult)
	for _, r := range resp.Ratings {
		meta, known := sourceMetaMap[r.Source]
		if !known || r.Score == nil {
			continue
		}
		score := *r.Score
		if meta.Divide10 {
			score /= 10
		}
		votes := 0
		if r.Votes != nil {
			votes = *r.Votes
		}
		out[meta.OurSource] = RatingValue{
			Value: strconv.FormatFloat(score, 'f', 1, 64),
			Votes: votes,
		}
	}

	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

type EpisodeRating struct {
	Value string
	Votes int
}

// FetchShowWithEpisodes делает ОДИН запрос на сериал целиком и возвращает
// и его собственный рейтинг, и таблицу рейтингов всех серий, извлечённую
// из того же ответа. Ключ episodes — "season:episode".
func (p *MDBListProvider) FetchShowWithEpisodes(ctx context.Context, ids IDs) (show FetchResult, episodes map[string]EpisodeRating, err error) {
	idProvider, id := pickID(ids, MediaTypeShow)
	if id == "" {
		return nil, nil, ErrUnsupportedID
	}

	url := fmt.Sprintf("%s/%s/%s/%s/", mdblistBaseURL, idProvider, MediaTypeShow, id)
	resp, err := p.getWithRetry(ctx, url)
	if err != nil {
		return nil, nil, err
	}

	show = make(FetchResult)
	for _, r := range resp.Ratings {
		meta, known := sourceMetaMap[r.Source]
		if !known || r.Score == nil {
			continue
		}
		score := *r.Score
		if meta.Divide10 {
			score /= 10
		}
		votes := 0
		if r.Votes != nil {
			votes = *r.Votes
		}
		show[meta.OurSource] = RatingValue{Value: strconv.FormatFloat(score, 'f', 1, 64), Votes: votes}
	}

	episodes = make(map[string]EpisodeRating)
	for _, season := range resp.Seasons {
		for _, ep := range season.Episodes {
			if ep.Rating == nil {
				continue
			}
			votes := 0
			if ep.Votes != nil {
				votes = *ep.Votes
			}
			key := fmt.Sprintf("%d:%d", season.SeasonNumber, ep.EpisodeNumber)
			episodes[key] = EpisodeRating{
				Value: strconv.FormatFloat(float64(*ep.Rating)/10, 'f', 1, 64),
				Votes: votes,
			}
		}
	}

	if len(show) == 0 && len(episodes) == 0 {
		return nil, nil, ErrNotFound
	}
	return show, episodes, nil
}

// getWithRetry перебирает ключи, пока запрос не удастся или пул не кончится.
func (p *MDBListProvider) getWithRetry(ctx context.Context, url string) (*mdblistResponse, error) {
	for {
		key, keyIndex, err := p.pool.pick()
		if err != nil {
			return nil, err
		}

		resp, err := p.doGet(ctx, url, key, keyIndex)
		if ke, ok := AsKeyError(err); ok {
			p.pool.handleKeyError(ke)
			continue
		}
		return resp, err
	}
}

func (p *MDBListProvider) doGet(ctx context.Context, url, key string, keyIndex int) (*mdblistResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &NetworkError{Provider: p.Name(), Err: err}
	}
	q := req.URL.Query()
	q.Set("apikey", key)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		// До сервиса не дошли — запрос не потрачен.
		return nil, &NetworkError{Provider: p.Name(), Err: err}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, &KeyError{
			Provider: p.Name(),
			KeyIndex: keyIndex,
			Kind:     KeyInvalid,
			Detail:   fmt.Sprintf("http status %d", resp.StatusCode),
		}
	case resp.StatusCode == http.StatusTooManyRequests:
		// У MDBList 429 означает именно исчерпанный СУТОЧНЫЙ лимит ключа,
		// а не мгновенный троттлинг, поэтому ключ выбывает до конца суток.
		return nil, &KeyError{
			Provider: p.Name(),
			KeyIndex: keyIndex,
			Kind:     KeyExhausted,
			Detail:   "http status 429",
		}
	case resp.StatusCode == http.StatusNotFound:
		p.pool.noteRequest(keyIndex)
		return nil, ErrNotFound
	case resp.StatusCode >= 500:
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("http status %d", resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("unexpected http status %d", resp.StatusCode)}
	}

	p.pool.noteRequest(keyIndex)

	var data mdblistResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("decode response: %w", err)}
	}
	return &data, nil
}
