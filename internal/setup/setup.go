// internal/setup/setup.go
//
// Мастер первичной настройки: флаг --setup.
//
// Написан на Go, а не на shell, по одной причине: всё, что ему нужно, здесь
// уже есть и проверено. Разбор сетевых ошибок медиасерверов, проверка ключей
// по сети, правила раздельности деревьев медиатеки, права на конфиг, запись
// файла из шаблона — переписывать это на POSIX sh значит получить вторую,
// худшую реализацию каждого пункта. Установочному скрипту остаётся то, чего
// программа про себя знать не может: где лежит бинарник и что думает systemd.
//
// Мастер НИЧЕГО не пишет на диск, пока не задан последний вопрос и не
// показана сводка. Прерывание на любом шаге — Ctrl+C, Ctrl+D, отказ
// в подтверждении — оставляет систему нетронутой.
package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nfo_updater/internal/config"
	"nfo_updater/internal/mediaserver"
	"nfo_updater/internal/scheduler"
	"nfo_updater/internal/version"
)

// Result — то, что мастер сообщает вызывающему помимо факта успеха.
type Result struct {
	// ConfigPath — куда записан конфиг.
	ConfigPath string

	// Scheduled — человек захотел, чтобы программа работала сама по
	// расписанию. Установочному скрипту это говорит, что нужна служба.
	//
	// Передаётся наружу отдельным полем, а не вычитывается скриптом из
	// конфига: SCHEDULE непуст и у того, кто запускает программу руками,
	// потому что расписание — про то, КОГДА работать, а не про то, КАК
	// программа оказывается запущена.
	Scheduled bool

	// KeyUsage — сколько запросов суточной квоты израсходовала проверка
	// ключей: имя сервиса, дальше по номеру ключа в итоговом списке.
	//
	// Наружу это уходит потому, что записать расход в базу мастер не может
	// сам. База лежит по пути, который настраивается прямо сейчас, и открыть
	// её раньше записи конфига значило бы завести файл на диске в середине
	// диалога — а мастер до последнего вопроса не меняет на диске ничего.
	// Учесть расход обязательно: иначе счётчик демона с первого же дня
	// врёт на число проверок.
	//
	// Непустым бывает только у OMDb: отдельного способа проверить ключ
	// у сервиса нет, и проверка стоит настоящего запроса. У MDBList и TMDb
	// она бесплатна.
	KeyUsage map[string][]int
}

// Run проводит человека по всем секциям и записывает конфиг.
//
// configPath передаётся готовым: путь по умолчанию вычисляет main.go, он же
// разбирает случай неизвестного домашнего каталога.
func Run(ctx context.Context, configPath string) (Result, error) {
	p, err := Open()
	if err != nil {
		return Result{}, err
	}
	defer p.Close()

	res, err := runSections(ctx, p, configPath)
	if errors.Is(err, ErrAborted) {
		// Прощание печатает мастер, и только он.
		p.Blank()
		p.Text("Setup cancelled. Nothing has been changed.")
	}
	return res, err
}

func runSections(ctx context.Context, p *Prompt, configPath string) (Result, error) {
	values, err := config.ExistingValues(configPath)
	if err != nil {
		return Result{}, fmt.Errorf("read the existing configuration: %w", err)
	}

	// "Уже настроено" и "файл существует" — разные вещи: непустой шаблон,
	// созданный обычным запуском, не является чужой настройкой. Подробности
	// в config.Configured; здесь важно, что от этого флага зависит и текст
	// приветствия, и умолчание вопроса о расписании.
	reconfigure, err := config.Configured(configPath)
	if err != nil {
		return Result{}, fmt.Errorf("read the existing configuration: %w", err)
	}

	p.Blank()
	p.Text("NFO Updater %s — setup", version.Version)
	p.Note("Press Ctrl+C at any time to leave without changing anything.")
	if reconfigure {
		p.Blank()
		p.Note("An existing configuration was found. Current values are offered")
		p.Note("as defaults: press Enter to keep them.")
	}

	scheduled, err := askSchedule(p, values, reconfigure)
	if err != nil {
		return Result{}, err
	}
	if err := askSystemPaths(p, values); err != nil {
		return Result{}, err
	}
	if err := askBackups(p, values); err != nil {
		return Result{}, err
	}
	if err := askLibrary(p, values); err != nil {
		return Result{}, err
	}
	keyUsage, err := askKeys(ctx, p, values)
	if err != nil {
		return Result{}, err
	}
	if err := askServers(ctx, p, values); err != nil {
		return Result{}, err
	}

	if err := confirmAndWrite(p, configPath, values); err != nil {
		return Result{}, err
	}
	return Result{ConfigPath: configPath, Scheduled: scheduled, KeyUsage: keyUsage}, nil
}

