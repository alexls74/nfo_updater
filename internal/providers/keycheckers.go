// internal/providers/keycheckers.go
package providers

import (
	"net/http"

	"nfo_updater/internal/logging"
)

// Сборка провайдеров для ОДНОЙ ТОЛЬКО проверки ключей — для мастера
// настройки и для всего, что хочет спросить "годятся ли эти ключи",
// не открывая базу.
//
// Живёт в этом пакете, а не в мастере, по единственной причине: usageStore
// не экспортирован. Внешний пакет мог бы передать свою заглушку (интерфейсы
// в Go удовлетворяются структурно), но такая связь ломается молча и в самом
// неприятном месте — при добавлении метода в usageStore внешняя заглушка
// перестаёт подходить, а сообщение компилятора укажет на чужой файл.

// noUsageStore — учёт расхода квоты, который ничего не учитывает.
//
// Мастеру нечего и некуда записывать: база может не существовать вовсе,
// а её путь настраивается прямо сейчас. Цена — незаписанный расход в
// несколько запросов: проверка ключа OMDb стоит одного настоящего обращения
// (бесплатного способа у сервиса нет), у MDBList и TMDb она бесплатна.
// Против суточной тысячи это шум.
type noUsageStore struct{}

func (noUsageStore) GetUsageToday(string, int) (int, error)  { return 0, nil }
func (noUsageStore) IncrementUsage(string, int) (int, error) { return 0, nil }
func (noUsageStore) SetUsageToday(string, int, int) error    { return nil }

// keyCheckDailyLimit — лимит для пула, собранного ради проверки.
//
// На саму проверку не влияет: CheckKeys обходит ключи напрямую через
// keyAt, минуя pick() с его учётом расхода. Значение положительное просто
// потому, что ноль означал бы "ключей не осталось" для любого другого
// пути, если такой когда-нибудь появится.
const keyCheckDailyLimit = 1

// NewKeyCheckers собирает провайдеров, у которых есть что проверять.
//
// Провайдер с пустым набором ключей не создаётся вовсе: проверять нечего,
// а пустой результат в отчёте выглядел бы как молчаливый отказ. О том, что
// сервис не настроен, сообщает валидация конфига, и это другой разговор.
func NewKeyCheckers(omdbKeys, mdblistKeys []string, tmdbKey string, client *http.Client) []KeyChecker {
	// Логгер с двумя nil-приёмниками пишет в io.Discard: сообщения пула
	// об отвергнутых ключах не должны лезть в диалог мастера, который
	// показывает те же факты своими словами.
	silent := logging.New(nil, nil, false)
	store := noUsageStore{}

	var out []KeyChecker
	if len(omdbKeys) > 0 {
		out = append(out, NewOMDbProvider(omdbKeys, keyCheckDailyLimit, store, silent, client))
	}
	if len(mdblistKeys) > 0 {
		out = append(out, NewMDBListProvider(mdblistKeys, keyCheckDailyLimit, store, silent, client))
	}
	if tmdbKey != "" {
		out = append(out, NewTMDbProvider(tmdbKey, client))
	}
	return out
}
