// internal/providers/providers.go
package providers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// IDs — набор известных идентификаторов тайтла. Не все поля обязательно
// заполнены. Провайдер сам решает, каким из непустых полей воспользоваться.
type IDs struct {
	IMDb string
	TMDb string
	TVDb string
}

func (ids IDs) Empty() bool {
	return ids.IMDb == "" && ids.TMDb == "" && ids.TVDb == ""
}

type RatingValue struct {
	Value string
	Votes int
}

type FetchResult map[string]RatingValue

const (
	MediaTypeMovie   = "movie"
	MediaTypeShow    = "show"
	MediaTypeEpisode = "episode"
)

type Provider interface {
	Name() string
	Supports(ids IDs, mediaType string) bool
	FetchRatings(ctx context.Context, ids IDs, mediaType string) (FetchResult, error)
}

// ErrNotFound — тайтла с таким ID у провайдера НЕТ вовсе. Почти всегда это
// значит, что ID в .nfo неверный: базы провайдеров пополняются из общих
// источников, и одновременное незнание тайтла всеми — редкое событие.
var ErrNotFound = errors.New("title not found by this provider")

// ErrNoRatings — тайтл у провайдера ЕСТЬ, но рейтингов у него нет ни одного.
// Обычная судьба свежих релизов, короткого метра и малоизвестных тайтлов.
//
// Отделено от ErrNotFound намеренно, и это не педантизм: два случая требуют
// от пользователя противоположных действий. При ErrNotFound чинить надо
// файл, при ErrNoRatings чинить нечего вовсе.
var ErrNoRatings = errors.New("title has no ratings at this provider")

var ErrUnsupportedID = errors.New("provider does not support the given ID combination")

// ErrQuotaExhausted означает, что у провайдера НЕ ОСТАЛОСЬ пригодных ключей:
// все либо выбрали суточный лимит, либо отвергнуты сервисом. Это ответ
// провайдера целиком, а не отдельного ключа, и он не отменяет прогон —
// вызывающий код откатывается на кэш.
var ErrQuotaExhausted = errors.New("provider quota exhausted for today")

// KeyErrorKind различает две ситуации, которые до сих пор сваливались
// в NetworkError и потому лечились одинаково неправильно — повторами
// и накруткой circuit breaker.
type KeyErrorKind int

const (
	// KeyInvalid — сервис не признал ключ. Повторять бессмысленно ни сейчас,
	// ни завтра: нужно вмешательство пользователя.
	KeyInvalid KeyErrorKind = iota
	// KeyExhausted — ключ рабочий, но его суточный лимит выбран. Завтра
	// он снова годен, поэтому ключ отключается только до конца суток.
	KeyExhausted
)

func (k KeyErrorKind) String() string {
	if k == KeyInvalid {
		return "invalid key"
	}
	return "daily limit reached"
}

// KeyError — сбой уровня ОТДЕЛЬНОГО ключа, а не провайдера и не сети.
// Правильная реакция на него — вывести этот ключ из оборота и немедленно
// повторить тот же запрос следующим, без задержки и без влияния на
// circuit breaker: сеть-то в порядке.
type KeyError struct {
	Provider string
	KeyIndex int
	Kind     KeyErrorKind
	Detail   string
}

func (e *KeyError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%s key #%d: %s", e.Provider, e.KeyIndex+1, e.Kind)
	}
	return fmt.Sprintf("%s key #%d: %s (%s)", e.Provider, e.KeyIndex+1, e.Kind, e.Detail)
}

// AsKeyError извлекает *KeyError из цепочки ошибок.
func AsKeyError(err error) (*KeyError, bool) {
	var ke *KeyError
	if errors.As(err, &ke) {
		return ke, true
	}
	return nil, false
}

// NetworkError — сбой транспорта или сервиса: соединение не установилось,
// сервер ответил 5xx, тело не разобралось. Только такие ошибки крутят
// circuit breaker.
type NetworkError struct {
	Provider string
	Err      error
}

func (e *NetworkError) Error() string {
	return fmt.Sprintf("%s: network error: %v", e.Provider, e.Err)
}

func (e *NetworkError) Unwrap() error { return e.Err }

func IsNetworkError(err error) bool {
	var ne *NetworkError
	return errors.As(err, &ne)
}

type CircuitBreaker struct {
	mu               sync.Mutex
	maxFailures      int
	backoff          time.Duration
	consecutiveFails map[string]int
	tripped          map[string]bool
}

func NewCircuitBreaker(maxFailures int, backoff time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		maxFailures:      maxFailures,
		backoff:          backoff,
		consecutiveFails: make(map[string]int),
		tripped:          make(map[string]bool),
	}
}

func (cb *CircuitBreaker) Allowed(provider string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return !cb.tripped[provider]
}

func (cb *CircuitBreaker) RecordSuccess(provider string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutiveFails[provider] = 0
}

func (cb *CircuitBreaker) RecordNetworkFailure(provider string) (tripped bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.consecutiveFails[provider]++
	if cb.consecutiveFails[provider] >= cb.maxFailures {
		cb.tripped[provider] = true
		return true
	}
	return false
}

func (cb *CircuitBreaker) Backoff() time.Duration {
	return cb.backoff
}

func (cb *CircuitBreaker) AllTripped(providerNames []string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	for _, name := range providerNames {
		if !cb.tripped[name] {
			return false
		}
	}
	return len(providerNames) > 0
}

// KnownGoodIMDbID — тайтл, заведомо существующий во всех базах. Нужен для
// проверки ключей OMDb, у которого нет отдельного endpoint'а валидации
// и единственный способ проверить ключ — сделать настоящий запрос.
const KnownGoodIMDbID = "tt0111161" // The Shawshank Redemption
