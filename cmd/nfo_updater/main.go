// cmd/nfo_updater/main.go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"nfo_updater/internal/config"
	"nfo_updater/internal/daemon"
	"nfo_updater/internal/db"
	"nfo_updater/internal/lock"
	"nfo_updater/internal/logging"
	"nfo_updater/internal/processor"
	"nfo_updater/internal/providers"
	"nfo_updater/internal/scheduler"
	"nfo_updater/internal/version"
)

// defaultConfigPath — путь по умолчанию, ~/.config/nfo_updater/config.conf.
// Переопределяется флагом --config, что нужно и для тестовых запусков, и для
// нескольких экземпляров с разными библиотеками (правда, lock-файл у них
// общий — см. internal/lock).
//
// Вычисляется один раз при старте, а не константой: путь зависит от домашнего
// каталога текущего пользователя. Пустая строка означает, что домашний каталог
// определить не удалось; этот случай разбирается в run().
var defaultConfigPath = config.DefaultConfigPath()

// Коды возврата. Разделены, чтобы systemd и скрипты могли отличать
// "сломан конфиг" (чинить руками) от "прогон не удался" (можно повторить)
// и от "уже работает" (не ошибка вовсе).
const (
	exitOK     = 0
	exitError  = 1
	exitConfig = 2
	exitBusy   = 3
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		daemonMode  bool
		showInfo    bool
		showVersion bool
		checkConfig bool
		showHelp    bool
		configPath  string
	)

	fs := flag.NewFlagSet("nfo_updater", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage печатаем сами, флаговый формат нам не годится
	fs.BoolVar(&daemonMode, "d", false, "")
	fs.BoolVar(&showInfo, "v", false, "")
	// Регистр здесь значащий: пакет flag различает -v и -V. Первый — сводка
	// путей, второй — одна строка с версией.
	fs.BoolVar(&showVersion, "V", false, "")
	fs.BoolVar(&showVersion, "version", false, "")
	// --check-config намеренно не документирован в -h: он служебный, для
	// разработки и разбора проблем. Обычному пользователю хватает того, что
	// обычный запуск сам проверит конфиг и ключи перед началом работы.
	fs.BoolVar(&checkConfig, "check-config", false, "")
	fs.BoolVar(&showHelp, "h", false, "")
	fs.BoolVar(&showHelp, "help", false, "")
	fs.StringVar(&configPath, "config", defaultConfigPath, "")

	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		usage(os.Stderr)
		return exitError
	}
	if showHelp {
		usage(os.Stdout)
		return exitOK
	}
	// --version отвечает раньше всех прочих проверок и ничего не читает
	// с диска. Так флаг работает и на системе, где конфига ещё нет, и на
	// системе, где он испорчен, — а именно там его и спрашивает установочный
	// скрипт, определяя установленную версию перед обновлением.
	if showVersion {
		fmt.Println(version.String())
		return exitOK
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\n\n", fs.Arg(0))
		usage(os.Stderr)
		return exitError
	}

	// Пустой путь означает, что и умолчание не вычислилось, и --config не задан.
	// Единственная причина — неизвестный домашний каталог; дальше идти незачем,
	// иначе ошибка вылезет как невнятное "open : no such file or directory".
	if configPath == "" {
		fmt.Fprintf(os.Stderr, "cannot determine the default configuration path: "+
			"the home directory of the current user is unknown.\n"+
			"Pass the path explicitly: nfo_updater --config /path/to/config.conf\n")
		return exitConfig
	}

	// -v обрабатывается ДО EnsureConfig, и это принципиально: справочные
	// флаги ничего не меняют на диске. Иначе запрос версии на свежей системе
	// заводил бы конфиг и отвечал сообщением о его создании вместо
	// запрошенной информации.
	if showInfo {
		return doShowInfo(configPath)
	}

	// EnsureConfig создаёт конфиг из шаблона при первом запуске и переносит
	// значения при апгрейде. Возвращает ErrConfigCreated, если файла не было:
	// стартовать с пустым шаблоном бессмысленно, нужно дать пользователю
	// его заполнить.
	if _, err := config.EnsureConfig(configPath); err != nil {
		if errors.Is(err, config.ErrConfigCreated) {
			// Справка о ключах печатается сразу: в этот момент человек как раз
			// собирается открыть только что созданный файл, и отправлять его
			// искать три сайта самостоятельно незачем.
			fmt.Printf("A default configuration file has been created at:\n  %s\n\n"+
				"Fill in the API keys and the media library paths, then start nfo_updater again.\n\n"+
				"Where to get the keys:\n%s\n", configPath, providers.FormatKeyHelp("  "))
			return exitConfig
		}
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return exitConfig
	}

	cfg, cfgErr := config.Load(configPath)
	if cfgErr != nil {
		reportConfigError(configPath, cfgErr)
		return exitConfig
	}

	database, err := db.Open(cfg.DatabasePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		return exitError
	}
	defer database.Close()

	// Загрузочный логгер пишет только в консоль: файл лога заводится внутри
	// прогона, после захвата блокировки.
	bootLogger := logging.New(nil, os.Stdout, cfg.LogVerbose)
	runner := processor.NewRunner(cfg, database, configPath, bootLogger)

	ctx := context.Background()

	if checkConfig {
		return doCheckConfig(ctx, cfg, configPath, runner)
	}
	if daemonMode {
		return doDaemon(ctx, cfg, configPath, database, runner, bootLogger)
	}
	return doOneShot(ctx, runner)
}

