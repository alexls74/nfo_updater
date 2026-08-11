// internal/providers/keypool.go
package providers

import (
	"sync"

	"nfo_updater/internal/logging"
)

// usageStore — то, что пулу нужно от БД. Интерфейс вместо *db.DB, чтобы
// пакет providers не тянул за собой весь слой хранения ради трёх методов.
type usageStore interface {
	GetUsageToday(provider string, keyIndex int) (int, error)
	IncrementUsage(provider string, keyIndex int) (int, error)
	SetUsageToday(provider string, keyIndex int, requests int) error
}

// keyPool — общий для OMDb и MDBList механизм работы с несколькими ключами.
//
// Ключ выбывает из оборота по трём причинам:
//   - локальный счётчик достиг dailyLimit (наш собственный лимит из конфига);
//   - сервис ответил "ключ невалиден" — до конца прогона;
//   - сервис ответил "лимит исчерпан" — тогда локальный счётчик ещё и
//     подтягивается до лимита, чтобы следующие прогоны в эти же сутки
//     не тратили запрос на повторное выяснение.
type keyPool struct {
	provider   string
	keys       []string
	dailyLimit int
	store      usageStore
	logger     *logging.Logger

	mu       sync.Mutex
	index    int
	disabled map[int]bool
}

func newKeyPool(provider string, keys []string, dailyLimit int, store usageStore, logger *logging.Logger) *keyPool {
	return &keyPool{
		provider:   provider,
		keys:       keys,
		dailyLimit: dailyLimit,
		store:      store,
		logger:     logger,
		disabled:   make(map[int]bool),
	}
}

func (p *keyPool) size() int { return len(p.keys) }

func (p *keyPool) keyAt(i int) string { return p.keys[i] }

// pick выдаёт следующий пригодный ключ по кругу (round-robin), чтобы расход
// размазывался по пулу равномерно, а не выжигал первый ключ до дна.
// Возвращает ErrQuotaExhausted, если пригодных ключей не осталось.
func (p *keyPool) pick() (string, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.keys)
	for i := 0; i < n; i++ {
		idx := (p.index + i) % n
		if p.disabled[idx] {
			continue
		}
		used, err := p.store.GetUsageToday(p.provider, idx)
		if err != nil {
			return "", 0, err
		}
		if used >= p.dailyLimit {
			continue
		}
		p.index = (idx + 1) % n
		return p.keys[idx], idx, nil
	}
	return "", 0, ErrQuotaExhausted
}

// noteRequest засчитывает потраченный запрос. Вызывается ТОЛЬКО когда
// сервис реально обслужил обращение (200 или 404). Отказы по невалидному
// ключу не считаются: у неопознанного ключа нет аккаунта, с которого можно
// было бы списывать, и сам сервис такой запрос себе не засчитывает —
// раньше мы накручивали пользователю фантомный расход при обычной опечатке
// в ключе, вплоть до ложного "лимит исчерпан".
func (p *keyPool) noteRequest(index int) {
	if _, err := p.store.IncrementUsage(p.provider, index); err != nil && p.logger != nil {
		p.logger.Event("[ERROR] %s: cannot record api usage for key #%d: %v", p.provider, index+1, err)
	}
}

// disableInvalid выводит ключ из оборота до конца прогона.
func (p *keyPool) disableInvalid(index int, detail string) {
	p.mu.Lock()
	already := p.disabled[index]
	p.disabled[index] = true
	p.mu.Unlock()

	if !already && p.logger != nil {
		p.logger.Event("[KEY_REJECTED] %s key #%d was rejected by the service (%s), not used again this run",
			p.provider, index+1, detail)
	}
}

// disableExhausted выводит ключ из оборота и подтягивает локальный счётчик
// до лимита. Второе важно потому, что ключом могли пользоваться и вне нашей
// программы: без этого каждый следующий прогон в те же сутки заново тратил
// бы запрос, чтобы услышать тот же отказ.
func (p *keyPool) disableExhausted(index int) {
	p.mu.Lock()
	already := p.disabled[index]
	p.disabled[index] = true
	p.mu.Unlock()

	if err := p.store.SetUsageToday(p.provider, index, p.dailyLimit); err != nil && p.logger != nil {
		p.logger.Event("[ERROR] %s: cannot record exhausted quota for key #%d: %v", p.provider, index+1, err)
	}
	if !already && p.logger != nil {
		p.logger.Event("[KEY_EXHAUSTED] %s key #%d reached its daily limit on the service side, switching to the next key",
			p.provider, index+1)
	}
}

// handleKeyError — общая реакция на ошибку уровня ключа: вывести ключ
// из оборота нужным способом. Возвращает true, если запрос имеет смысл
// немедленно повторить другим ключом.
func (p *keyPool) handleKeyError(ke *KeyError) bool {
	switch ke.Kind {
	case KeyExhausted:
		p.disableExhausted(ke.KeyIndex)
	default:
		p.disableInvalid(ke.KeyIndex, ke.Detail)
	}
	return true
}
