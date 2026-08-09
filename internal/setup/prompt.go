// internal/setup/prompt.go
package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrAborted — человек прервал мастер: Ctrl+D или явный отказ в одном из
// вопросов. Не ошибка в обычном смысле, поэтому вызывающий код обязан
// отличать её от прочих и не печатать как аварию.
//
// Ничего непоправимого к этому моменту не произошло по построению: мастер
// собирает ответы в память и пишет конфиг единственный раз, после последнего
// вопроса и показанной сводки.
var ErrAborted = errors.New("setup was cancelled, nothing has been changed")

// Prompt — диалог с человеком через управляющий терминал.
//
// И чтение, и вывод идут в /dev/tty, а не в stdin/stdout, и это главное,
// ради чего тип существует. Установочный скрипт запускается как
//
//	wget -qO- .../nfo_updater.sh | sh -s -- install
//
// то есть stdin шелла — это канал с телом скрипта, и он же достаётся
// по наследству бинарнику. Чтение оттуда вернуло бы остаток скрипта вместо
// ответа пользователя. Вывод отправлен туда же за компанию: так приглашения
// не пропадают, если stdout кому-то понадобится перенаправить.
type Prompt struct {
	tty   *os.File
	in    *bufio.Reader
	Style *Style
}

// Open захватывает /dev/tty.
//
// Неудача здесь означает, что управляющего терминала у процесса нет вовсе:
// запуск из systemd, из cron, из ssh с -T. Спрашивать некого, и вызывающий
// код должен сказать об этом внятно, а не повиснуть на чтении.
func Open() (*Prompt, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("no terminal is available for interactive setup: %w", err)
	}
	return &Prompt{
		tty:   tty,
		in:    bufio.NewReader(tty),
		Style: NewStyle(colorEnabled(tty)),
	}, nil
}

func (p *Prompt) Close() error {
	return p.tty.Close()
}

// ----------------------------------------------------------------------------
// Вывод
// ----------------------------------------------------------------------------

// Text — обычная строка.
func (p *Prompt) Text(format string, a ...any) {
	fmt.Fprintf(p.tty, format+"\n", a...)
}

// Note — пояснение к вопросу.
//
// Печатается обычной яркостью, БЕЗ приглушения. Приглушение (SGR 2) многие
// терминалы рисуют серым по серому, а на светлой теме оно и вовсе почти
// не читается — а здесь этим цветом набрана добрая половина того, что
// человеку нужно прочесть, чтобы ответить. Пояснение отличается от вопроса
// не яркостью, а начертанием: вопрос жирный, пояснение обычное. Такой
// контраст поддерживают все терминалы и переживает копирование в переписку.
//
// Приглушённым остаётся только то, что действительно второстепенно:
// умолчания в скобках и линии рамок.
//
// Многострочный текст печатается построчно: так он ведёт себя одинаково
// вне зависимости от того, красим мы его или нет.
func (p *Prompt) Note(format string, a ...any) {
	for _, line := range strings.Split(fmt.Sprintf(format, a...), "\n") {
		fmt.Fprintln(p.tty, line)
	}
}

// Section — заголовок секции.
func (p *Prompt) Section(title string) {
	fmt.Fprintln(p.tty, p.Style.Section(title))
}

// Result — строка результата проверки.
func (p *Prompt) Result(ok bool, format string, a ...any) {
	fmt.Fprintln(p.tty, p.Style.Result(ok, format, a...))
}

// Problem — сообщение о негодном вводе, перед повторным вопросом.
func (p *Prompt) Problem(format string, a ...any) {
	fmt.Fprintln(p.tty, p.Style.Result(false, format, a...))
}

// Blank — пустая строка.
func (p *Prompt) Blank() {
	fmt.Fprintln(p.tty)
}

// ----------------------------------------------------------------------------
// Ввод
// ----------------------------------------------------------------------------

// ask печатает вопрос и приглашение. Вопрос и умолчание стоят отдельной
// строкой от приглашения намеренно: пути в умолчаниях длинные, а перенос
// строки посреди введённого текста мешает его править.
func (p *Prompt) ask(question, def string) {
	line := p.Style.Bold(question)
	if def != "" {
		line += " " + p.Style.Dim("["+def+"]")
	}
	fmt.Fprintln(p.tty, line)
	fmt.Fprint(p.tty, p.Style.Accent("> "))
}