// askSchedule — как программа запускается.
//
// Вопрос намеренно про расписание, а не про "службу или вручную". Служба —
// это файл в /etc/systemd/system и состояние systemctl enable, то есть
// свойство системы, которым распоряжается установочный скрипт, а не конфиг.
// Мастер спрашивает то, что действительно ложится в конфиг, а вывод про
// службу делает вызывающий из ответа.
func askSchedule(p *Prompt, values map[string]string, reconfigure bool) (bool, error) {
	p.Section("HOW IT RUNS")
	p.Note("NFO Updater can update the library on a schedule, in the background,")
	p.Note("or only when you start it yourself.")

	// При первой установке умолчание "да": фоновая работа — то, ради чего
	// программу обычно и ставят. При перенастройке умолчанием служит
	// прежний выбор, иначе один Enter молча включил бы то, от чего человек
	// в прошлый раз отказался.
	def := true
	if reconfigure {
		def = strings.TrimSpace(values["SCHEDULE"]) != ""
	}

	auto, err := p.YesNo("Update the library automatically on a schedule?", def)
	if err != nil {
		return false, err
	}
	if !auto {
		values["SCHEDULE"] = ""
		p.Note("Start a pass yourself with: nfo_updater")
		return false, nil
	}

	p.Note("Five cron fields: minute hour day month weekday.")
	p.Note("The default means 03:00 every Monday.")
	for {
		expr, err := p.Required("Schedule", valueOr(values["SCHEDULE"], config.DefaultDaemonSchedule))
		if err != nil {
			return false, err
		}
		if _, err := scheduler.ParseCron(expr); err != nil {
			p.Problem("%v", err)
			continue
		}
		values["SCHEDULE"] = expr
		return true, nil
	}
}

// askBackups — часть секции размещения данных, поэтому без своего заголовка.
//
// Спрашивается отдельно, хотя умолчание разумно, ровно по одной причине:
// бэкап — единственный путь назад после CREW_ORDER_FIX, который переставляет
// теги в уже существующих файлах. Человек, выключающий бэкапы, должен знать,
// что именно он выключает.
func askBackups(p *Prompt, values map[string]string) error {
	p.Blank()
	p.Note("Before changing a file, NFO Updater can put the original into a zip")
	p.Note("archive. This is the only way back: the tag reordering it performs")
	p.Note("cannot be undone by turning the setting off again.")

	on, err := p.YesNo("Keep backups of the original files?", !isNo(values["BACKUP_ENABLED"]))
	if err != nil {
		return err
	}
	if on {
		values["BACKUP_ENABLED"] = "yes"
	} else {
		values["BACKUP_ENABLED"] = "no"
	}
	return nil
}

// askLibrary — секция путей медиатеки.
func askLibrary(p *Prompt, values map[string]string) error {
	p.Section("MEDIA LIBRARY")
	p.Note("Movies and TV shows must live in separate directory trees: neither")
	p.Note("may contain the other. The category of every file, and its place")
	p.Note("inside a backup archive, is decided by which tree it was found in.")
	p.Note("At least one of the two is required.")

	for {
		var tvshows []string

		// accept видит только очередного кандидата, поэтому список уже
		// принятых ведётся здесь и пополняется ровно тогда, когда проверка
		// прошла. Проверять кандидата против другой категории обязательно
		// сразу: узнать о пересечении деревьев в конце секции — значит
		// переспрашивать всё заново.
		var acceptedMovies []string
		knownShows := splitCSV(values["TVSHOWS_PATH"])

		movies, err := askMediaPaths(p, "Movies directory", splitCSV(values["MOVIES_PATH"]),
			func(path string) error {
				candidate := append(append([]string{}, acceptedMovies...), path)
				if err := config.CheckMediaPaths(candidate, knownShows); err != nil {
					return err
				}
				acceptedMovies = candidate
				return nil
			})
		if err != nil {
			return err
		}

		var acceptedShows []string
		tvshows, err = askMediaPaths(p, "TV shows directory", splitCSV(values["TVSHOWS_PATH"]),
			func(path string) error {
				candidate := append(append([]string{}, acceptedShows...), path)
				if err := config.CheckMediaPaths(movies, candidate); err != nil {
					return err
				}
				acceptedShows = candidate
				return nil
			})
		if err != nil {
			return err
		}

		if len(movies) == 0 && len(tvshows) == 0 {
			p.Problem("at least one of the two must be set")
			continue
		}

		values["MOVIES_PATH"] = strings.Join(movies, ",")
		values["TVSHOWS_PATH"] = strings.Join(tvshows, ",")
		return nil
	}
}

