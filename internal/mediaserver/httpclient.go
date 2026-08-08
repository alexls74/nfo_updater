// internal/mediaserver/httpclient.go
package mediaserver

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// NewHTTPClient собирает клиента для запросов к медиасерверам.
//
// Отдельный от providers.NewHTTPClient намеренно: у двух подсистем разная
// сеть и разная цена ожидания. Провайдеры рейтингов — публичные сервисы,
// к ним за прогон уходят сотни запросов. Медиасервер — свой, чаще всего
// в локальной сети, и запросов к нему ровно два: проверка на старте
// и просьба пересканировать в конце.
//
// Чем этот клиент отличается от провайдерского и почему:
//
//   - Proxy: nil вместо ProxyFromEnvironment. Переменная http_proxy
//     в окружении означала бы, что запрос к соседней машине уходит наружу,
//     а в тексте ошибки стоит адрес прокси, а не сервера. Медиасервер
//     всегда свой; ходить к нему через чужой узел незачем.
//
//   - DisableKeepAlives. Между двумя запросами прогона проходит всё
//     сканирование библиотеки — минуты, а на большой медиатеке и больше.
//     Соединение к этому моменту заведомо мертво, а держать мёртвое
//     соединение в пуле — ровно тот сценарий, на котором мы уже обожглись
//     с MDBList.
//
//   - Сроки короче. Пять секунд на дозвон покрывают и локальную сеть,
//     и публичный адрес с холодным DNS. Ждать дольше нечего: недоступный
//     медиасервер прогон всё равно не отменяет.
//
// HTTP/2 запрещён так же, как у провайдеров, — пустой, но НЕ nil картой
// TLSNextProto. Публичный медиасервер почти всегда стоит за reverse proxy
// или Cloudflare, то есть ровно в той конфигурации, где зависание
// мультиплексированного соединения уже наблюдалось.
func NewHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,

		DisableKeepAlives: true,

		TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}

	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
	}
}
