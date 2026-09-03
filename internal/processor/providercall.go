// internal/processor/providercall.go
package processor

import (
	"fmt"
	"time"

	"nfo_updater/internal/providers"
)

// providerAttempts — сколько раз подряд обращаемся к провайдеру, прежде чем
// признать тайтл неудавшимся и уйти к следующему провайдеру.
//
// Двойка, а не больше. Повтор закрывает одиночную неисправность: потерянный
// UDP-пакет с DNS-запросом, единичный RST, разовый 5xx. Такое лечится второй
// попыткой; то, что не прошло со второй, не пройдёт и с третьей, а цена
// растёт на каждом из тысячи с лишним тайтлов.
//
// Сознательно НЕ вынесено в config.conf: крутить это число пользователю
// незачем, а каждый новый ключ конфига — это ещё и строка в шаблоне,
// в мастере настройки, в README и миграция старых файлов.
const providerAttempts = 2

// providerRetryDelay — пауза перед повтором. Отдельно от
// CIRCUIT_BREAKER_BACKOFF_SECONDS намеренно: это разные по смыслу сроки.
// Backoff означает "сервису плохо, дадим ему передохнуть", а эта пауза —
// "пакет потерялся, спросим ещё раз". Свести их в один параметр значит
// потом не суметь настроить ни один из двух.
const providerRetryDelay = 2 * time.Second

// breakerProbeAfter — через сколько отключённый провайдер получает пробный
// запрос. Тридцать секунд: заметно больше типичного сетевого моргания,
// но много меньше прохода, так что провайдер успевает вернуться в строй
// и отработать оставшуюся часть медиатеки.
const breakerProbeAfter = 30 * time.Second

// callProvider выполняет одно обращение к провайдеру со всей сопутствующей
// обвязкой: повтор, запись в лог, учёт в circuit breaker и в статистике.
//
// attempt делает сам запрос и складывает результат туда, куда нужно
// вызывающему; сюда возвращается только ошибка. Замыкание вместо
// возвращаемого значения потому, что вызывающих двое и результаты у них
// разной формы: у фильмов это FetchResult, у сериалов ещё и таблица серий.
//
// Возвращается ошибка последней попытки. Всё, что не является
// NetworkError — успех, ErrNotFound, ErrNoRatings, ErrQuotaExhausted,
// ErrUnsupportedID, — отдаётся вызывающему немедленно и без вмешательства:
// это содержательные ответы, разбирать их не наше дело.
func callProvider(deps *Deps, name string, ids providers.IDs, attempt func() error) error {
	// Повтор получает только ПЕРВЫЙ сбой в серии. Если провайдер уже
	// спотыкается, вторая попытка на каждом тайтле удвоила бы простой
	// и ничего не дала бы: сеть не чинится за две секунды. Отключённый
	// провайдер сюда приходит с непустой серией, поэтому пробный запрос
	// повтора не получает — это выходит само собой.
	tries := 1
	if deps.Breaker.ConsecutiveFails(name) == 0 {
		tries = providerAttempts
	}

	var err error
	for attemptNo := 1; attemptNo <= tries; attemptNo++ {
		err = attempt()

		ne, isNetwork := providers.AsNetworkError(err)
		if !isNetwork {
			return err
		}

		if ne.Retryable() && attemptNo < tries {
			// Промежуточная неудача идёт в Detail: при обычном прогоне
			// лог не должен пухнуть вдвое ровно в тот момент, когда его
			// тяжелее всего читать. Понадобится разбор — включается -v.
			deps.Logger.Detail("[PROVIDER_RETRY] %s key #%d (%s): %v, retrying in %s",
				name, ne.KeyIndex+1, idsLabel(ids), ne.Err, providerRetryDelay)
			time.Sleep(providerRetryDelay)
			continue
		}

		logProviderFailure(deps, name, ids, ne, attemptNo)
		deps.Stats.IncProviderFailure(name)

		// В breaker засчитывается ТАЙТЛ, а не попытка: иначе порог
		// CIRCUIT_BREAKER_FAILURES срабатывал бы вдвое раньше написанного.
		if deps.Breaker.RecordNetworkFailure(name) {
			deps.Logger.Event("[PROVIDER_DOWN] %s: tripped after repeated network failures, will be probed again later this run", name)
		}
		time.Sleep(deps.Breaker.Backoff())
		return err
	}

	return err
}

// logProviderFailure пишет итоговую неудачу по тайтлу — ровно одну строку
// независимо от числа попыток.
//
// Номер ключа в сообщении не украшение: без него нельзя отличить "отвалился
// один ключ из трёх" от "отвалилась сеть". Текст самой ошибки берётся из
// ne.Err, а не из ne.Error(), чтобы имя провайдера не печаталось дважды.
func logProviderFailure(deps *Deps, name string, ids providers.IDs, ne *providers.NetworkError, attempts int) {
	suffix := ""
	if attempts > 1 {
		suffix = fmt.Sprintf(" (%d attempts)", attempts)
	}
	deps.Logger.Event("[PROVIDER_ERROR] %s key #%d (%s): %v%s",
		name, ne.KeyIndex+1, idsLabel(ids), ne.Err, suffix)
}

// providerAnswered — общая реакция на любой содержательный ответ провайдера:
// значение, "тайтла нет" или "оценок нет". Для circuit breaker и счётчика
// запросов все три одинаковы — сервис нас обслужил.
func providerAnswered(deps *Deps, name string) {
	if deps.Breaker.RecordSuccess(name) {
		deps.Logger.Event("[PROVIDER_UP] %s: answered again, back in service for the rest of this run", name)
	}
	deps.Stats.IncProviderRequest(name)
}
