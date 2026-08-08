// internal/logging/logger.go
package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Logger разделяет сообщения на два уровня:
//   - Detail — рутинные события ("не изменилось", "взято из кэша", успешная
//     запись обычного файла). ВЫКЛЮЧЕНЫ по умолчанию: при демоне, работающем
//     месяцами без присмотра, тысячи строк "всё в порядке" только мешают
//     увидеть настоящую проблему. Включаются через LOG_VERBOSE=yes —
//     временно, для диагностики конкретной ситуации.
//   - Event — значимые события (ошибки, pending, бэкапы, фолбэк рейтинга,
//     circuit breaker, старт/конец прогона, reload).
//     Пишутся всегда, и в файл, и в консоль (stdout/docker logs/journald).
type Logger struct {
	file    io.Writer
	console io.Writer

	detail      *log.Logger
	event       *log.Logger
	consoleOnly *log.Logger

	consoleTTY bool
	verbose    bool
}

// New создаёт Logger поверх двух приёмников, любой из которых может быть nil:
// fileWriter отсутствует при LOG_ENABLED=no, consoleWriter — если вывод
// в консоль не нужен.
//
// Detail пишется ИСКЛЮЧИТЕЛЬНО в файл. Если файлового лога нет, Detail
// не пишется никуда, даже при LOG_VERBOSE=yes: LOG_ENABLED=no — это отказ
// от подробностей как таковых, и перенаправлять тысячи рутинных строк
// в консоль или journald вместо файла означало бы забить системный журнал
// ровно тем, от чего пользователь отказался.
func New(fileWriter, consoleWriter io.Writer, verbose bool) *Logger {
	detailWriter := io.Writer(io.Discard)
	if fileWriter != nil {
		detailWriter = fileWriter
	}

	var eventWriter io.Writer
	switch {
	case fileWriter != nil && consoleWriter != nil:
		eventWriter = io.MultiWriter(fileWriter, consoleWriter)
	case fileWriter != nil:
		eventWriter = fileWriter
	case consoleWriter != nil:
		eventWriter = consoleWriter
	default:
		eventWriter = io.Discard
	}

	consoleOnlyWriter := io.Writer(io.Discard)
	if consoleWriter != nil {
		consoleOnlyWriter = consoleWriter
	}

	return &Logger{
		file:        fileWriter,
		console:     consoleWriter,
		detail:      log.New(detailWriter, "", log.LstdFlags),
		event:       log.New(eventWriter, "", log.LstdFlags),
		consoleOnly: log.New(consoleOnlyWriter, "", log.LstdFlags),
		consoleTTY:  isTerminal(consoleWriter),
		verbose:     verbose,
	}
}

// isTerminal отличает живой терминал от трубы, сокета или файла.
//
// Нужно ровно для одного решения — печатать ли многострочную сводку
// в консоль. Под systemd stdout это сокет journald, и блок с рамкой
// превратился бы там в десяток отдельных записей, часть из которых
// пустые. У терминала же обратная логика: его читает человек, и рамка
// ему помогает.
//
// Проверка через ModeCharDevice — стандартный для Go способ, не требующий
// внешних зависимостей.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Detail — рутинное событие. Ничего не делает, если LOG_VERBOSE=no
// (значение по умолчанию) или если файловый лог отключён — см. New.
func (l *Logger) Detail(format string, args ...any) {
	if !l.verbose {
		return
	}
	l.detail.Printf(format, args...)
}

// Event — значимое событие, всегда в файл и в консоль.
func (l *Logger) Event(format string, args ...any) {
	l.event.Printf(format, args...)
}

