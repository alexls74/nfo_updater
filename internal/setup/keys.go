// internal/setup/keys.go
package setup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nfo_updater/internal/providers"
)

// keyCheckTimeout — срок на проверку ОДНОГО ключа.
//
// Ключи проверяются по одному, сразу после ввода, поэтому и таймаут теперь
// поштучный. Пятнадцати секунд хватает даже на медленной сети; дольше
// держать человека перед немым экраном нечестно, а вывод "сервис не ответил"
// ничего не портит — ключ в этом случае принимается как есть.
const keyCheckTimeout = 15 * time.Second

// keyProviders — порядок опроса сервисов. Отдельный слайс, а не обход
// справки, чтобы порядок диалога не зависел от внутреннего устройства
// пакета providers.
var keyProviders = []string{"omdb", "mdblist", "tmdb"}

// keyUsage — сколько запросов суточной квоты израсходовала проверка,
// в разрезе сервиса и самого ключа.
//
// Ключом внутренней карты служит СТРОКА ключа, а не его номер: при
// перенаборе список меняется, и номера съезжают. Разложить расход по
// номерам можно только в самом конце, когда набор окончателен, — этим
// занимается byIndex.
type keyUsage map[string]map[string]int

func (u keyUsage) add(provider, key string, requests int) {
	if requests == 0 {
		return
	}
	if u[provider] == nil {
		u[provider] = make(map[string]int)
	}
	u[provider][key] += requests
}

// total — сколько запросов израсходовано у одного сервиса. Нужно сводке.
func (u keyUsage) total(provider string) int {
	sum := 0
	for _, n := range u[provider] {
		sum += n
	}
	return sum
}

// byIndex переводит расход из «по ключам» в «по номерам ключей», как его
// хранит база. final — итоговые списки ключей по сервисам.
func (u keyUsage) byIndex(final map[string][]string) map[string][]int {
	if len(u) == 0 {
		return nil
	}
	out := make(map[string][]int, len(u))
	for provider, keys := range final {
		spent := u[provider]
		if len(spent) == 0 {
			continue
		}
		counts := make([]int, len(keys))
		for i, k := range keys {
			counts[i] = spent[k]
		}
		out[provider] = counts
	}
	return out
}

// askKeys — секция ключей API.
//
// Каждый сервис обязателен, и это не прихоть: иначе пришлось бы городить
// запасные ветки на каждое сочетание отсутствующих провайдеров. Пропустить
// секцию нельзя, но и держать человека силой незачем — Ctrl+D прерывает
// мастер, ничего не записав.
func askKeys(ctx context.Context, p *Prompt, values map[string]string) (map[string][]int, error) {
	p.Section("RATING SERVICES")
	p.Note("Every service listed below is required. Each has a free tier that is")
	p.Note("more than enough for a home library.")
	p.Note("Each key is checked against its service as soon as you enter it.")

	usage := make(keyUsage)
	final := make(map[string][]string, len(keyProviders))

	for _, provider := range keyProviders {
		help, ok := providers.KeyHelpFor(provider)
		if !ok {
			// Расхождение между списком сервисов и справкой — ошибка
			// в коде, а не в настройке, и молчать о ней нельзя.
			return nil, fmt.Errorf("internal error: no key help for provider %q", provider)
		}
		keys, err := askKeysFor(ctx, p, help, splitCSV(values[help.Setting]), usage)
		if err != nil {
			return nil, err
		}
		final[provider] = keys
		values[help.Setting] = strings.Join(keys, ",")
	}

	usage.report(p)
	return usage.byIndex(final), nil
}

// report — строка о том, во что обошлась проверка.
//
// Печатается только про OMDb и только если что-то потрачено: у MDBList
// проверка идёт через /user, у TMDb через /authentication, и обе бесплатны.
// Молчать об этом нельзя — тысяча запросов в сутки не бесконечна, и человек
// вправе знать, что несколько из них ушли на настройку.
//
// Считать израсходованное в сумме по всем ключам НЕЛЬЗЯ, и первая версия
// этого текста именно этим и грешила: "2 of the 1000 daily requests have
// been used" при двух ключах читается так, будто общий запас на всех —
// тысяча. Между тем квота у OMDb привязана к ключу, у каждого своя тысяча,
// и потрачено было по одному запросу с каждого.
func (u keyUsage) report(p *Prompt) {
	spent := u.total("omdb")
	if spent == 0 {
		return
	}
	help, ok := providers.KeyHelpFor("omdb")
	if !ok {
		return
	}

	p.Blank()
	p.Note("Checking an OMDb key costs one real request: the service has no free")
	p.Note("way to verify one. Every key has its own allowance of %d per day.",
		help.DailyLimit)
	// Будущее время здесь точное, а не осторожное: расход списывается
	// в базу уже после записи конфига, и человек, отказавшийся на сводке,
	// не получит в базе ничего.
	keys := len(u["omdb"])
	switch {
	case keys == 1 && spent == 1:
		p.Note("One request has been used, and will be counted in the database")
		p.Note("once the configuration is written.")
	case keys == spent:
		p.Note("One request from each of the %d keys has been used, and they will be", keys)
		p.Note("counted in the database once the configuration is written.")
	default:
		// Ключ мог проверяться дважды: сначала как унаследованный из
		// конфига, потом введённый заново.
		p.Note("%d requests across %d %s have been used, and they will be counted",
			spent, keys, plural("key", "keys", keys))
		p.Note("in the database once the configuration is written.")
	}
}

