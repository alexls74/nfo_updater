// internal/setup/servers.go
package setup

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"nfo_updater/internal/config"
	"nfo_updater/internal/mediaserver"
)

// serverPrompt — один медиасервер в опроснике. Таблица вместо трёх похожих
// блоков кода: серверы отличаются только именами параметров, названием
// секрета и конструктором.
type serverPrompt struct {
	name string // совпадает с ServerHelp.Name и с Server.Name()

	enabledKey string
	urlKey     string
	secretKey  string

	// secretLabel — как секрет называется у этого сервера. У Plex это токен,
	// а не ключ, и спрашивать про "API key" там значило бы отправить человека
	// искать несуществующий пункт меню.
	secretLabel string

	// build собирает готовый Server для проверки связи. sections заполнены
	// только у Plex, остальные их игнорируют.
	build func(url, secret string, sections []string, client *http.Client) mediaserver.Server
}

func serverPrompts() []serverPrompt {
	return []serverPrompt{
		{
			name:        "emby",
			enabledKey:  "EMBY_ENABLED",
			urlKey:      "EMBY_URL",
			secretKey:   "EMBY_API_KEY",
			secretLabel: "API key",
			build: func(url, secret string, _ []string, c *http.Client) mediaserver.Server {
				return mediaserver.NewEmby(url, secret, c)
			},
		},
		{
			name:        "jellyfin",
			enabledKey:  "JELLYFIN_ENABLED",
			urlKey:      "JELLYFIN_URL",
			secretKey:   "JELLYFIN_API_KEY",
			secretLabel: "API key",
			build: func(url, secret string, _ []string, c *http.Client) mediaserver.Server {
				return mediaserver.NewJellyfin(url, secret, c)
			},
		},
		{
			name:        "plex",
			enabledKey:  "PLEX_ENABLED",
			urlKey:      "PLEX_URL",
			secretKey:   "PLEX_TOKEN",
			secretLabel: "authentication token",
			build: func(url, secret string, sections []string, c *http.Client) mediaserver.Server {
				return mediaserver.NewPlex(url, secret, sections, c)
			},
		},
	}
}

// isYes — разбор булева значения из конфига для подстановки в умолчание
// вопроса. Умолчание у всех трёх параметров одно и то же — "выключено", —
// поэтому отдельного аргумента def здесь, в отличие от config.parseBool,
// не нужно: любое нераспознанное значение означает "нет".
func isYes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "1", "on":
		return true
	}
	return false
}

// askServers — секция медиасерверов.
//
// Секция целиком необязательна, и это надо сказать вслух: обновление
// библиотеки лишь ускоряет появление новых рейтингов, а без него они
// появятся на плановом сканировании самого сервера. Человек, у которого
// Kodi читает файлы напрямую, должен спокойно проскочить всю секцию.
//
// Значения пишутся прямо в values. Выключенный сервер сохраняет свой адрес
// и ключ: пригодятся, если его когда-нибудь включат обратно, а вреда
// от них никакого — при ENABLED=no ни одна подсистема их не читает.
func askServers(ctx context.Context, p *Prompt, values map[string]string) error {
	p.Section("MEDIA SERVERS")
	p.Note("Optional. A media server rescans its library on its own schedule")
	p.Note("anyway; configuring one here only makes new ratings appear sooner.")
	p.Blank()

	client := mediaserver.NewHTTPClient()

	for _, sp := range serverPrompts() {
		help, ok := mediaserver.ServerHelpFor(sp.name)
		if !ok {
			// Таблица опросника и таблица справки разошлись — это ошибка
			// в коде, а не в настройке, и молчать о ней нельзя.
			return fmt.Errorf("internal error: no help entry for media server %q", sp.name)
		}

		enable, err := p.YesNo("Use "+help.Display+"?", isYes(values[sp.enabledKey]))
		if err != nil {
			return err
		}
		if !enable {
			values[sp.enabledKey] = "no"
			continue
		}

		if err := askOneServer(ctx, p, values, sp, help, client); err != nil {
			return err
		}
	}
	return nil
}