// Summary печатает итог прогона в двух видах сразу.
//
// block — многострочная сводка с рамкой, пишется БЕЗ метки времени и
// только в файл: метка от log.Printf ставится в начало всего сообщения
// и превращала бы первую строку блока в одинокую дату.
//
// line — та же сводка одной строкой вида key=value. Уходит в консоль,
// то есть в journald или docker logs, где многострочные блоки неуместны:
// каждая строка там становится отдельной записью, пустые строки — пустыми
// записями, а grep по такому не работает вовсе.
//
// Исключение — интерактивный запуск: если консоль это терминал, человек
// читает вывод глазами, и ему полезнее блок. Тогда однострочный вариант
// не печатается, чтобы не дублировать одно и то же дважды подряд.
func (l *Logger) Summary(block, line string) {
	if l.file != nil {
		fmt.Fprintf(l.file, "\n%s\n\n", block)
	}
	if l.console == nil {
		return
	}
	if l.consoleTTY {
		fmt.Fprintf(l.console, "\n%s\n\n", block)
		return
	}
	l.consoleOnly.Print(line)
}

// Имя файла лога: префикс приложения + сортируемый timestamp. Формат времени
// тот же, что у архивов бэкапов, поэтому лексикографическая сортировка имён
// совпадает с сортировкой по времени создания.
const (
	logFilePrefix     = "nfo_updater_"
	logFileTimeFormat = "2006-01-02_15-04-05"
	logFileExt        = ".log"
)

// reLogFileName ограничивает ротацию файлами, которые создали мы сами.
// LOG_DIR вполне может оказаться общим каталогом вроде /var/log, и удалять
// оттуда чужие файлы по счётчику недопустимо.
var reLogFileName = regexp.MustCompile(
	`^` + regexp.QuoteMeta(logFilePrefix) + `\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}` + regexp.QuoteMeta(logFileExt) + `$`)

// RunLog — файл лога одного прогона.
type RunLog struct {
	file *os.File
	path string
}

// OpenRunLog создаёт каталог логов при необходимости, заводит файл текущего
// прогона, пишет в него шапку и удаляет лишние старые логи.
//
// Ротация выполняется ПОСЛЕ создания нового файла, чтобы limit означал
// "столько файлов лежит на диске" — ровно как BACKUP_LIMIT для архивов.
// Ошибка ротации не отменяет уже открытый лог: RunLog возвращается вместе
// с ошибкой, и вызывающий код может записать жалобу в этот же файл вместо
// того, чтобы падать из-за неудалённого старья.
func OpenRunLog(dir string, limit int, at time.Time, header string) (*RunLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir %s: %w", dir, err)
	}

	path := filepath.Join(dir, logFilePrefix+at.Format(logFileTimeFormat)+logFileExt)
	// O_APPEND, а не усечение: два прогона в одну и ту же секунду невозможны
	// (их разводит flock), но если файл с таким именем уже есть по любой
	// другой причине, дописать безопаснее, чем затереть.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create log file %s: %w", path, err)
	}

	if header != "" {
		if _, err := fmt.Fprintf(f, "%s\n\n", header); err != nil {
			f.Close()
			return nil, fmt.Errorf("write log header: %w", err)
		}
	}

	rl := &RunLog{file: f, path: path}
	if err := rotateLogs(dir, limit); err != nil {
		return rl, fmt.Errorf("rotate logs: %w", err)
	}
	return rl, nil
}

// Writer возвращает приёмник для New(). Рассчитан на вызов только
// у открытого RunLog.
func (r *RunLog) Writer() io.Writer { return r.file }

// Path — путь созданного файла, нужен для сообщения в консоль.
func (r *RunLog) Path() string { return r.path }

func (r *RunLog) Close() error { return r.file.Close() }

// rotateLogs оставляет в dir только последние limit файлов лога
// (0 = безлимитно, ничего не удаляем).
func rotateLogs(dir string, limit int) error {
	if limit <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	var matched []string
	for _, e := range entries {
		if e.IsDir() || !reLogFileName.MatchString(e.Name()) {
			continue
		}
		matched = append(matched, e.Name())
	}
	if len(matched) <= limit {
		return nil
	}

	sort.Strings(matched)
	for _, name := range matched[:len(matched)-limit] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
