// internal/config/exported.go
package config

// Экспортируемые обёртки над внутренними проверками конфига.
//
// Существуют ради мастера настройки (internal/setup): он обязан проверять
// введённое по тем же правилам и теми же словами, что и валидация конфига.
// Своя реализация в мастере означала бы две трактовки одного требования,
// а требования тут не косметические — от раздельности деревьев медиатеки
// зависит и категория файла, и его путь внутри архива бэкапа.
//
// Файл собран из обёрток намеренно, а не из перенесённого кода: сами
// проверки остаются там, где их вызывает Validate, и не переезжают
// туда-сюда при каждой новой надобности.

import (
	"maps"
	"os"
	"path/filepath"
)

// ExistingValues возвращает значения, заданные пользователем в конфиге
// по пути path. Отсутствие файла — не ошибка: это первая установка,
// и карта просто пуста.
//
// Мастер настройки начинает с этой карты и кладёт поверх свои ответы.
// Так параметры, о которых он не спрашивает (LOG_LIMIT, BACKUP_LIMIT,
// пороги circuit breaker), переживают перенастройку: без этого базой
// служил бы чистый шаблон, и всё, о чём не спросили, молча вернулось бы
// к умолчаниям.
func ExistingValues(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return userValues(string(raw)), nil
}

// Configured отвечает, настраивал ли кто-нибудь эту систему, — в отличие
// от ExistingValues, которая отвечает, что именно там написано.
//
// Вопросы разные, и путать их дорого. Мастер настройки судил о том, есть
// ли прежняя настройка, по непустоте карты значений — но свежесозданный
// шаблон содержит дюжину непустых умолчаний (LOG_ENABLED, IMDB_RATING,
// DEFAULT_RATING, CREW_ORDER_FIX, пороги circuit breaker), и человек,
// один раз запустивший программу голым и получивший шаблон, слышал бы
// от мастера "найдена существующая конфигурация". Хуже того, умолчание
// вопроса о расписании считается из SCHEDULE, а в шаблоне он пуст, —
// то есть на первой же установке Enter отказывался бы от фоновой работы
// вместо согласия на неё.
//
// Сравнение идёт с самим шаблоном, а не с перечнем "важных" ключей:
// список важного пришлось бы держать в согласии с мастером, а шаблон
// и так лежит рядом.
func Configured(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !maps.Equal(userValues(string(raw)), userValues(defaultConfigTemplate)), nil
}

// Имена внутри каталога данных. Собраны здесь, потому что нужны в двух
// местах: умолчаниям конфига (defaults) и мастеру настройки, когда человек
// назначает свой корень вместо ~/.local/share/nfo_updater.
const (
	dataDatabaseFile = "database.db"
	dataLogsDir      = "logs"
	dataBackupsDir   = "backups"
)

// DataPathsUnder — раскладка базы, логов и бэкапов внутри заданного корня.
//
// Пустой корень остаётся пустым во всех трёх путях: лучше пустое значение
// и понятная ошибка при валидации ("умолчание не удалось вычислить, задайте
// путь явно"), чем относительный путь, который заведёт базу неизвестно где.
func DataPathsUnder(root string) (databasePath, logDir, backupDir string) {
	if root == "" {
		return "", "", ""
	}
	return filepath.Join(root, dataDatabaseFile),
		filepath.Join(root, dataLogsDir),
		filepath.Join(root, dataBackupsDir)
}

// HomeDir возвращает домашний каталог текущего пользователя или "".
//
// Мастеру нужен для разворачивания тильды во введённом пути: сам конфиг
// тильду не понимает (см. pathError), но человек, набирающий путь руками
// в диалоге, напишет её почти наверняка, и отвечать ему отказом там, где
// программа прекрасно знает ответ, было бы буквоедством.
func HomeDir() string {
	return homeDir()
}

// CheckMediaPaths проверяет пару списков путей медиатеки целиком: каждый
// путь абсолютен, дубли схлопнуты, деревья не пересекаются и не вложены
// друг в друга.
//
// Мастер вызывает это после каждого добавленного пути, передавая уже
// собранные списки обеих категорий, — так конфликт обнаруживается сразу
// на вводе, а не в конце, когда переспрашивать поздно.
func CheckMediaPaths(movies, tvshows []string) error {
	m, err := normalizePathList("MOVIES_PATH", movies)
	if err != nil {
		return err
	}
	t, err := normalizePathList("TVSHOWS_PATH", tvshows)
	if err != nil {
		return err
	}

	refs := make([]pathRef, 0, len(m)+len(t))
	for _, p := range m {
		refs = append(refs, pathRef{setting: "MOVIES_PATH", path: p})
	}
	for _, p := range t {
		refs = append(refs, pathRef{setting: "TVSHOWS_PATH", path: p})
	}
	return checkPathOverlaps(refs)
}

// CheckServerURL проверяет адрес медиасервера: схема http/https и непустой
// хост. Самая частая ошибка — адрес без схемы, который net/url разберёт
// молча, оставив пустой host.
//
// setting передаётся, чтобы в сообщении стояло имя параметра: мастер
// показывает его же, и человек, вернувшийся потом к config.conf руками,
// найдёт там ровно ту строку, о которой шла речь.
func CheckServerURL(setting, raw string) error {
	return checkServerURL(setting, raw)
}

// CheckPlexSectionIDs требует, чтобы секции были числами. Название
// библиотеки вместо номера — самая вероятная ошибка в этом поле, и молча
// уйти в запрос она не должна.
func CheckPlexSectionIDs(ids []string) error {
	for _, id := range ids {
		if !isDigits(id) {
			return sectionIDError(id)
		}
	}
	return nil
}
