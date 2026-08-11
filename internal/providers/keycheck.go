// internal/providers/keycheck.go
package providers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"nfo_updater/internal/logging"
)

// KeyStatus — результат проверки одного ключа.
type KeyStatus struct {
	Provider string
	KeyIndex int
	OK       bool
	// Err при OK == false: *KeyError (ключ невалиден или лимит исчерпан)
	// либо *NetworkError (сервис не ответил). Различать их обязательно:
	// первое требует вмешательства пользователя, второе пройдёт само.
	Err error
}

// KeyChecker умеет проверить ВСЕ свои ключи. Реализуют OMDb, MDBList и TMDb.
//
// Проверка обходит именно весь пул, а не один ключ: раньше preflight дёргал
// pickKey() ровно один раз, и протухший второй-третий ключ обнаруживался
// только в середине прогона, когда до него доходила ротация.
type KeyChecker interface {
	Name() string
	CheckKeys(ctx context.Context) []KeyStatus
}

// CheckAll последовательно проверяет ключи всех переданных провайдеров.
// Реализации CheckKeys сами выводят негодные ключи из оборота, так что
// после этого вызова прогон уже не будет пытаться ими пользоваться.
func CheckAll(ctx context.Context, checkers []KeyChecker) []KeyStatus {
	var out []KeyStatus
	for _, c := range checkers {
		out = append(out, c.CheckKeys(ctx)...)
	}
	return out
}

// checkPoolKeys — общая реализация для провайдеров с пулом ключей.
// probe делает минимально возможный запрос конкретным ключом.
func checkPoolKeys(pool *keyPool, probe func(key string, index int) error) []KeyStatus {
	out := make([]KeyStatus, 0, pool.size())
	for i := 0; i < pool.size(); i++ {
		err := probe(pool.keyAt(i), i)
		if err == nil {
			out = append(out, KeyStatus{Provider: pool.provider, KeyIndex: i, OK: true})
			continue
		}
		// Негодный ключ сразу выбывает: прогон не должен натыкаться
		// на него повторно.
		if ke, ok := AsKeyError(err); ok {
			pool.handleKeyError(ke)
		}
		out = append(out, KeyStatus{Provider: pool.provider, KeyIndex: i, OK: false, Err: err})
	}
	return out
}

// CheckKeys для OMDb: отдельного endpoint'а валидации у сервиса нет,
// поэтому каждый ключ проверяется НАСТОЯЩИМ запросом по заведомо
// существующему тайтлу. Стоит по одному запросу на ключ.
func (p *OMDbProvider) CheckKeys(ctx context.Context) []KeyStatus {
	return checkPoolKeys(p.pool, func(key string, index int) error {
		_, err := p.get(ctx, key, index, map[string]string{
			"i":    KnownGoodIMDbID,
			"type": "movie",
		})
		return err
	})
}

// CheckKeys для MDBList: используется endpoint /user, который отдаёт
// сведения о самом ключе, а не данные тайтла.
func (p *MDBListProvider) CheckKeys(ctx context.Context) []KeyStatus {
	return checkPoolKeys(p.pool, func(key string, index int) error {
		return p.checkKey(ctx, key, index)
	})
}

// CheckKeys для TMDb: ключ один, пула нет, endpoint /authentication
// бесплатен (суточной квоты у TMDb не существует).
func (p *TMDbProvider) CheckKeys(ctx context.Context) []KeyStatus {
	if p.apiKey == "" {
		return nil
	}
	if err := p.checkKey(ctx); err != nil {
		return []KeyStatus{{Provider: p.Name(), KeyIndex: 0, OK: false, Err: err}}
	}
	return []KeyStatus{{Provider: p.Name(), KeyIndex: 0, OK: true}}
}

// ---------------------------------------------------------------------------
// Проверка одного ключа
//
// Мастер настройки проверяет ключи по одному, сразу после ввода, а не все
// разом в конце. Причина не в удобстве: при пакетной проверке единственный
// неверный ключ отправлял человека вводить заново ВЕСЬ набор, и каждый
// повторный проход заново тратил по запросу квоты на те ключи OMDb, которые
// уже были признаны годными.
// ---------------------------------------------------------------------------

// KeyVerdict — исход проверки одного ключа.
type KeyVerdict int

const (
	// KeyGood — сервис ключ принял.
	KeyGood KeyVerdict = iota
	// KeyRejected — сервис ключ не признал либо его квота исчерпана.
	// Чинится вводом другого ключа.
	KeyRejected
	// KeyUnreachable — сервис не ответил. О ключе это не говорит ничего,
	// и предлагать в таком случае перенабрать его было бы вредным советом.
	KeyUnreachable
)

// KeyCheck — что выяснилось про один ключ.
type KeyCheck struct {
	Verdict KeyVerdict

	// Reason — короткая причина без имени сервиса и номера ключа.
	Reason string

	// Requests — сколько запросов суточной квоты израсходовала проверка.
	//
	// Ненулевым бывает только у OMDb и только при KeyGood. Отдельного
	// endpoint'а валидации у сервиса нет, поэтому ключ проверяется настоящим
	// запросом — но квота привязана К КЛЮЧУ, и отвергнутый ключ не тратит
	// ничью: с точки зрения сервиса такого ключа не существует. У MDBList
	// (endpoint /user) и TMDb (/authentication) проверка бесплатна.
	Requests int
}

// CheckKey проверяет один ключ одного сервиса.
//
// Ошибку возвращает только на неизвестное имя провайдера — это ошибка в коде,
// а не в настройке. Всё остальное описывается вердиктом.
func CheckKey(ctx context.Context, provider, key string, client *http.Client) (KeyCheck, error) {
	silent := logging.New(nil, nil, false)
	store := noUsageStore{}

	var checker KeyChecker
	switch strings.ToLower(provider) {
	case "omdb":
		checker = NewOMDbProvider([]string{key}, keyCheckDailyLimit, store, silent, client)
	case "mdblist":
		checker = NewMDBListProvider([]string{key}, keyCheckDailyLimit, store, silent, client)
	case "tmdb":
		checker = NewTMDbProvider(key, client)
	default:
		return KeyCheck{}, fmt.Errorf("internal error: no key checker for provider %q", provider)
	}

	statuses := checker.CheckKeys(ctx)
	if len(statuses) == 0 {
		// Пустой ключ до сюда не доходит — мастер его не принимает, — но
		// молча возвращать «годен» на пустой результат нельзя.
		return KeyCheck{Verdict: KeyRejected, Reason: "empty key"}, nil
	}

	st := statuses[0]
	if st.OK {
		requests := 0
		if strings.EqualFold(provider, "omdb") {
			requests = 1
		}
		return KeyCheck{Verdict: KeyGood, Requests: requests}, nil
	}

	if ke, ok := AsKeyError(st.Err); ok {
		// Детали вроде "(http status 403)" отбрасываются: вид ошибки уже
		// сказал всё, что человеку нужно для действия.
		return KeyCheck{Verdict: KeyRejected, Reason: ke.Kind.String()}, nil
	}
	if IsNetworkError(st.Err) {
		return KeyCheck{Verdict: KeyUnreachable, Reason: "the service did not answer"}, nil
	}
	// Неожиданное: текст оставляем как есть, это единственная зацепка.
	return KeyCheck{Verdict: KeyUnreachable, Reason: st.Err.Error()}, nil
}
