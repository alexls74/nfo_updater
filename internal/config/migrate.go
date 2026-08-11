// internal/config/migrate.go
package config

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"nfo_updater/internal/version"
)

//go:embed config.conf
var defaultConfigTemplate string

var reKeyLine = regexp.MustCompile(`(?m)^([A-Z_][A-Z0-9_]*)=(.*)$`)
var reVersionLine = regexp.MustCompile(`(?m)^VERSION=.*$`)

// backupSuffix — расширение резервной копии, которая создаётся перед
// перезаписью конфига.
const backupSuffix = ".bak"

// managedKeys — параметры, которыми распоряжается сама программа, а не
// пользователь. В переносе значений они не участвуют вовсе.
var managedKeys = map[string]bool{
	"VERSION": true,
}

// ErrConfigCreated — файла конфига не было, только что создан
// шаблон по умолчанию. main.go должен вывести дружелюбное сообщение
// с путём к файлу и выйти, не пытаясь сразу стартовать.
var ErrConfigCreated = fmt.Errorf("default config created, please edit it and restart")

// EnsureConfig гарантирует, что по пути path лежит конфиг актуальной версии.
func EnsureConfig(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := writeDefaultConfig(path); err != nil {
			return "", fmt.Errorf("write default config: %w", err)
		}
		return "", ErrConfigCreated
	}
	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}

	content := string(raw)
	fileVersion := extractVersion(content)

	switch compareVersions(fileVersion, version.Version) {
	case 0:
		return content, nil
	case 1:
		return "", fmt.Errorf(
			"config VERSION (%s) is newer than this binary (%s): downgrading the binary over a newer config is not supported, install a matching or newer release",
			fileVersion, version.Version,
		)
	}

	migrated, dropped := applyValues(defaultConfigTemplate, userValues(content))
	migrated = setVersion(migrated, version.Version)

	// Режим определяется по оригиналу и применяется к обоим файлам.
	// Конфиг содержит ключи API: копия не должна оказаться доступнее
	// исходника, а мигрированный файл — потерять затянутые пользователем
	// права.
	mode := configFileMode(path)

	// Копия делается ДО перезаписи и БЕЗУСЛОВНО, а не только когда что-то
	// выброшено.
	//
	// Неудача копирования ОТМЕНЯЕТ миграцию: писать поверх файла, который
	// не удалось сохранить, — ровно то, от чего эта копия и защищает.
	bakPath, err := backupConfig(path, raw, mode)
	if err != nil {
		return "", fmt.Errorf("cannot back up the existing config before migrating it: %w", err)
	}

	if err := os.WriteFile(path, []byte(migrated), mode); err != nil {
		return "", fmt.Errorf("write migrated config: %w", err)
	}
	// WriteFile не меняет режим уже существующего файла, поэтому права
	// проставляются явно.
	if err := os.Chmod(path, mode); err != nil {
		return "", fmt.Errorf("set permissions on migrated config: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[CONFIG_MIGRATION] config updated to version %s, the previous one was kept as %s\n",
		version.Version, bakPath)

	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "[CONFIG_MIGRATION] the following keys had values in your old config but no longer exist in version %s and were dropped: %s\n",
			version.Version, strings.Join(dropped, ", "))
		fmt.Fprintf(os.Stderr, "[CONFIG_MIGRATION] if that list is unexpected, the old values are still in %s\n",
			bakPath)
	}
	return migrated, nil
}

// WriteConfig создаёт конфиг из встроенного шаблона, подставив переданные
// значения. Используется мастером настройки (--setup).
//
// Пустая строка в values — это осмысленный ответ "оставить умолчание",
// а не отсутствие ответа: строка DATABASE_PATH= означает ровно то же, что
// и в свежем шаблоне. Поэтому значения подставляются как есть, а решение
// не спрашивать о чём-то принимает вызывающий, просто не кладя ключ в карту.
//
// Существующий файл сохраняется в .bak и наследует свои права — те же
// правила, что при миграции: конфиг с ключами API нельзя ни потерять,
// ни сделать доступнее, чем его сделал пользователь.
func WriteConfig(path string, values map[string]string) error {
	content, unknown := applyValues(defaultConfigTemplate, values)
	if len(unknown) > 0 {
		// Ключа нет в шаблоне — это ошибка программиста, а не пользователя:
		// опечатка в имени параметра тихо потеряла бы введённое значение.
		return fmt.Errorf("internal error: no such settings in the config template: %s",
			strings.Join(unknown, ", "))
	}
	content = setVersion(content, version.Version)

	// 0700 на каталоге — там лежат четыре набора ключей API. MkdirAll
	// применяет режим только к тем каталогам, которые создаёт сам, так что
	// уже существующий ~/.config останется с прежними правами.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	mode := os.FileMode(0o600)
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		mode = configFileMode(path)
		if _, err := backupConfig(path, old, mode); err != nil {
			return fmt.Errorf("cannot back up the existing config before rewriting it: %w", err)
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("read existing config: %w", err)
	}

	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	// WriteFile не меняет режим уже существующего файла.
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("set permissions on config: %w", err)
	}
	return nil
}

