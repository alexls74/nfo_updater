// internal/mediaserver/mediaserver.go
//
// Пакет сообщает медиасерверу, что файлы .nfo изменились, и просит его
// пересканировать библиотеку. Никакой другой работы здесь нет: рейтинги
// правит processor, а сервер только перечитывает то, что уже лежит на диске.
//
// Обновление запрашивается ЦЕЛИКОМ по библиотеке, а не по конкретным файлам.
// Точечное обновление потребовало бы искать item по пути (у трёх серверов —
// тремя разными способами) и стоило бы запроса на каждый изменённый файл;
// полное сканирование стоит одного запроса на прогон, а всю остальную работу
// сервер делает сам и лучше нас.
//
// Степень проверки реализаций разная, и об этом честно:
//   - Emby — проверен на живом сервере;
//   - Jellyfin — форк Emby, эндпоинты и семантика те же; отличается только
//     схема авторизации, поэтому вынесен отдельным конструктором;
//   - Plex — написан по документации API, ВЖИВУЮ НЕ ПРОВЕРЯЛСЯ.
package mediaserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"

	"nfo_updater/internal/config"
	"nfo_updater/internal/logging"
)

// Server — один настроенный медиасервер.
type Server interface {
	// Name — имя для логов: emby, jellyfin, plex.
	Name() string
	// Ping проверяет, что сервер отвечает и принимает наш ключ.
	Ping(ctx context.Context) error
	// Refresh просит сервер пересканировать библиотеку.
	Refresh(ctx context.Context) error
}

// FromConfig собирает список включённых серверов. Выключенные не создаются
// вовсе, поэтому дальше по коду проверять флаги не нужно.
//
// httpClient должен быть получен из NewHTTPClient этого же пакета, а не
// от providers: у медиасерверов свои сроки, свой запрет на прокси и свой
// отказ от keep-alive.
func FromConfig(cfg *config.Config, httpClient *http.Client) []Server {
	var out []Server
	if cfg.EmbyEnabled {
		out = append(out, NewEmby(cfg.EmbyURL, cfg.EmbyAPIKey, httpClient))
	}
	if cfg.JellyfinEnabled {
		out = append(out, NewJellyfin(cfg.JellyfinURL, cfg.JellyfinAPIKey, httpClient))
	}
	if cfg.PlexEnabled {
		out = append(out, NewPlex(cfg.PlexURL, cfg.PlexToken, cfg.PlexSectionIDs, httpClient))
	}
	return out
}

// RefreshAll просит каждый сервер обновить библиотеку.
//
// Ошибки НЕ возвращаются наверх намеренно. Рейтинги к этому моменту уже
// записаны в файлы, и недоступный медиасервер ничего из сделанного не
// отменяет: он подхватит изменения на своём плановом сканировании.
func RefreshAll(ctx context.Context, servers []Server, logger *logging.Logger) {
	for _, s := range servers {
		if err := s.Refresh(ctx); err != nil {
			logger.Event("[MEDIASERVER_WARNING] %s: library refresh failed: %v", s.Name(), err)
			continue
		}
		logger.Event("[MEDIASERVER] %s: library scan requested", s.Name())
	}
}

// CheckAll опрашивает серверы, ничего не запуская. Для --check-config
// и для стартовой проверки прогона.
func CheckAll(ctx context.Context, servers []Server, logger *logging.Logger) {
	for _, s := range servers {
		if err := s.Ping(ctx); err != nil {
			logger.Event("[MEDIASERVER_WARNING] %s: %v", s.Name(), err)
			continue
		}
		logger.Event("[MEDIASERVER] %s: reachable", s.Name())
	}
}

// normalizeURL убирает слэш, чтобы склейка с путём эндпоинта
// не давала двойного: пользователь пишет адрес как ему привычнее.
func normalizeURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// requestError переводит ошибку транспорта в осмысленное сообщение.
type Unreachable struct{ msg string }

func (e *Unreachable) Error() string { return e.msg }

// Rejected — сервер ответил и сказал «нет»: не тот адрес, не тот ключ.
// Введённое заведомо неверно, и чинится это только перенабором.
type Rejected struct{ msg string }

func (e *Rejected) Error() string { return e.msg }

// IsUnreachable отличает «не дозвонились» от «ответили отказом». Развилка
// та же, что у ключей API, и по той же причине: в одном случае человека
// просят исправить введённое, в другом просить нечего.
func IsUnreachable(err error) bool {
	var u *Unreachable
	return errors.As(err, &u)
}

func requestError(target string, err error) error {
	return &Unreachable{msg: requestErrorText(target, err)}
}

