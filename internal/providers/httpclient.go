// internal/providers/httpclient.go
package providers

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// NewHTTPClient собирает клиента для запросов ко всем провайдерам рейтингов.
//
// Главное здесь — ОТКЛЮЧЁННЫЙ HTTP/2, и это не перестраховка, а лечение
// конкретной болезни. Транспорт Go по умолчанию не шлёт HTTP/2-пингов
// (ReadIdleTimeout в стандартной библиотеке не настраивается вовсе) и не
// замечает, что мультиплексированное соединение умерло на той стороне.
// Новые запросы уходят в мёртвое соединение и висят до общего таймаута
// клиента. На MDBList это давало ровно один удачный запрос за прогон и
// двадцатисекундный таймаут на каждый следующий; OMDb с его другим набором
// узлов Cloudflare ту же беду не проявлял, отчего проблема и выглядела
// как отказ одного сервиса.
//
// Мультиплексирование нам не нужно: запросы к провайдерам идут строго
// последовательно, по одному. HTTP/1.1 с keep-alive закрывает потребность
// полностью и ведёт себя предсказуемо.
//
// Пустая, но НЕ nil карта TLSNextProto — стандартный способ запретить
// согласование h2 при TLS-рукопожатии. nil означало бы обратное:
// то есть HTTP/2 включён.
func NewHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,

		// Отдельный срок на ожидание заголовков ответа. Меньше общего
		// таймаута клиента намеренно: так в логе видно, что запрос ушёл,
		// а ответ не пришёл, — это другая неисправность, чем "не смогли
		// дозвониться", и различать их в сообщении полезно.
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,

		// Простаивающее соединение живёт недолго. Между тайтлами проходят
		// секунды, между прогонами демона — дни; держать открытым то,
		// что почти наверняка уже закрыто на той стороне, незачем.
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 2,

		TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}