// askKeysFor собирает ключи одного сервиса.
//
// Устройство цикла отвечает сразу на несколько замечаний, всплывших при
// испытаниях:
//
//   - Ключ проверяется немедленно. Негодный переспрашивается на месте, пока
//     человек ещё смотрит на страницу сайта, откуда его скопировал, а уже
//     принятые ключи при этом не трогаются и повторно не проверяются. Раньше
//     проверка шла пакетом в конце секции, и один неверный ключ отправлял
//     вводить заново весь набор — заодно повторно тратя квоту OMDb на те
//     ключи, что уже были признаны годными.
//   - Вопрос один, а не два. Раньше после каждого ключа спрашивали «добавить
//     ещё?», и только потом просили сам ключ; теперь пустая строка означает
//     «больше нет».
//   - Вставленная строка с запятыми разбирается молча. Рекламировать это
//     незачем — лишний способ ошибиться, — но и отвергать строку,
//     скопированную из конфига, было бы недружелюбно.
func askKeysFor(ctx context.Context, p *Prompt, help providers.KeyHelp,
	current []string, usage keyUsage) ([]string, error) {

	p.Blank()
	p.Text("%s — %s", help.Display, help.URL)
	if s := help.QuotaNote(); s != "" {
		p.Note("%s", s)
	}
	if help.Multi {
		p.Note("More than one key may be given: they are used in turn as each one")
		p.Note("runs out. One is enough to start with.")
	}
	for _, n := range help.Note {
		p.Note("%s", n)
	}

	var accepted []string

	// При перенастройке имеющиеся ключи предлагается оставить — но проверить
	// их всё равно надо: ключ мог протухнуть с прошлого раза, и промолчать
	// об этом значит отдать человеку конфиг, который сломается на первом же
	// прогоне. Непрошедшие в accepted не попадают и переспрашиваются ниже.
	if len(current) > 0 {
		for _, k := range current {
			p.Note("  current: %s", k)
		}
		keep, err := p.YesNo("Keep "+plural("it", "them", len(current))+"?", true)
		if err != nil {
			return nil, err
		}
		if keep {
			for _, k := range current {
				ok, err := checkAndReport(ctx, p, help, k, usage)
				if err != nil {
					return nil, err
				}
				if ok {
					accepted = append(accepted, k)
				}
			}
		}
	}

	for {
		if len(accepted) > 0 && !help.Multi {
			return accepted, nil
		}

		question := help.Display + " key"
		hint := ""
		if len(accepted) > 0 {
			question = "Add another " + help.Display + " key"
			hint = "leave empty when done"
		}

		answer, err := p.LineWithHint(question, hint)
		if err != nil {
			return nil, err
		}

		if strings.TrimSpace(answer) == "" {
			if len(accepted) > 0 {
				return accepted, nil
			}
			// Ни одного годного ключа. Молча пропустить нельзя: без ключа
			// сервис не работает, а прогон откажется стартовать с ошибкой
			// валидации, которая всплывёт через несколько экранов отсюда
			// и уже без объяснения, где её заработали.
			p.Problem("at least one %s key is required: the program cannot work without it",
				help.Display)
			choice, err := p.Choice("What would you like to do?", []Option{
				{Key: "e", Label: "enter a key now"},
				{Key: "a", Label: "leave setup without changing anything"},
			}, 0)
			if err != nil {
				return nil, err
			}
			if choice == 1 {
				return nil, ErrAborted
			}
			continue
		}

		for _, candidate := range splitCSV(answer) {
			if contains(accepted, candidate) {
				p.Problem("that key has already been entered")
				continue
			}
			keep, err := checkAndReport(ctx, p, help, candidate, usage)
			if err != nil {
				return nil, err
			}
			if keep {
				accepted = append(accepted, candidate)
			}
			if len(accepted) > 0 && !help.Multi {
				break
			}
		}
	}
}

// checkAndReport проверяет ключ по сети, печатает результат и, если проверить
// не удалось, спрашивает, менять ли ключ.
//
// Возвращает true, если ключ следует принять. Недоступный сервис сам по себе
// не повод браковать ключ: о ключе это не говорит ничего. Но и проходить
// молча такая проверка не должна — человек, ошибившийся в ключе, иначе узнает
// об этом только на первом прогоне. Поэтому вопрос с умолчанием «оставить»:
// один Enter для того, у кого просто нет сети, и возможность исправить для
// того, кто подозревает опечатку.
func checkAndReport(ctx context.Context, p *Prompt, help providers.KeyHelp,
	key string, usage keyUsage) (bool, error) {

	ctx, cancel := context.WithTimeout(ctx, keyCheckTimeout)
	defer cancel()

	client := providers.NewHTTPClient(keyCheckTimeout)
	res, err := providers.CheckKey(ctx, help.Provider, key, client)
	if err != nil {
		return false, err
	}
	usage.add(help.Provider, key, res.Requests)

	label := help.Display + " key"
	switch res.Verdict {
	case providers.KeyGood:
		p.Result(true, "%s accepted", label)
		return true, nil
	case providers.KeyRejected:
		p.Result(false, "%s was rejected: %s", label, res.Reason)
		return false, nil
	default:
		p.Result(false, "%s could not be checked: %s", label, res.Reason)
		p.Note("That says nothing about the key, only about the service. Keeping it")
		p.Note("means it will be checked again on the first pass.")
		again, err := p.YesNo("Enter a different "+help.Display+" key?", false)
		if err != nil {
			return false, err
		}
		return !again, nil
	}
}

// plural — выбор формы по количеству. Ровно те случаи, ради которых
// заводить зависимость незачем.
func plural(one, many string, n int) string {
	if n == 1 {
		return one
	}
	return many
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// splitCSV разбирает список значений из конфига — и заодно строку, которую
// человек вставил в поле ввода целиком. Пустая строка даёт nil, а не срез
// с одним пустым элементом.
func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
