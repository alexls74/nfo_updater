// internal/providers/keycheck.go
package providers

import "context"

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
// существующему тайтлу. Стоит по одному запросу на ключ и честно
// списывается с квоты — сервис обращение обслужил.
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
