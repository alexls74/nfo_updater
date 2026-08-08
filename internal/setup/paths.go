// internal/setup/paths.go
package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"nfo_updater/internal/config"
)

// Константы режима для access(2) — те же, что в internal/processor/access.go
// и по той же причине: в syscall они не экспортируются платформонезависимо,
// а тянуть golang.org/x/sys ради трёх чисел незачем. Значения зафиксированы
// POSIX и одинаковы на всех Linux-архитектурах.
const (
	unixXOK = 0x1
	unixWOK = 0x2
	unixROK = 0x4
)

// expandTilde разворачивает ведущую тильду в домашний каталог.
//
// Делается ТОЛЬКО здесь, в диалоге, и никогда при чтении конфига. Разница
// принципиальна: в файл всегда попадает развёрнутый абсолютный путь, так что
// инвариант "в конфиге только абсолютные пути" сохраняется, а человек,
// набирающий путь руками, не получает отказа там, где программа знает ответ.
//
// Форма ~user не поддерживается: она потребовала бы разбора passwd ради
// случая, которого в домашней медиатеке не бывает. Такая строка останется
// как есть и будет отвергнута проверкой на абсолютность с внятным текстом.
func expandTilde(raw string) string {
	if raw != "~" && !strings.HasPrefix(raw, "~/") {
		return raw
	}
	home := config.HomeDir()
	if home == "" {
		return raw
	}
	if raw == "~" {
		return home
	}
	return filepath.Join(home, raw[2:])
}

// cleanPath приводит введённое к каноничному виду или объясняет, почему это
// не путь. Возвращает готовое к записи в конфиг значение.
func cleanPath(raw string) (string, error) {
	p := expandTilde(strings.TrimSpace(raw))
	if p == "" {
		return "", fmt.Errorf("the path is empty")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("must be an absolute path, starting with /")
	}
	return filepath.Clean(p), nil
}

// checkMediaDir — проверка каталога медиатеки.
//
// Требуются R_OK|X_OK, а не право записи в сам каталог: программа не создаёт
// и не удаляет в медиатеке файлы, только перезаписывает существующие .nfo.
// Право на запись проверяется у каждого файла отдельно уже во время прогона
// (см. processor/access.go) — здесь его требовать нельзя, иначе мы отвергнем
// вполне рабочую медиатеку, лежащую на разделе только для чтения каталогов.
func checkMediaDir(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("no such directory")
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	if err := syscall.Access(path, unixROK|unixXOK); err != nil {
		return fmt.Errorf("not readable by the current user")
	}
	return nil
}

// checkDataDir — проверка каталога, в который программа будет ПИСАТЬ: база,
// логи, бэкапы.
//
// Каталога может ещё не быть, и это нормально — программа создаст его сама.
// Поэтому при отсутствии проверяется ближайший существующий предок: если
// в него можно писать, создание удастся. Так вопрос "а заведётся ли база
// на этом диске" получает ответ сразу, а не при первом прогоне.
func checkDataDir(path string) error {
	info, err := os.Stat(path)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("exists and is not a directory")
	case err == nil:
		if err := syscall.Access(path, unixWOK|unixXOK); err != nil {
			return fmt.Errorf("not writable by the current user")
		}
		return nil
	case !os.IsNotExist(err):
		return err
	}

	// Каталога нет. Ищем ближайшего существующего предка.
	parent := path
	for {
		next := filepath.Dir(parent)
		if next == parent {
			// Дошли до корня, а он существует всегда — сюда попасть нельзя.
			return fmt.Errorf("cannot be created")
		}
		parent = next
		if _, err := os.Stat(parent); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := syscall.Access(parent, unixWOK|unixXOK); err != nil {
		return fmt.Errorf("does not exist and cannot be created: %s is not writable", parent)
	}
	return nil
}

// askDir спрашивает один каталог и проверяет его.
//
// При неудачной проверке предлагается развилка: повторить, пропустить,
// прервать. Пропуск разрешён не всегда — для каталога данных его нет:
// пропустить там означает остаться без базы, а это не настройка, а поломка.
//
// Возвращает (путь, принят, ошибка). Принят == false означает пропуск.
func askDir(p *Prompt, question, def string, check func(string) error, allowSkip bool) (string, bool, error) {
	opts := []Option{
		{Key: "r", Label: "enter a different path"},
		{Key: "s", Label: "skip this one"},
		{Key: "a", Label: "abort setup"},
	}
	if !allowSkip {
		opts = append(opts[:1], opts[2:]...)
	}

	for {
		raw, err := p.Required(question, def)
		if err != nil {
			return "", false, err
		}

		path, err := cleanPath(raw)
		if err != nil {
			p.Problem("%v", err)
			continue
		}
		// Развёрнутая тильда показывается явно: в конфиг уедет именно это,
		// и человек должен увидеть, с чем согласился.
		if path != strings.TrimSpace(raw) {
			p.Note("using %s", path)
		}

		if err := check(path); err != nil {
			p.Result(false, "%s — %v", path, err)
			choice, cerr := p.Choice("What would you like to do?", opts, 0)
			if cerr != nil {
				return "", false, cerr
			}
			switch opts[choice].Key {
			case "r":
				continue
			case "s":
				return "", false, nil
			default:
				return "", false, ErrAborted
			}
		}

		p.Result(true, "%s", path)
		return path, true, nil
	}
}

// askMediaPaths собирает список каталогов одной категории.
//
// accept вызывается с очередным кандидатом уже после проверки самого
// каталога и решает, не конфликтует ли он с остальной конфигурацией:
// вызывающий подставляет туда config.CheckMediaPaths с накопленными списками
// обеих категорий. Так пересечение деревьев ловится сразу на вводе, а не
// в конце, когда переспрашивать поздно.
//
// Пустой ответ на первый вопрос означает "этой категории у меня нет" —
// законное состояние: конфиг требует хотя бы одну из двух, а не обе.
func askMediaPaths(p *Prompt, question string, current []string, accept func(path string) error) ([]string, error) {
	// Перенастройка: показать, что уже задано, и одним Enter оставить как есть.
	if len(current) > 0 {
		p.Note("currently configured:")
		for _, path := range current {
			p.Note("  %s", path)
		}
		keep, err := p.YesNo("Keep these directories?", true)
		if err != nil {
			return nil, err
		}
		if keep {
			return current, nil
		}
	}

	var out []string
	for {
		prompt := question
		if len(out) > 0 {
			prompt = "Another directory (leave empty when done)"
		}

		raw, err := p.Line(prompt, "")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(raw) == "" {
			return out, nil
		}

		path, err := cleanPath(raw)
		if err != nil {
			p.Problem("%v", err)
			continue
		}
		if path != strings.TrimSpace(raw) {
			p.Note("using %s", path)
		}

		if err := checkMediaDir(path); err != nil {
			p.Result(false, "%s — %v", path, err)
			again, aerr := p.YesNo("Try another path?", true)
			if aerr != nil {
				return nil, aerr
			}
			if again {
				continue
			}
			return out, nil
		}

		// Конфликт с уже собранными путями — не повод прекращать ввод:
		// человек просто назвал не тот каталог, и следующая попытка вполне
		// может оказаться верной.
		if err := accept(path); err != nil {
			p.Result(false, "%v", err)
			continue
		}

		p.Result(true, "%s", path)
		out = append(out, path)
	}
}
