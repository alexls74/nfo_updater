// internal/setup/keys.go
package setup

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"nfo_updater/internal/providers"
)

// keyCheckTimeout — общий срок на проверку всех ключей.
//
// Проверка последовательна и стоит по одному запросу на ключ, так что при
// трёх сервисах и паре запасных ключей это единицы секунд. Полминуты —
// потолок на случай медленной сети, после которого честнее сказать "сервис
// не отвечает", чем держать человека перед немым экраном.
const keyCheckTimeout = 30 * time.Second

// askKeys — секция ключей API.
//
// Все три сервиса обязательны, и это не прихоть: иначе пришлось бы городить
// запасные ветки на каждое сочетание отсутствующих провайдеров. Пропустить
// секцию нельзя, но и держать человека силой незачем — Ctrl+D прерывает
// мастер, ничего не записав.
func askKeys(ctx context.Context, p *Prompt, values map[string]string) error {
	p.Section("RATING SERVICES")
	p.Note("All three services are required. Each has a free tier that is more")
	p.Note("than enough for a home library.")

	omdb, err := askKeyList(p, "omdb", splitCSV(values["OMDB_API_KEYS"]))
	if err != nil {
		return err
	}
	mdblist, err := askKeyList(p, "mdblist", splitCSV(values["MDBLIST_API_KEYS"]))
	if err != nil {
		return err
	}
	tmdb, err := askKeyList(p, "tmdb", splitCSV(values["TMDB_API_KEY"]))
	if err != nil {
		return err
	}

	for {
		ok, err := verifyKeys(ctx, p, omdb, mdblist, tmdb)
		if err != nil {
			return err
		}
		if ok {
			break
		}

		choice, err := p.Choice("What would you like to do?", []Option{
			{Key: "r", Label: "enter the keys again"},
			{Key: "k", Label: "keep them anyway (the service may be down right now)"},
			{Key: "a", Label: "abort setup"},
		}, 0)
		if err != nil {
			return err
		}
		if choice == 1 {
			// Осознанное согласие. Прогон всё равно проверяет ключи на старте
			// и скажет о негодном в логе — так что молчаливой поломки не будет.
			break
		}
		if choice == 2 {
			return ErrAborted
		}

		if omdb, err = askKeyList(p, "omdb", nil); err != nil {
			return err
		}
		if mdblist, err = askKeyList(p, "mdblist", nil); err != nil {
			return err
		}
		if tmdb, err = askKeyList(p, "tmdb", nil); err != nil {
			return err
		}
	}

	values["OMDB_API_KEYS"] = strings.Join(omdb, ",")
	values["MDBLIST_API_KEYS"] = strings.Join(mdblist, ",")
	values["TMDB_API_KEY"] = strings.Join(tmdb, ",")
	return nil
}

// askKeyList спрашивает ключи одного сервиса.
//
// У OMDb и MDBList ключей может быть несколько: они расходуются по очереди,
// когда суточный лимит предыдущего выбран. У TMDb суточного лимита нет
// вовсе, поэтому второй ключ там бессмыслен, и предлагать его не нужно.
//
// current непуст только при перенастройке: тогда мастер показывает имеющееся
// и позволяет оставить всё одним Enter. При повторном заходе после неудачной
// проверки сюда передаётся nil — переспрашивать заново там и есть смысл.
func askKeyList(p *Prompt, provider string, current []string) ([]string, error) {
	help, ok := providers.KeyHelpFor(provider)
	if !ok {
		// Расхождение между списком провайдеров и справкой — ошибка в коде,
		// а не в настройке, и молчать о ней нельзя.
		return nil, fmt.Errorf("internal error: no key help for provider %q", provider)
	}
	multiple := provider != "tmdb"

	p.Blank()
	p.Note("%s — %s", help.Display, help.URL)
	for _, n := range help.Note {
		p.Note("%s", n)
	}

	if len(current) > 0 {
		for _, k := range current {
			p.Note("  current: %s", k)
		}
		keep, err := p.YesNo("Keep it?", true)
		if err != nil {
			return nil, err
		}
		if keep {
			return current, nil
		}
	}

	var out []string
	for {
		question := help.Display + " key"
		if len(out) > 0 {
			question = "Another " + help.Display + " key"
		}
		key, err := p.Required(question, "")
		if err != nil {
			return nil, err
		}
		out = append(out, strings.TrimSpace(key))

		if !multiple {
			return out, nil
		}
		more, err := p.YesNo("Add another "+help.Display+" key?", false)
		if err != nil {
			return nil, err
		}
		if !more {
			return out, nil
		}
	}
}

// verifyKeys проверяет все ключи по сети и печатает результат построчно.
//
// Возвращает true, если годны все. Различие между негодным ключом и
// недоступным сервисом сохраняется в тексте: первое чинится вводом
// правильного ключа, второе пройдёт само, и предлагать в этом случае
// перенабрать ключи было бы вредным советом.
func verifyKeys(ctx context.Context, p *Prompt, omdb, mdblist, tmdb []string) (bool, error) {
	p.Blank()
	p.Text("Checking the keys...")

	ctx, cancel := context.WithTimeout(ctx, keyCheckTimeout)
	defer cancel()

	client := providers.NewHTTPClient(keyCheckTimeout)
	checkers := providers.NewKeyCheckers(omdb, mdblist, strings.Join(tmdb, ""), client)

	allOK := true
	for _, st := range providers.CheckAll(ctx, checkers) {
		label := keyLabel(st.Provider, st.KeyIndex)
		if st.OK {
			p.Result(true, "%s accepted", label)
			continue
		}
		allOK = false
		if providers.IsNetworkError(st.Err) {
			p.Result(false, "%s could not be checked: the service did not answer", label)
			continue
		}
		p.Result(false, "%s was rejected: %v", label, st.Err)
	}
	return allOK, nil
}

// keyLabel — как ключ называется в отчёте. Номер печатается только там,
// где ключей может быть несколько: "TMDb key #1" сбивало бы с толку.
func keyLabel(provider string, index int) string {
	display := provider
	if help, ok := providers.KeyHelpFor(provider); ok {
		display = help.Display
	}
	if provider == "tmdb" {
		return display + " key"
	}
	return display + " key #" + strconv.Itoa(index+1)
}

// splitCSV разбирает список значений из конфига. Пустая строка даёт nil,
// а не срез с одним пустым элементом.
func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