// doShowInfo — режим -v: версия и пути, с которыми программа работает.
// Ничего не проверяет и ничего не создаёт.
//
// Ошибки валидации здесь молчат намеренно: спрошено "чем ты работаешь",
// а не "всё ли в порядке". Незаполненный конфиг — обычное состояние сразу
// после установки, и ругаться на него в ответ на запрос версии незачем;
// обычный запуск всё равно откажется стартовать и всё объяснит.
func doShowInfo(configPath string) int {
	cfg, err := config.Load(configPath)
	if cfg != nil {
		// Конфиг прочитан. Прошёл он валидацию или нет — нам здесь неважно.
		fmt.Println(cfg.Describe(configPath))
		return exitOK
	}

	if errors.Is(err, os.ErrNotExist) {
		// Файла ещё нет. Заводить его не будем, но показать, куда программа
		// будет складывать базу, логи и бэкапы, полезно — это те же пути,
		// что появятся в конфиге при первом настоящем запуске.
		fmt.Println(config.Defaults().Describe(configPath))
		fmt.Printf("\nno configuration file yet, the paths above are the defaults\n")
		return exitOK
	}

	// Файл есть, но не читается: нет прав, битая ссылка и тому подобное.
	fmt.Printf("NFO Updater\nVersion %s • %s\n", version.Version, version.BuildDate)
	fmt.Fprintf(os.Stderr, "\ncannot read the configuration file: %v\n", err)
	return exitConfig
}

// reportConfigError печатает ошибку конфига и, если дело в незаполненных
// ключах, дописывает справку о том, где их брать.
//
// Лога прогона на этой стадии не существует физически: файл заводится внутри
// Run() после захвата блокировки, а до прогона дело не дошло. Так что для
// демона единственный след — journal, и текст должен быть самодостаточным.
func reportConfigError(configPath string, err error) {
	fmt.Fprintf(os.Stderr, "config error in %s:\n  %v\n", configPath, err)
	if errors.Is(err, config.ErrMissingAPIKeys) {
		fmt.Fprintf(os.Stderr, "\nWhere to get the keys:\n%s\n", providers.FormatKeyHelp("  "))
	}
}

