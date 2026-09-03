// internal/providers/providers.go
package providers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
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

// NetworkErrorKind различает сбои, которые до сих пор сваливались в один
// тип и потому лечились одинаково. Разница между ними ровно одна и она
// практическая: имеет ли смысл повторить тот же запрос.
type NetworkErrorKind int

const (
	// NetTransport — до сервиса не дошли вовсе: не разрешилось имя,
	// не установилось соединение, не состоялось рукопожатие, оборвался
	// ответ. Запрос сервисом не обслужен и из квоты не списан, поэтому
	// повтор бесплатен.
	NetTransport NetworkErrorKind = iota

	// NetServerFault — сервис ответил 5xx или отказал по частоте. Тоже
	// не списывается с квоты и тоже лечится повтором.
	NetServerFault

	// NetBadResponse — ответ получен и он осмыслен как ответ, но негоден:
	// неожиданный код, нечитаемое или неразбираемое тело. Повторять
	// НЕЛЬЗЯ: разбор тела происходит уже ПОСЛЕ noteRequest, то есть
	// запрос списан с суточного лимита, и повтор списал бы второй,
	// чтобы почти наверняка получить тот же негодный ответ.
	NetBadResponse
)

// NetworkError — сбой транспорта или сервиса. Только такие ошибки крутят
// circuit breaker.
//
// KeyIndex — номер ключа (с нуля), которым делался запрос. Нужен в логе:
// без него нельзя отличить "сдох один ключ из трёх" от "сдохла сеть".
// У провайдеров с единственным ключом всегда 0.
type NetworkError struct {
	Provider string
	KeyIndex int
	Kind     NetworkErrorKind
	Err      error
}

// newNetworkError — ЕДИНСТВЕННЫЙ способ завести NetworkError внутри пакета.
// Литералом эту структуру собирать нельзя, и причина серьёзнее вкусовой:
// здесь из ошибки вычищаются ключи API. Ошибка от http.Client.Do — это
// *url.Error, а он печатает URL целиком, вместе с query-строкой, в которой
// лежит apikey. Любой пропущенный вызов означает ключ открытым текстом
// в файле лога и в journald.
func newNetworkError(provider string, keyIndex int, kind NetworkErrorKind, err error) *NetworkError {
	return &NetworkError{
		Provider: provider,
		KeyIndex: keyIndex,
		Kind:     kind,
		Err:      scrubSecrets(err),
	}
}

func (e *NetworkError) Error() string {
	return fmt.Sprintf("%s key #%d: network error: %v", e.Provider, e.KeyIndex+1, e.Err)
}

func (e *NetworkError) Unwrap() error { return e.Err }

// Retryable сообщает, имеет ли смысл повторить тот же запрос. Решение
// живёт здесь, а не у вызывающего: только пакет providers знает, был ли
// запрос уже списан с квоты.
func (e *NetworkError) Retryable() bool {
	return e.Kind == NetTransport || e.Kind == NetServerFault
}

func IsNetworkError(err error) bool {
	var ne *NetworkError
	return errors.As(err, &ne)
}

// AsNetworkError извлекает *NetworkError из цепочки ошибок.
func AsNetworkError(err error) (*NetworkError, bool) {
	var ne *NetworkError
	if errors.As(err, &ne) {
		return ne, true
	}
	return nil, false
}

// secretParams — имена query-параметров, значение которых нельзя печатать.
// MDBList и OMDb называют его apikey, TMDb — api_key.
var secretParams = []string{"apikey", "api_key"}

// secretPlaceholder — то, что встаёт на место ключа. Слово из одних букв
// выбрано не случайно: url.Values.Encode() процентно кодирует всё
// небуквенное, и заполнитель вроде "***" превратился бы в логе
// в нечитаемое "%2A%2A%2A".
const secretPlaceholder = "redacted"

// scrubSecrets возвращает ошибку, безопасную для печати.
//
// Разбирается только верхний уровень цепочки, и это не упрощение:
// http.Client.Do всегда возвращает *url.Error именно верхним уровнем.
// Искать *url.Error глубже через errors.As было бы хуже — пришлось бы
// вернуть найденное звено, потеряв всё, что его оборачивало.
func scrubSecrets(err error) error {
	ue, ok := err.(*url.Error)
	if !ok {
		return err
	}
	// Копия: чужую ошибку на месте не правим.
	clean := *ue
	clean.URL = scrubURL(ue.URL)
	return &clean
}