// confirmAndWrite показывает итог, спрашивает подтверждение и записывает
// конфиг.
//
// После записи файл читается обратно и проверяется валидацией — той же
// самой, что отработает при настоящем запуске. Это не паранойя: мастер
// оперирует картой значений, а конфиг живёт текстом, и единственный честный
// способ убедиться, что получилось рабочее, — прочитать записанное.
func confirmAndWrite(p *Prompt, configPath string, values map[string]string) error {
	p.Section("SUMMARY")
	p.Text("%s", p.Style.Rule())
	printSummary(p, configPath, values)
	p.Text("%s", p.Style.Rule())
	p.Blank()
	p.Note("Ratings are left at their defaults: IMDb and Rotten Tomatoes critics,")
	p.Note("with IMDb as the main one. Change them in the config file if needed.")

	write, err := p.YesNo("Write this configuration?", true)
	if err != nil {
		return err
	}
	if !write {
		return ErrAborted
	}

	if err := config.WriteConfig(configPath, values); err != nil {
		return err
	}
	p.Result(true, "written to %s", configPath)

	cfg, err := config.Load(configPath)
	if err != nil {
		p.Blank()
		p.Result(false, "the configuration was written, but it is not usable yet:")
		p.Text("  %v", err)
		return err
	}

	p.Blank()
	p.Text("%s", cfg.Describe(configPath))
	return nil
}

// printSummary перечисляет то, о чём мастер спрашивал. Ключи не печатаются
// НИКОГДА: сводку копируют в переписку и баг-репорты.
func printSummary(p *Prompt, configPath string, values map[string]string) {
	current := currentDataPaths(values, config.DefaultDataDir())

	// Путь к ФАЙЛУ базы, а не к каталогу: ровно то, что покажет -v и что
	// увидит человек в логе. Каталогами оперирует секция DATA LOCATION,
	// потому что спрашивает она именно о них, — но сводка обязана совпадать
	// с тем, как программа отчитывается о себе потом.
	databasePath, _, _ := config.DataPathsUnder(current.databaseDir)

	p.Text("config     %s", configPath)
	p.Text("database   %s", databasePath)
	p.Text("logs       %s", current.logs)
	if isNo(values["BACKUP_ENABLED"]) {
		p.Text("backups    disabled")
	} else {
		p.Text("backups    %s", current.backups)
	}

	if v := values["MOVIES_PATH"]; v != "" {
		for i, path := range splitCSV(v) {
			p.Text("%-10s %s", labelOnce("movies", i), path)
		}
	}
	if v := values["TVSHOWS_PATH"]; v != "" {
		for i, path := range splitCSV(v) {
			p.Text("%-10s %s", labelOnce("tvshows", i), path)
		}
	}

	if s := strings.TrimSpace(values["SCHEDULE"]); s != "" {
		p.Text("schedule   %s", s)
	} else {
		p.Text("schedule   on demand only")
	}

	for _, sp := range serverPrompts() {
		if !isYes(values[sp.enabledKey]) {
			continue
		}
		label := sp.name
		if help, ok := mediaserver.ServerHelpFor(sp.name); ok {
			label = strings.ToLower(help.Display)
		}
		p.Text("%-10s %s", label, values[sp.urlKey])
	}
}

// labelOnce печатает подпись только у первой строки списка, чтобы значения
// выстроились в колонку. Тот же приём, что в config.Describe.
func labelOnce(label string, i int) string {
	if i == 0 {
		return label
	}
	return ""
}

// isNo — явное "нет" в булевом параметре конфига. Отличается от !isYes тем,
// что пустое значение сюда не попадает: пустое означает умолчание, а оно
// у BACKUP_ENABLED равно "да".
func isNo(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "no", "0", "off":
		return true
	}
	return false
}