func requestErrorText(target string, err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Sprintf("request to %s was cancelled", target)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("%s did not answer in time", target)
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Sprintf("cannot reach %s: the host name %q could not be resolved from this machine",
			target, dnsErr.Name)
	}

	// Порт ответил, но не TLS-рукопожатием. Практически всегда это
	// https:// в адресе сервера, который говорит по обычному http.
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return fmt.Sprintf("cannot reach %s: the port answered, but not with TLS — the address is most likely http:// rather than https://", target)
	}

	// Ошибки сертификата разбираются по типам из x509: с версии Go 1.20
	// они завёрнуты в tls.CertificateVerificationError, но errors.As
	// доберётся до них через Unwrap в любом случае.
	var authErr x509.UnknownAuthorityError
	if errors.As(err, &authErr) {
		return fmt.Sprintf("cannot reach %s: the TLS certificate is not trusted by this machine — a self-signed certificate has to be added to the system trust store", target)
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return fmt.Sprintf("cannot reach %s: the TLS certificate is not valid for this host name", target)
	}
	var invalidErr x509.CertificateInvalidError
	if errors.As(err, &invalidErr) {
		return fmt.Sprintf("cannot reach %s: the TLS certificate was rejected: %v", target, invalidErr)
	}

	// Op == "dial" означает, что до обмена данными дело не дошло вовсе.
	// Это самая ценная развилка: три её ветки требуют трёх совершенно
	// разных действий от пользователя.
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			return fmt.Sprintf("cannot reach %s: connection refused, nothing is listening on that port", target)
		case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
			return fmt.Sprintf("cannot reach %s: no route to that address from this machine", target)
		case opErr.Timeout():
			return fmt.Sprintf("cannot reach %s: the connection attempt timed out with no reply — the address has to be reachable from the machine running nfo_updater, which is not necessarily the one your browser runs on", target)
		}
		return fmt.Sprintf("cannot reach %s: %v", target, opErr.Err)
	}

	// Соединение установилось, а ответа не дождались: это уже про сервер,
	// а не про сеть.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Sprintf("%s accepted the connection but did not answer in time", target)
	}

	return fmt.Sprintf("cannot reach %s: %v", target, err)
}

// checkStatus переводит код ответа в осмысленную ошибку.
//
// Отдельная ветка на 401/403 нужна потому, что это единственный случай,
// который пользователь обязан чинить руками; всё прочее — временное.
// Отдельная ветка на 404 — потому что чаще всего это не «нет такого
// эндпоинта», а опечатка в адресе сервера или лишний путь в нём.
func checkStatus(code int) error {
	switch {
	case code >= 200 && code < 300:
		return nil
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return &Rejected{msg: fmt.Sprintf(
			"the server is reachable but rejected the API key (http status %d)", code)}
	case code == http.StatusNotFound:
		return &Rejected{msg: "endpoint not found (http status 404), check the server address"}
	default:
		return &Unreachable{msg: fmt.Sprintf("the server answered with an unexpected http status %d", code)}
	}
}

// ---------------------------------------------------------------------------
// Проверка одного только адреса
// ---------------------------------------------------------------------------

// reachPaths — эндпоинты, отвечающие БЕЗ авторизации.
//
// У Emby и Jellyfin это /System/Info/Public: тот же /System/Info, только
// урезанный до сведений, которые сервер показывает кому угодно. У Plex —
// /identity, который отвечает и без токена; именно поэтому Ping для Plex
// им не пользуется, а спрашивает список секций.
var reachPaths = map[string]string{
	"emby":     "/System/Info/Public",
	"jellyfin": "/System/Info/Public",
	"plex":     "/identity",
}

// Reach проверяет ТОЛЬКО адрес: стоит ли по нему сервер и тот ли он, за кого
// себя выдаёт. Про ключ здесь не спрашивается ничего.
//
// Нужно затем, чтобы разделить два вопроса, которые Ping сваливает в один.
// Мастер настройки спрашивает адрес и ключ по очереди, и человек, ошибившийся
// в адресе, должен узнать об этом сразу.
func Reach(ctx context.Context, name, rawURL string, client *http.Client) error {
	path, ok := reachPaths[strings.ToLower(name)]
	if !ok {
		return fmt.Errorf("internal error: no reachability endpoint for media server %q", name)
	}
	base := normalizeURL(rawURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", base+path, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return requestError(base, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// Сервер на месте и отвечает — просто закрыт целиком. Для проверки
		// адреса это успех: разбираться с доступом будет следующий шаг.
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return &Rejected{msg: fmt.Sprintf("something answered at %s, but it does not look like a media server of this kind (http status 404) — check the address, and that there is no extra path in it", base)}
	default:
		return &Unreachable{msg: fmt.Sprintf("%s answered with an unexpected http status %d", base, resp.StatusCode)}
	}
}