// readLine читает одну строку ответа.
//
// Ctrl+D приводит к ErrAborted. Ctrl+C сюда не доходит вовсе: сигнал
// обрабатывается по умолчанию и убивает процесс, что для мастера правильно —
// на диске к этому моменту ничего не изменено.
func (p *Prompt) readLine() (string, error) {
	line, err := p.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("cannot read from the terminal: %w", err)
	}
	// EOF с непустым остатком — это последняя строка без перевода в конце,
	// её надо принять как ответ. EOF на пустом месте — это Ctrl+D.
	if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
		// Курсор стоит сразу за приглашением, и без перевода строки
		// следующий вывод склеился бы с ним.
		fmt.Fprintln(p.tty)
		return "", ErrAborted
	}
	return strings.TrimSpace(line), nil
}

// Line — свободный ответ. Пустой ввод означает согласие с умолчанием.
//
// Ключи API спрашиваются этим же методом, без скрытия ввода: скрытие требует
// возни с termios, а пользы не даёт. Ключ вставляют из буфера обмена, и
// увидеть его на экране полезно — опечатка в невидимой строке обнаружилась бы
// только на сетевой проверке.
func (p *Prompt) Line(question, def string) (string, error) {
	p.ask(question, def)
	ans, err := p.readLine()
	if err != nil {
		return "", err
	}
	if ans == "" {
		return def, nil
	}
	return ans, nil
}

// LineWithHint — свободный ответ без умолчания, но с подсказкой в скобках.
//
// Отличается от Line тем, что подсказка не является значением: пустой ввод
// так и возвращается пустым. Нужно там, где пустая строка сама по себе
// осмысленный ответ, — например «больше ключей нет».
func (p *Prompt) LineWithHint(question, hint string) (string, error) {
	p.ask(question, hint)
	return p.readLine()
}

// Required — как Line, но пустой ответ не принимается, пока нет умолчания.
func (p *Prompt) Required(question, def string) (string, error) {
	for {
		ans, err := p.Line(question, def)
		if err != nil {
			return "", err
		}
		if ans != "" {
			return ans, nil
		}
		p.Problem("a value is required here")
	}
}

// YesNo — вопрос с ответом да/нет. Умолчание видно по заглавной букве
// в скобках, как принято в консольных программах.
func (p *Prompt) YesNo(question string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		p.ask(question, hint)
		ans, err := p.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(ans) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		p.Problem("please answer y or n")
	}
}

// Option — один вариант ответа в Choice.
type Option struct {
	Key   string // что вводит человек: "r", "s", "a"
	Label string // что это значит
}

// Choice — выбор из нескольких вариантов по букве. Используется для режима
// установки и для развилки retry/skip/abort на недоступном пути.
//
// Возвращает индекс выбранного варианта. def — индекс умолчания; значение
// вне диапазона означает, что умолчания нет и пустой ответ не принимается.
func (p *Prompt) Choice(question string, options []Option, def int) (int, error) {
	keys := make([]string, 0, len(options))
	for _, o := range options {
		keys = append(keys, o.Key)
	}
	hint := strings.Join(keys, "/")
	hasDefault := def >= 0 && def < len(options)

	for {
		// Умолчание показывается в тексте вопроса — как во всех остальных
		// вопросах мастера. Раньше оно вычислялось и не печаталось нигде:
		// Enter молча выбирал первый вариант, и промах по клавише обходился
		// в полный перенабор ключей.
		line := p.Style.Bold(question)
		if hasDefault {
			line += " " + p.Style.Dim("["+options[def].Key+"]")
		}
		fmt.Fprintln(p.tty, line)
		for _, o := range options {
			p.Note("  %s  %s", o.Key, o.Label)
		}
		fmt.Fprint(p.tty, p.Style.Accent("> "))

		ans, err := p.readLine()
		if err != nil {
			return 0, err
		}
		if ans == "" {
			if hasDefault {
				return def, nil
			}
			p.Problem("please pick one of: %s", hint)
			continue
		}
		for i, o := range options {
			if strings.EqualFold(ans, o.Key) {
				return i, nil
			}
		}
		p.Problem("%q is not one of: %s", ans, strings.Join(keys, ", "))
	}
}
