// internal/providers/mdblist_user.go
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// UserInfo — ответ endpoint'а /user: сведения о самом ключе.
//
// APIRequests здесь — лимит, назначенный СЕРВИСОМ (у патронов он выше
// бесплатной тысячи), APIRequestsCount — расход по данным сервиса.
// Мы этими числами пока не управляем поведением: MDBLIST_DAILY_LIMIT_PER_KEY
// остаётся самоограничением пользователя, потому что ключом могут
// пользоваться и другие инструменты, и оставить себе запас — его право.
// Но в лог их вывести полезно: расхождение с нашим счётчиком сразу видно.
type UserInfo struct {
	APIRequests      int    `json:"api_requests"`
	APIRequestsCount int    `json:"api_requests_count"`
	UserID           int    `json:"user_id"`
	PatronStatus     string `json:"patron_status"`
}

// checkKey проверяет один ключ MDBList через /user.
//
// Вызов /user НЕ расходует суточную квоту — проверено живыми запросами:
// два обращения подряд оставили api_requests_count нулевым, а один
// обычный запрос данных сразу после них дал +1. Поэтому noteRequest
// здесь намеренно не вызывается: проверка всех ключей MDBList перед
// каждым прогоном бесплатна.
func (p *MDBListProvider) checkKey(ctx context.Context, key string, index int) error {
	_, err := p.fetchUserInfo(ctx, key, index)
	return err
}

func (p *MDBListProvider) fetchUserInfo(ctx context.Context, key string, index int) (*UserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mdblistBaseURL+"/user", nil)
	if err != nil {
		return nil, &NetworkError{Provider: p.Name(), Err: err}
	}
	q := req.URL.Query()
	q.Set("apikey", key)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, &NetworkError{Provider: p.Name(), Err: err}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, &KeyError{
			Provider: p.Name(),
			KeyIndex: index,
			Kind:     KeyInvalid,
			Detail:   fmt.Sprintf("http status %d", resp.StatusCode),
		}
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &KeyError{
			Provider: p.Name(),
			KeyIndex: index,
			Kind:     KeyExhausted,
			Detail:   "http status 429",
		}
	case resp.StatusCode != http.StatusOK:
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("http status %d", resp.StatusCode)}
	}

	var info UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, &NetworkError{Provider: p.Name(), Err: fmt.Errorf("decode user info: %w", err)}
	}
	return &info, nil
}