// askOneServer спрашивает адрес, секрет и (для Plex) секции.
//
// Схема ровно та же, что у ключей API, и это не совпадение, а требование:
// человек проходит обе секции подряд, и правила поведения в них не должны
// расходиться.
//
//	введено значение -> оно проверяется -> сервис сказал «нет»: спрашиваем
//	снова, здесь же, пока человек ещё смотрит на то место, откуда значение
//	брал. Сервис не ответил — принимаем как есть и говорим почему.
//
// Меню из трёх ответов, которое стояло здесь раньше, убрано. Оно спрашивало
// про отказ ровно то же, что спрашивает пустой ввод, а про недоступность —
// то, что и так решается само: недоступный сервер не повод бросать настройку,
// прогон его всё равно не роняет.
//
// Разделить «не тот адрес» и «не тот ключ» позволяет mediaserver.Reach: он
// трогает эндпоинт, отвечающий без авторизации, и потому говорит про адрес
// и только про адрес. Раньше неверный адрес выяснялся уже после того, как
// человек сходил в настройки сервера за ключом, и перенабирать предлагалось
// и то и другое.
func askOneServer(ctx context.Context, p *Prompt, values map[string]string,
	sp serverPrompt, help mediaserver.ServerHelp, client *http.Client) error {

	url, err := askServerURL(ctx, p, values, sp, help, client)
	if err != nil {
		return err
	}
	if url == "" {
		values[sp.enabledKey] = "no"
		return nil
	}

	secret, err := askServerSecret(ctx, p, values, sp, help, client, url)
	if err != nil {
		return err
	}
	if secret == "" {
		values[sp.enabledKey] = "no"
		values[sp.urlKey] = url
		return nil
	}

	// Секции спрашиваются последними и только у живого сервера: номер
	// библиотеки бессмысленно уточнять у адреса, который ещё неизвестно чей.
	if sp.name == "plex" {
		sections, err := askPlexSections(p, values["PLEX_SECTION_IDS"])
		if err != nil {
			return err
		}
		values["PLEX_SECTION_IDS"] = strings.Join(sections, ",")
	}

	values[sp.enabledKey] = "yes"
	values[sp.urlKey] = url
	values[sp.secretKey] = secret
	return nil
}

// askServerURL спрашивает адрес и сразу проверяет, кто по нему стоит.
// Пустой ответ означает отказ от этого сервера.
func askServerURL(ctx context.Context, p *Prompt, values map[string]string,
	sp serverPrompt, help mediaserver.ServerHelp, client *http.Client) (string, error) {

	p.Note("The address must be reachable from THIS machine, which is not")
	p.Note("necessarily the one your browser runs on.")
	p.Note("Leave it empty to skip %s after all.", help.Display)

	question := help.Display + " address, including http:// or https://"
	for {
		url, err := p.Line(question, values[sp.urlKey])
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(url) == "" {
			return "", nil
		}
		if err := config.CheckServerURL(sp.urlKey, url); err != nil {
			p.Problem("%v", err)
			continue
		}

		checkErr := mediaserver.Reach(ctx, sp.name, url, client)
		if checkErr == nil {
			p.Result(true, "%s answered at %s", help.Display, url)
			return url, nil
		}
		p.Result(false, "%v", checkErr)
		if mediaserver.IsUnreachable(checkErr) {
			p.Note("That says nothing about the address, only about the server.")
			p.Note("Keeping it means it will be checked again on the first pass.")
			again, err := p.YesNo("Enter a different address?", false)
			if err != nil {
				return "", err
			}
			if !again {
				return url, nil
			}
		}
	}
}

// askServerSecret спрашивает ключ или токен и проверяет его на уже введённом
// адресе. Пустой ответ означает отказ от этого сервера.
func askServerSecret(ctx context.Context, p *Prompt, values map[string]string,
	sp serverPrompt, help mediaserver.ServerHelp, client *http.Client,
	url string) (string, error) {

	p.Note("Where to get it: %s", help.Where)
	p.Note("Leave it empty to skip %s after all.", help.Display)

	question := help.Display + " " + sp.secretLabel
	for {
		secret, err := p.Line(question, values[sp.secretKey])
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(secret) == "" {
			return "", nil
		}

		// Секции Ping не нужны: он спрашивает у Plex список секций,
		// а не содержимое конкретной.
		checkErr := sp.build(url, secret, nil, client).Ping(ctx)
		if checkErr == nil {
			p.Result(true, "%s accepted the %s", help.Display, sp.secretLabel)
			return secret, nil
		}
		p.Result(false, "%v", checkErr)
		if mediaserver.IsUnreachable(checkErr) {
			p.Note("That says nothing about the %s, only about the server. Keeping", sp.secretLabel)
			p.Note("it means it will be checked again on the first pass.")
			again, err := p.YesNo("Enter a different "+sp.secretLabel+"?", false)
			if err != nil {
				return "", err
			}
			if !again {
				return secret, nil
			}
			continue
		}
		// Адрес свою проверку прошёл, и отказ может быть только про секрет.
		// Сказать это прямо стоит: иначе человек пойдёт проверять адрес,
		// с которым всё в порядке.
		p.Note("The address itself answered, so this is about the %s.", sp.secretLabel)
	}
}

// askPlexSections спрашивает номера библиотек Plex.
//
// Пустой ответ — законный и, пожалуй, предпочтительный: тогда программа сама
// спросит у сервера список секций и обновит все библиотеки фильмов и сериалов.
func askPlexSections(p *Prompt, current string) ([]string, error) {
	p.Note("A section id is the number in the address bar of the Plex web app")
	p.Note("when that library is open, not the library name.")
	p.Note("Leave empty to rescan every movie and TV show library.")

	for {
		raw, err := p.Line("Plex section ids, comma-separated", current)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(raw) == "" {
			return nil, nil
		}

		var ids []string
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				ids = append(ids, part)
			}
		}
		if err := config.CheckPlexSectionIDs(ids); err != nil {
			p.Problem("%v", err)
			continue
		}
		return ids, nil
	}
}