// doCheckConfig — служебный режим --check-config: конфиг уже прочитан и
// провалидирован к этому моменту, остаётся проверить по сети ключи и
// медиасерверы. Файлы медиатеки не трогаются.
//
// Оговорка: проверка ключей OMDb стоит по одному настоящему запросу на ключ
// и честно списывается с суточной квоты — у сервиса нет способа проверить
// ключ бесплатно. У MDBList и TMDb проверка бесплатна.
//
// Недоступный медиасервер кода возврата не меняет — ровно как и во время
// прогона: это предупреждение, а не сломанная настройка.
func doCheckConfig(ctx context.Context, cfg *config.Config, configPath string, runner *processor.Runner) int {
	fmt.Println(cfg.Describe(configPath))
	fmt.Println("\nConfiguration is valid. Checking API keys...")

	if err := runner.CheckKeys(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "key check failed: %v\n", err)
		return exitError
	}

	fmt.Println("\nChecking media servers...")
	runner.CheckMediaServers(ctx)

	fmt.Println("Done.")
	return exitOK
}

// doOneShot — обычный запуск без флагов: один прогон и выход.
func doOneShot(ctx context.Context, runner *processor.Runner) int {
	if err := runner.Run(ctx); err != nil {
		if errors.Is(err, lock.ErrBusy) {
			return exitBusy
		}
		return exitError
	}
	return exitOK
}

// doDaemon — режим -d: демон живёт постоянно, запускает прогоны по
// расписанию и слушает сигналы.
//
// Цикл расписания живёт ЗДЕСЬ, а не внутри пакета daemon, намеренно:
// так всё поведение демона — когда он просыпается, что делает по сигналам,
// когда перечитывает конфиг — читается в одном месте, а не размазано
// между main и пакетом.
func doDaemon(ctx context.Context, cfg *config.Config, configPath string, database *db.DB,
	runner *processor.Runner, bootLogger *logging.Logger) int {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// state хранит актуальный конфиг для цикла расписания. Собственный
	// мьютекс нужен потому, что reload приходит из горутины обработки
	// сигналов, а расписание читается из горутины цикла.
	var (
		stateMu    sync.Mutex
		currentCfg = cfg
	)
	getSchedule := func() string {
		stateMu.Lock()
		defer stateMu.Unlock()
		return scheduleOf(currentCfg)
	}

	// scheduleChanged будит цикл расписания после reload: иначе он спал бы
	// до срока, вычисленного по старому SCHEDULE.
	scheduleChanged := make(chan struct{}, 1)

	reload := func() error {
		if _, err := config.EnsureConfig(configPath); err != nil {
			return err
		}
		newCfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		newLogger := logging.New(nil, os.Stdout, newCfg.LogVerbose)

		stateMu.Lock()
		currentCfg = newCfg
		stateMu.Unlock()

		runner.Reload(newCfg, newLogger)

		select {
		case scheduleChanged <- struct{}{}:
		default:
		}
		return nil
	}

	d := daemon.New(runner.Run, reload, bootLogger)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scheduleLoop(ctx, d, bootLogger, getSchedule, scheduleChanged)
	}()

	bootLogger.Event("[DAEMON_START] nfo_updater %s (built %s), schedule: %s",
		version.Version, version.BuildDate, getSchedule())

	err := d.Serve(ctx)

	// Serve возвращается после SIGTERM/SIGINT. Отменяем контекст, чтобы
	// остановить цикл расписания и прогон, если он в этот момент идёт,
	// и дожидаемся их завершения — прогон при отмене всё равно упакует
	// бэкапы уже изменённых файлов, обрывать его на полуслове нельзя.
	cancel()
	wg.Wait()

	if err != nil {
		bootLogger.Event("[ERROR] %v", err)
		return exitError
	}
	bootLogger.Event("[DAEMON_STOP] stopped")
	return exitOK
}

