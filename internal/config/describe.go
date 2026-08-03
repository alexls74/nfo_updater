// internal/config/describe.go
package config

import (
	"fmt"
	"strings"

	"nfo_updater/internal/version"
)

// describeLabelWidth — ширина колонки подписей в выводе Describe.
// Рассчитана на самую длинную подпись ("database"), плюс пробел-разделитель.
const describeLabelWidth = 9

// Describe возвращает многострочную сводку: версия бинарника и все пути,
// с которыми он сейчас работает. Один и тот же текст показывается по флагу
// -v и пишется в начало каждого файла лога — чтобы по логу недельной
// давности можно было понять, какая версия и какая конфигурация его писали,
// не гадая, что с тех пор поменялось в config.conf.
//
// Шапка (название и версия) формируется ЗДЕСЬ, а не в main.go: -v и шапка
// лога обязаны выглядеть одинаково, иначе по логу нельзя будет сверить
// версию с тем, что показывает работающий бинарник.
//
// configPath передаётся аргументом: сам Config не помнит, откуда он прочитан
// (путь может быть переопределён флагом --config), это знает только main.go.
func (c *Config) Describe(configPath string) string {
	groups := [][]string{
		{fmt.Sprintf("NFO Updater\nVersion %s • %s", version.Version, version.BuildDate)},
	}

	system := []string{
		describeLine("config", configPath),
		describeLine("database", c.DatabasePath),
	}
	// Строка logs опускается при LOG_ENABLED=no — так же, как backup при
	// BACKUP_ENABLED=no: не показываем путь, по которому ничего не появится.
	// В шапке самого файла лога она поэтому есть всегда, что и требовалось.
	if c.LogEnabled {
		system = append(system, describeLine("logs", c.LogDir))
	}
	groups = append(groups, system)

	// Пути медиатеки печатаются, только если заданы: у только что созданного
	// конфига их ещё нет, и -v в этот момент должен спокойно показать, куда
	// программа смотрит по умолчанию, а не ругаться на незаполненность.
	var sources []string
	sources = append(sources, describeList("movies", c.MoviesPaths)...)
	sources = append(sources, describeList("tvshows", c.TVShowsPaths)...)
	groups = append(groups, sources)

	if c.BackupEnabled {
		groups = append(groups, []string{describeLine("backup", c.BackupDir)})
	}

	groups = append(groups, c.describeMediaServers())

	var blocks []string
	for _, g := range groups {
		if len(g) > 0 {
			blocks = append(blocks, strings.Join(g, "\n"))
		}
	}
	return strings.Join(blocks, "\n\n")
}

// describeMediaServers перечисляет ТОЛЬКО включённые серверы и ТОЛЬКО их
// адреса.
//
// Выключенные не показываются по тому же правилу, что logs и backup:
// обновление библиотеки — необязательная возможность, и три строки
// "disabled" у тех, кому она не нужна, лишь загромождают вывод. Пустой
// список групп Describe отбрасывает сам, так что отдельной проверки
// здесь не требуется.
//
// Ключи и токен не выводятся НИКОГДА. Этот же текст ложится в шапку каждого
// файла лога, а логи копируют в баг-репорты и в переписку — секрету там
// делать нечего.
func (c *Config) describeMediaServers() []string {
	var out []string
	if c.EmbyEnabled {
		out = append(out, describeLine("emby", c.EmbyURL))
	}
	if c.JellyfinEnabled {
		out = append(out, describeLine("jellyfin", c.JellyfinURL))
	}
	if c.PlexEnabled {
		out = append(out, describeLine("plex", c.PlexURL+plexSectionsNote(c.PlexSectionIDs)))
	}
	return out
}

// plexSectionsNote поясняет, что именно будет сканироваться. Без этого
// по выводу не отличить "обновится всё" от "обновится вот эта одна секция",
// а разница существенная.
func plexSectionsNote(ids []string) string {
	if len(ids) == 0 {
		return "  (all movie and TV show sections)"
	}
	return fmt.Sprintf("  (sections: %s)", strings.Join(ids, ", "))
}

// describeLine — строка "подпись<отступ>значение".
func describeLine(label, value string) string {
	return fmt.Sprintf("%-*s %s", describeLabelWidth, label, value)
}

// describeList печатает список путей: первый — с подписью, остальные —
// тем же отступом без подписи, чтобы значения выстроились в колонку.
// Пустой список не печатается вовсе (категория может быть не задана).
func describeList(label string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for i, p := range paths {
		if i == 0 {
			out = append(out, describeLine(label, p))
			continue
		}
		out = append(out, describeLine("", p))
	}
	return out
}
