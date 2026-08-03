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
//   - Emby     — проверен на живом сервере;
//   - Jellyfin — форк Emby, эндпоинты и семантика те же; отличается только
//     схема авторизации, поэтому вынесен отдельным конструктором;
//   - Plex     — написан по документации API, ВЖИВУЮ НЕ ПРОВЕРЯЛСЯ.
package mediaserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"

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
// отменяет: он подхватит изменения на своём плановом сканировании. Считать
// прогон неудавшимся из-за выключенного на ночь Plex было бы неправдой —
// поэтому сюда пишется предупреждение, а код возврата не меняется.
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
// и для стартовой проверки прогона: лучше сказать про неверный токен сразу,
// чем через час молчаливого бездействия.
func CheckAll(ctx context.Context, servers []Server, logger *logging.Logger) {
	for _, s := range servers {
		if err := s.Ping(ctx); err != nil {
			logger.Event("[MEDIASERVER_WARNING] %s: %v", s.Name(), err)
			continue
		}
		logger.Event("[MEDIASERVER] %s: reachable", s.Name())
	}
}

// normalizeURL убирает хвостовой слэш, чтобы склейка с путём эндпоинта
// не давала двойного: пользователь пишет адрес как ему привычнее.
func normalizeURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
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
		return fmt.Errorf("server rejected the API key (http status %d)", code)
	case code == http.StatusNotFound:
		return fmt.Errorf("endpoint not found (http status 404), check the server address")
	default:
		return fmt.Errorf("unexpected http status %d", code)
	}
}