func scrubURL(raw string) string {
	u, parseErr := url.Parse(raw)
	if parseErr != nil {
		// Разобрать не смогли. Режем query целиком: потерять подробности
		// не жалко, напечатать ключ — нельзя.
		if i := strings.IndexByte(raw, '?'); i >= 0 {
			return raw[:i] + "?<redacted>"
		}
		return raw
	}

	q := u.Query()
	hidden := false
	for _, name := range secretParams {
		if q.Has(name) {
			q.Set(name, secretPlaceholder)
			hidden = true
		}
	}
	if !hidden {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// CircuitBreaker гасит провайдера после нескольких сетевых сбоев подряд
// и — это важно — сам же возвращает его в строй.
//
// Раньше отключение было пожизненным в пределах прогона: карта tripped
// выставлялась и никогда не очищалась. Расплата за это несоразмерна
// причине. Сорокасекундный обрыв связи в начале прохода гасил MDBList
// на оставшиеся одиннадцать минут и тысячу с лишним фильмов, которые
// затем сваливались на OMDb с его куда более скромной суточной квотой,
// а после её исчерпания — на кэш. То есть кратковременная неисправность
// сети превращалась в потерю свежих данных по всей медиатеке.
//
// Отсюда полуоткрытое состояние: отключение помнит СВОЁ ВРЕМЯ, и по
// истечении probeAfter один следующий запрос пропускается как пробный.
// Ответил — провайдер снова в работе, не ответил — новый отсчёт.
type CircuitBreaker struct {
	mu          sync.Mutex
	maxFailures int
	backoff     time.Duration
	probeAfter  time.Duration

	// now подменяется в тестах; в работе это всегда time.Now.
	now func() time.Time

	consecutiveFails map[string]int

	// trippedAt — момент последнего отключения. Отсутствие записи
	// означает, что провайдер в работе.
	trippedAt map[string]time.Time
}

func NewCircuitBreaker(maxFailures int, backoff, probeAfter time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		maxFailures:      maxFailures,
		backoff:          backoff,
		probeAfter:       probeAfter,
		now:              time.Now,
		consecutiveFails: make(map[string]int),
		trippedAt:        make(map[string]time.Time),
	}
}

// Allowed сообщает, можно ли сейчас обращаться к провайдеру.
//
// ВНИМАНИЕ: у метода есть побочный эффект. Пропуская пробный запрос,
// он тут же сдвигает отсчёт, чтобы следующая проба ушла не раньше чем
// через probeAfter. Без этого отключённый провайдер после истечения
// паузы получал бы пробу на каждом тайтле подряд, то есть отключение
// перестало бы что-либо значить.
func (cb *CircuitBreaker) Allowed(provider string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	at, tripped := cb.trippedAt[provider]
	if !tripped {
		return true
	}
	now := cb.now()
	if now.Sub(at) < cb.probeAfter {
		return false
	}
	cb.trippedAt[provider] = now
	return true
}

// RecordSuccess отмечает содержательный ответ провайдера. Возвращает true,
// если этим ответом провайдер вернулся из отключённого состояния, — только
// чтобы вызывающий написал об этом в лог ровно один раз.
func (cb *CircuitBreaker) RecordSuccess(provider string) (recovered bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFails[provider] = 0
	if _, tripped := cb.trippedAt[provider]; tripped {
		delete(cb.trippedAt, provider)
		return true
	}
	return false
}

// RecordNetworkFailure засчитывает ОДИН неудавшийся тайтл — не одну
// неудавшуюся попытку. Повторы внутри одного обращения сюда не попадают,
// иначе порог maxFailures срабатывал бы вдвое раньше и настройка
// CIRCUIT_BREAKER_FAILURES перестала бы означать то, что написано в её имени.
//
// Возвращает true только в момент ПЕРЕХОДА в отключённое состояние.
// Неудачная проба уже отключённого провайдера возвращает false: она лишь
// начинает отсчёт заново, а сообщать в лог тут нечего.
func (cb *CircuitBreaker) RecordNetworkFailure(provider string) (justTripped bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFails[provider]++

	if _, tripped := cb.trippedAt[provider]; tripped {
		cb.trippedAt[provider] = cb.now()
		return false
	}
	if cb.consecutiveFails[provider] >= cb.maxFailures {
		cb.trippedAt[provider] = cb.now()
		return true
	}
	return false
}

// ConsecutiveFails — длина текущей серии неудач. По ней решается, давать
// ли повтор: одиночному сбою даём, продолжению серии — нет.
func (cb *CircuitBreaker) ConsecutiveFails(provider string) int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.consecutiveFails[provider]
}

func (cb *CircuitBreaker) Backoff() time.Duration {
	return cb.backoff
}

// AllTripped — все ли перечисленные провайдеры сейчас отключены.
// Полуоткрытое состояние здесь не учитывается: провайдер, ждущий пробы,
// считается отключённым.
//
// Вызывающих у метода сейчас нет.
func (cb *CircuitBreaker) AllTripped(providerNames []string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	for _, name := range providerNames {
		if _, tripped := cb.trippedAt[name]; !tripped {
			return false
		}
	}
	return len(providerNames) > 0
}

// KnownGoodIMDbID — тайтл, заведомо существующий во всех базах. Нужен для
// проверки ключей OMDb, у которого нет отдельного endpoint'а валидации
// и единственный способ проверить ключ — сделать настоящий запрос.
const KnownGoodIMDbID = "tt0111161" // The Shawshank Redemption
