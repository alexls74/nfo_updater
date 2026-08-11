// internal/providers/omdb.go
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"nfo_updater/internal/logging"
)

const omdbURL = "https://www.omdbapi.com/"

// OMDbProvider — реализация Provider для OMDb. Ищет ТОЛЬКО по IMDb id —
// ни TMDb, ни TVDb id для OMDb не годятся ни при каких обстоятельствах.
type OMDbProvider struct {
	pool *keyPool
	http *http.Client
}

func NewOMDbProvider(keys []string, dailyLimit int, store usageStore, logger *logging.Logger, httpClient *http.Client) *OMDbProvider {
	return &OMDbProvider{
		pool: newKeyPool("omdb", keys, dailyLimit, store, logger),
		http: httpClient,
	}
}

func (p *OMDbProvider) Name() string { return "omdb" }

func (p *OMDbProvider) Supports(ids IDs, mediaType string) bool {
	return ids.IMDb != ""
}

// omdbMediaType переводит наш MediaType в значение параметра type=...
// у OMDb ("movie"/"series"/"episode").
func omdbMediaType(mediaType string) string {
	switch mediaType {
	case MediaTypeShow:
		return "series"
	case MediaTypeEpisode:
		return "episode"
	default:
		return "movie"
	}
}

type omdbResponse struct {
	Response   string `json:"Response"`
	Error      string `json:"Error"`
	ImdbRating string `json:"imdbRating"`
	ImdbVotes  string `json:"imdbVotes"`
	Ratings    []struct {
		Source string `json:"Source"`
		Value  string `json:"Value"`
	} `json:"Ratings"`
}

// FetchRatings перебирает ключи, пока запрос не удастся или пул не кончится.
// Отказ по ключу (невалиден / лимит выбран) не считается сетевым сбоем:
// ключ выводится из оборота, и тот же запрос немедленно повторяется
// следующим — без задержки и без влияния на circuit breaker.
func (p *OMDbProvider) FetchRatings(ctx context.Context, ids IDs, mediaType string) (FetchResult, error) {
	if ids.IMDb == "" {
		return nil, ErrUnsupportedID
	}

	for {
		key, keyIndex, err := p.pool.pick()
		if err != nil {
			return nil, err
		}

		res, err := p.fetchWithKey(ctx, key, keyIndex, ids.IMDb, omdbMediaType(mediaType))
		if ke, ok := AsKeyError(err); ok {
			p.pool.handleKeyError(ke)
			continue
		}
		return res, err
	}
}

// fetchWithKey делает один запрос конкретным ключом.
func (p *OMDbProvider) fetchWithKey(ctx context.Context, key string, keyIndex int, imdbID, omdbType string) (FetchResult, error) {
	data, err := p.get(ctx, key, keyIndex, map[string]string{
		"i":    imdbID,
		"type": omdbType,
	})
	if err != nil {
		return nil, err
	}

	out := make(FetchResult)

	if data.ImdbRating != "" && data.ImdbRating != "N/A" {
		votes := 0
		if v, err := strconv.Atoi(strings.ReplaceAll(data.ImdbVotes, ",", "")); err == nil {
			votes = v
		}
		out["imdb"] = RatingValue{Value: data.ImdbRating, Votes: votes}
	}

	for _, r := range data.Ratings {
		switch r.Source {
		case "Rotten Tomatoes":
			if v := strings.TrimSuffix(r.Value, "%"); v != r.Value {
				out["tomatoes"] = RatingValue{Value: v}
			}
		case "Metacritic":
			if v := strings.TrimSuffix(r.Value, "/100"); v != r.Value {
				out["metacritic"] = RatingValue{Value: v}
			}
		}
	}

	// Ответ разобран, тайтл у OMDb есть (иначе get вернул бы ErrNotFound),
	// но ни одной оценки в нём не оказалось — imdbRating пришёл как "N/A",
	// а блок Ratings пуст. Обычное дело для свежих релизов и короткого метра.
	if len(out) == 0 {
		return nil, ErrNoRatings
	}
	return out, nil
}

// get выполняет запрос и классифицирует ответ.
//
// Тело читается ДО реакции на статус по двум причинам. Первая: OMDb отдаёт
// код 401 И на невалидный ключ, И на исчерпанный лимит, различить их можно
// только по тексту. Вторая: сообщение об ошибке ключа приходит и в теле,
// поэтому на статус мы не полагаемся вовсе — если сервис однажды ответит
// тем же "Invalid API key!" под кодом 200, мы всё равно распознаем это как
// проблему ключа, а не как "тайтл не найден".
// Прогон разметил бы pending всю библиотеку при полностью живых данных.
func (p *OMDbProvider) get(ctx context.Context, key string, keyIndex int, params map[string]string) (*omdbResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, omdbURL, nil)
	if err != nil {
		return nil, &NetworkError{Provider: p.Name(), Err: err}
	}
	q := req.URL.Query()
	q.Set("apikey", key)
	for k, v := range params {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()

	resp, err := p.http.Do(req)
	if err != nil {
		// До сервиса не дошли — запрос не потрачен.
		return nil, &NetworkError{Provider: p.Name(), Err: err}
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("read response: %w", readErr)}
	}

	var data omdbResponse
	// Ошибку разбора здесь не поднимаем: при 5xx в теле вполне может лежать
	// html-заглушка, и осмысленным ответом будет статус, а не тело.
	_ = json.Unmarshal(body, &data)

	// Проблема ключа — раньше всего остального, независимо от кода ответа.
	if data.Response == "False" && isOMDbKeyMessage(data.Error) {
		return nil, &KeyError{
			Provider: p.Name(),
			KeyIndex: keyIndex,
			Kind:     omdbKeyErrorKind(data.Error),
			Detail:   data.Error,
		}
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// 401 без распознаваемого текста в теле. Считаем ключ негодным:
		// он выбывает только до конца прогона, что безопаснее фиктивной
		// отметки об исчерпанной квоте на все сутки.
		return nil, &KeyError{
			Provider: p.Name(),
			KeyIndex: keyIndex,
			Kind:     KeyInvalid,
			Detail:   data.Error,
		}
	case resp.StatusCode >= 500:
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("http status %d", resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("unexpected http status %d", resp.StatusCode)}
	}

	// Сюда дошли только при 200 без ошибки ключа: сервис обслужил запрос.
	p.pool.noteRequest(keyIndex)

	if data.Response == "" {
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("malformed response body")}
	}
	if data.Response != "True" {
		// Обычное "Movie not found!" — тайтла нет, ключ в порядке.
		return nil, ErrNotFound
	}
	return &data, nil
}

// isOMDbKeyMessage отличает сообщения о проблеме с ключом от обычного
// "Movie not found!". Известные тексты: "Invalid API key!" (подтверждён
// живым запросом) и "Request limit reached!".
func isOMDbKeyMessage(message string) bool {
	m := strings.ToLower(message)
	return strings.Contains(m, "key") || strings.Contains(m, "limit")
}

// omdbKeyErrorKind разбирает текст ошибки ключа: упоминание лимита означает,
// что ключ рабочий, но исчерпан; всё остальное — невалидный ключ.
func omdbKeyErrorKind(message string) KeyErrorKind {
	if strings.Contains(strings.ToLower(message), "limit") {
		return KeyExhausted
	}
	return KeyInvalid
}