// scheduleLoop спит до ближайшего срока по расписанию и запускает прогон.
// Расписание перечитывается на каждой итерации, поэтому изменение SCHEDULE
// по SIGHUP подхватывается со следующего круга (а пробуждение по каналу
// scheduleChanged делает это немедленно).
func scheduleLoop(ctx context.Context, d *daemon.Daemon, logger *logging.Logger,
	getSchedule func() string, scheduleChanged <-chan struct{}) {

	for {
		expr := getSchedule()
		sched, err := scheduler.ParseCron(expr)
		if err != nil {
			// Сюда попасть нельзя: конфиг валидируется до старта и при
			// reload. Но если вдруг — спим сутки, а не крутим цикл впустую.
			logger.Event("[ERROR] invalid schedule %q: %v", expr, err)
			if !sleepUntil(ctx, time.Now().Add(24*time.Hour), scheduleChanged) {
				return
			}
			continue
		}

		next := sched.Next(time.Now())
		if next.IsZero() {
			logger.Event("[ERROR] schedule %q never matches any time, no runs will be started", expr)
			if !sleepUntil(ctx, time.Now().Add(24*time.Hour), scheduleChanged) {
				return
			}
			continue
		}

		logger.Event("[SCHEDULE] next run at %s", next.Format(time.RFC3339))
		if !sleepUntil(ctx, next, scheduleChanged) {
			return
		}

		// TriggerRun вызывается синхронно, чтобы wg.Wait() при остановке
		// дождался завершения прогона. Параллельные запуски исключены
		// самим демоном, а между процессами — flock внутри Run.
		d.TriggerRun(ctx)
	}
}

// sleepUntil ждёт указанного момента. Возвращает false, если пора
// завершаться, и true, если можно продолжать (срок наступил или
// расписание изменилось).
func sleepUntil(ctx context.Context, until time.Time, scheduleChanged <-chan struct{}) bool {
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-scheduleChanged:
		return true
	case <-timer.C:
		return true
	}
}

// scheduleOf возвращает расписание из конфига или значение по умолчанию.
// Пустой SCHEDULE в режиме демона означает не "не запускать никогда",
// а "раз в неделю" — молчаливо неработающий демон был бы худшим из
// возможных поведений.
func scheduleOf(cfg *config.Config) string {
	if cfg.Schedule != "" {
		return cfg.Schedule
	}
	return config.DefaultDaemonSchedule
}

// configPathForHelp — путь конфига для текста справки. Справка печатается
// в том числе там, где домашний каталог неизвестен, и пустая строка после
// "Default:" выглядела бы как ошибка вёрстки.
func configPathForHelp() string {
	if defaultConfigPath != "" {
		return defaultConfigPath
	}
	return "(unknown: the home directory could not be determined)"
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `NFO Updater
Version %s • %s

Updates ratings in Kodi NFO files for use with media libraries.

Usage:

  nfo_updater [flags]

Running with no flags performs a single pass over the library and exits.

Flags:

  -d              Run as a daemon: stay resident and start a pass on the
                  schedule set by SCHEDULE in the config file.

  -v              Show version and the paths this instance would use
                  (config, database, logs, media libraries, backups) along
                  with any media servers that are switched on.

  --config PATH   Path to the configuration file.
                  Default: %s

  -V, --version   Print the version and the build date on a single line,
                  then exit. Reads nothing and creates nothing, so it also
                  answers before a configuration file exists.

  -h, --help      Show this help.

API keys:

  All three services must be configured. Every key is verified at the start
  of each pass, so a key that has expired or been revoked is reported in the
  log right away, along with the address it can be replaced from:

%s

Media servers:

  Optional. When a media server is configured, NFO Updater asks it to rescan
  its library once a pass has finished, and only if at least one file was
  actually changed. Without this the new ratings still show up eventually,
  on the server's own scanning schedule — configuring a server only makes
  them appear sooner.

  A server that is switched on must have both its address and its key filled
  in. One that is unreachable, or that rejects its key, does not fail the
  pass: the ratings are already written to disk by then, so a warning goes
  to the log and the pass ends normally.

  See the configuration file for details.

Exit codes:

  %d  success
  %d  error during the pass
  %d  configuration problem
  %d  another instance is already running
`, version.Version, version.BuildDate, configPathForHelp(),
		providers.FormatKeyHelp("  "),
		exitOK, exitError, exitConfig, exitBusy)
	fmt.Fprintln(w)
}