// configFileMode — права существующего конфига. При неудаче stat берутся
// заведомо строгие 0600, а не привычные 0644.
func configFileMode(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0o600
}

// backupConfig сохраняет содержимое конфига рядом с ним же, под тем же
// именем с суффиксом .bak. Возвращает путь копии для сообщения пользователю.
//
// Копия ровно одна и перезаписывается при каждой перезаписи конфига.
func backupConfig(path string, content []byte, mode os.FileMode) (string, error) {
	bakPath := path + backupSuffix

	if err := os.WriteFile(bakPath, content, mode); err != nil {
		return "", err
	}
	if err := os.Chmod(bakPath, mode); err != nil {
		return "", err
	}
	return bakPath, nil
}

// writeDefaultConfig создаёт конфиг из шаблона при первом запуске.
//
// Через WriteConfig не выражено намеренно: там есть чтение и копирование
// существующего файла, а здесь заведомо нечего копировать — путь вызывается
// только когда os.ReadFile вернул "не существует".
func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	content := setVersion(defaultConfigTemplate, version.Version)
	return os.WriteFile(path, []byte(content), 0o600)
}

// applyValues подставляет значения в шаблон по именам ключей и возвращает
// те ключи из values, которым в шаблоне не нашлось строки.
//
// Общий примитив для двух операций, которые иначе писали бы формат файла
// каждая по-своему: миграция берёт значения из старого конфига, мастер —
// из ответов пользователя. Смысл несовпавших ключей у них разный (для
// миграции это выброшенный параметр, для мастера — ошибка в коде), поэтому
// решение о том, что с ними делать, принимает вызывающий.
//
// Список несовпавших сортируется: обход map даёт случайный порядок, и одно
// и то же сообщение об ошибке выглядело бы каждый раз иначе.
func applyValues(template string, values map[string]string) (result string, unknown []string) {
	used := make(map[string]bool, len(values))

	result = reKeyLine.ReplaceAllStringFunc(template, func(line string) string {
		m := reKeyLine.FindStringSubmatch(line)
		key := m[1]
		val, ok := values[key]
		if !ok {
			return line
		}
		used[key] = true
		return key + "=" + val
	})

	for key := range values {
		if !used[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return result, unknown
}

// userValues вытаскивает из текста конфига значения, заданные пользователем.
//
// Пустые значения пропускаются: в старом файле они означают "умолчание",
// и переносить их поверх шаблона незачем — там на их месте стоит ровно то же
// самое. Служебные ключи (см. managedKeys) не переносятся никогда.
func userValues(content string) map[string]string {
	out := make(map[string]string)
	for _, m := range reKeyLine.FindAllStringSubmatch(content, -1) {
		key, val := m[1], m[2]
		if managedKeys[key] || val == "" {
			continue
		}
		out[key] = val
	}
	return out
}

func extractVersion(content string) string {
	m := reVersionLine.FindString(content)
	if m == "" {
		return "0.0.0"
	}
	return strings.TrimPrefix(m, "VERSION=")
}

func setVersion(content, v string) string {
	block := "\n# ------------------------------------------------------------------------------\n" +
		"# INTERNAL — managed automatically, do not edit\n" +
		"# ------------------------------------------------------------------------------\n" +
		"VERSION=" + v + "\n"

	if reVersionLine.MatchString(content) {
		return reVersionLine.ReplaceAllLiteralString(content, "VERSION="+v)
	}
	return strings.TrimRight(content, "\n") + "\n" + block
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		na, nb := versionPart(pa, i), versionPart(pb, i)
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionPart(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n := 0
	for _, c := range parts[i] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
