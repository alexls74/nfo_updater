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

// askOneServer спрашивает адрес, секрет и (для Plex) секции, после чего
// проверяет связь.
//
// Неудачная проверка НЕ отменяет настройку: сервер может быть выключен
// на ночь, а прогон недоступный сервер всё равно не роняет. Поэтому
// развилка из трёх ответов, а не отказ.
func askOneServer(ctx context.Context, p *Prompt, values map[string]string,
	sp serverPrompt, help mediaserver.ServerHelp, client *http.Client) error {

	for {
		p.Note("The address must be reachable from THIS machine, which is not")
		p.Note("necessarily the one your browser runs on.")

		url, err := p.Required(help.Display+" address, including http:// or https://", values[sp.urlKey])
		if err != nil {
			return err
		}
		if err := config.CheckServerURL(sp.urlKey, url); err != nil {
			p.Problem("%v", err)
			continue
		}

		p.Note("Where to get it: %s", help.Where)
		secret, err := p.Required(help.Display+" "+sp.secretLabel, values[sp.secretKey])
		if err != nil {
			return err
		}

		var sections []string
		if sp.name == "plex" {
			sections, err = askPlexSections(p, values["PLEX_SECTION_IDS"])
			if err != nil {
				return err
			}
		}

		// Проверка связи. Ошибки транспорта здесь уже разобраны на
		// осмысленные (см. requestError в mediaserver.go): человек получит
		// не "i/o timeout", а указание, что именно проверять.
		server := sp.build(url, secret, sections, client)
		checkErr := server.Ping(ctx)

		if checkErr == nil {
			p.Result(true, "%s answered and accepted the %s", help.Display, sp.secretLabel)
			values[sp.enabledKey] = "yes"
			values[sp.urlKey] = url
			values[sp.secretKey] = secret
			if sp.name == "plex" {
				values["PLEX_SECTION_IDS"] = strings.Join(sections, ",")
			}
			return nil
		}

		p.Result(false, "%v", checkErr)
		choice, err := p.Choice("What would you like to do?", []Option{
			{Key: "r", Label: "re-enter the address and the " + sp.secretLabel},
			{Key: "k", Label: "keep these settings anyway (the server may simply be off right now)"},
			{Key: "d", Label: "do not use " + help.Display},
		}, 0)
		if err != nil {
			return err
		}

		switch choice {
		case 0:
			continue
		case 1:
			values[sp.enabledKey] = "yes"
			values[sp.urlKey] = url
			values[sp.secretKey] = secret
			if sp.name == "plex" {
				values["PLEX_SECTION_IDS"] = strings.Join(sections, ",")
			}
			return nil
		default:
			values[sp.enabledKey] = "no"
			return nil
		}
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
