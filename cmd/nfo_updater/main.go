// cmd/nfo_updater/main.go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"nfo_updater/internal/config"
	"nfo_updater/internal/daemon"
	"nfo_updater/internal/db"
	"nfo_updater/internal/exitcode"
	"nfo_updater/internal/lock"
	"nfo_updater/internal/logging"
	"nfo_updater/internal/processor"
	"nfo_updater/internal/providers"
	"nfo_updater/internal/scheduler"
	"nfo_updater/internal/setup"
	"nfo_updater/internal/unit"
	"nfo_updater/internal/version"
)

// defaultConfigPath — путь по умолчанию, ~/.config/nfo_updater/config.conf.
//
// Вычисляется один раз при старте, а не константой: путь зависит от домашнего
// каталога текущего пользователя. Пустая строка означает, что домашний каталог
// определить не удалось; этот случай разбирается в run().
var defaultConfigPath = config.DefaultConfigPath()

func main() {
	os.Exit(run())
}

func run() int {
	var (
		daemonMode  bool
		showInfo    bool
		showVersion bool
		checkConfig bool
		setupMode   bool
		printUnit   string
		showHelp    bool
		configPath  string
	)

	fs := flag.NewFlagSet("nfo_updater", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&daemonMode, "d", false, "")
	fs.BoolVar(&showInfo, "v", false, "")
	// Регистр здесь значащий: пакет flag различает -v и -V. Первый — сводка
	// путей, второй — одна строка с версией.
	fs.BoolVar(&showVersion, "V", false, "")
	fs.BoolVar(&showVersion, "version", false, "")
	// --check-config намеренно не документирован в -h: он служебный.
	fs.BoolVar(&checkConfig, "check-config", false, "")
	fs.BoolVar(&setupMode, "setup", false, "")
	// --print-unit тоже не документирован в -h, и по той же причине.
	//
	// Путь к бинарнику приходит аргументом флага, а не позиционным: при
	// установке юнит генерирует бинарник, лежащий ещё во временном каталоге,
	// так что os.Executable() указал бы в никуда.
	fs.StringVar(&printUnit, "print-unit", "", "")
	fs.BoolVar(&showHelp, "h", false, "")
	fs.BoolVar(&showHelp, "help", false, "")
	fs.StringVar(&configPath, "config", defaultConfigPath, "")

	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		usage(os.Stderr)
		return exitcode.Error
	}
	if showHelp {
		usage(os.Stdout)
		return exitcode.OK
	}
	// --version отвечает раньше всех прочих проверок и ничего не читает
	// с диска. Так флаг работает и на системе, где конфига ещё нет, и на
	// системе, где он испорчен.
	if showVersion {
		fmt.Println(version.String())
		return exitcode.OK
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\n\n", fs.Arg(0))
		usage(os.Stderr)
		return exitcode.Error
	}

	// Режимы работы взаимоисключающи, и это проверяется явно, а не решается
	// молчаливым приоритетом. "--setup -d" — не "настроить, а потом стать
	// демоном".
	if modes := requestedModes(daemonMode, showInfo, checkConfig, setupMode, printUnit); len(modes) > 1 {
		fmt.Fprintf(os.Stderr, "these flags cannot be combined: %s\n\n", strings.Join(modes, ", "))
		usage(os.Stderr)
		return exitcode.Error
	}

	// Пустой путь означает, что и умолчание не вычислилось, и --config не задан.
	// Единственная причина — неизвестный домашний каталог; дальше идти незачем,
	// иначе ошибка вылезет как невнятное "open : no such file or directory".
	if configPath == "" {
		fmt.Fprintf(os.Stderr, "cannot determine the default configuration path: "+
			"the home directory of the current user is unknown.\n"+
			"Pass the path explicitly: nfo_updater --config /path/to/config.conf\n")
		return exitcode.Config
	}

	// Обе установочные команды отказываются работать из-под sudo. Под ним
	// $HOME становится /root: мастер записал бы ключи API в /root/.config,
	// где обычный пользователь их не найдёт и прав на них не имеет,
	// а --print-unit выдал бы юнит с User=root.
	//
	// Настоящий вход под root (SUDO_USER пуст) — не этот случай: на NAS
	// с единственной учётной записью это нормальный режим, и запрещать его
	// нельзя. Условие в точности повторяет проверку установочного скрипта;
	// здесь она нужна потому, что команду могут набрать и руками.
	if setupMode || printUnit != "" {
		if os.Geteuid() == 0 && os.Getenv("SUDO_USER") != "" {
			fmt.Fprintf(os.Stderr,
				"refusing to run under sudo.\n"+
					"The configuration belongs to your own user; under sudo it would be written to\n"+
					"root's home directory instead, where you would neither find it nor be able to\n"+
					"read it. Run this command as yourself — the installer asks for the password\n"+
					"only for the few steps that need it.\n")
			return exitcode.Error
		}
	}

	// Справочные и служебные режимы обрабатываются ДО EnsureConfig, и это
	// принципиально: ни один из них не должен ничего менять на диске.
	// Иначе запрос версии на свежей системе заводил бы конфиг и отвечал
	// сообщением о его создании вместо запрошенной информации, а мастер
	// настройки стартовал бы поверх только что созданного им же шаблона.
	if showInfo {
		return doShowInfo(configPath)
	}
	if printUnit != "" {
		return doPrintUnit(configPath, printUnit)
	}
	if setupMode {
		return doSetup(context.Background(), configPath)
	}

	// EnsureConfig создаёт конфиг из шаблона при первом запуске и переносит
	// значения при апгрейде. Возвращает ErrConfigCreated, если файла не было:
	// стартовать с пустым шаблоном бессмысленно, нужно дать пользователю
	// его заполнить.
	if _, err := config.EnsureConfig(configPath); err != nil {
		if errors.Is(err, config.ErrConfigCreated) {
			// Предлагаются обе дороги, и мастер первой. Справка о ключах
			// печатается сразу: человек, выбравший вторую, в этот момент
			// как раз собирается открыть только что созданный файл,
			// и отправлять его искать три сайта самостоятельно незачем.
			fmt.Printf("A default configuration file has been created at:\n  %s\n\n"+
				"Run the setup wizard to fill it in:\n\n"+
				"  nfo_updater --setup\n\n"+
				"Or edit the file by hand: the API keys and the media library paths are\n"+
				"the parts that have to be filled in before the first pass.\n\n"+
				"Where to get the keys:\n%s\n", configPath, providers.FormatKeyHelp("  "))
			return exitcode.Config
		}
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return exitcode.Config
	}

	cfg, cfgErr := config.Load(configPath)
	if cfgErr != nil {
		reportConfigError(configPath, cfgErr)
		return exitcode.Config
	}

	database, err := db.Open(cfg.DatabasePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		return exitcode.Error
	}
	defer database.Close()

	// Загрузочный журнал пишет только в консоль: файл лога заводится внутри
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

// requestedModes — список заданных взаимоисключающих флагов, в том виде,
// в каком они написаны в командной строке. Нужен только для сообщения
// об ошибке: перечислить, что именно не сочетается, полезнее, чем сказать
// "флаги несовместимы" и заставить перечитывать справку.
//
// -h и -V сюда не входят: они обработаны выше и до этой точки не доходят.
func requestedModes(daemonMode, showInfo, checkConfig, setupMode bool, printUnit string) []string {
	var modes []string
	if daemonMode {
		modes = append(modes, "-d")
	}
	if showInfo {
		modes = append(modes, "-v")
	}
	if checkConfig {
		modes = append(modes, "--check-config")
	}
	if setupMode {
		modes = append(modes, "--setup")
	}
	if printUnit != "" {
		modes = append(modes, "--print-unit")
	}
	return modes
}

// doSetup — режим --setup: интерактивный мастер настройки.
//
// Собственного вывода здесь нет: мастер и спрашивает, и печатает в /dev/tty,
// потому что установочный скрипт приходит по каналу и stdin бинарника — это
// тело скрипта. Наружу отсюда уходит только код возврата.
func doSetup(ctx context.Context, configPath string) int {
	res, err := setup.Run(ctx, configPath)
	if err != nil {
		if errors.Is(err, setup.ErrAborted) {
			// Осознанный отказ, а не поломка: на диске ничего не изменено.
			// Молчим намеренно — о прерывании человеку уже сказал сам
			// мастер, на том же терминале, где шёл разговор. Второе
			// сообщение здесь было бы повтором, а третье добавил бы
			// установочный скрипт.
			return exitcode.Aborted
		}
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		return exitcode.Error
	}

	recordKeyUsage(configPath, res.KeyUsage)

	// Единственное, что мастер сообщает наружу помимо факта успеха: нужна
	// ли служба. Через код возврата, а не через stdout, потому что stdout
	// у мастера уже занят под юнит в соседнем.
	if res.Scheduled {
		return exitcode.ServiceWanted
	}
	return exitcode.OK
}

// recordKeyUsage списывает в базу запросы, потраченные мастером на проверку
// ключей.
//
// Делается ЗДЕСЬ, а не внутри мастера, по одной причине: база лежит по пути,
// который мастер как раз и настраивает, и открыть её раньше записи конфига
// значило бы завести файл на диске в середине диалога. Мастер до последнего
// вопроса не меняет на диске ничего, и ломать это ради счётчика нельзя.
//
// Неудача не влияет на код возврата. К этому моменту конфиг записан и
// установка состоялась; развалить её из-за несписанного счётчика было бы
// несоразмерно, а незамеченным это не останется — прогон просто посчитает
// на несколько запросов больше доступных.
func recordKeyUsage(configPath string, usage map[string][]int) {
	if len(usage) == 0 {
		return
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: could not record the requests spent on key checks: %v\n", err)
		return
	}
	database, err := db.Open(cfg.DatabasePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: could not record the requests spent on key checks: %v\n", err)
		return
	}
	defer database.Close()

	for provider, counts := range usage {
		for index, requests := range counts {
			for i := 0; i < requests; i++ {
				if _, err := database.IncrementUsage(provider, index); err != nil {
					fmt.Fprintf(os.Stderr,
						"note: could not record the requests spent on key checks: %v\n", err)
					return
				}
			}
		}
	}
}

// doPrintUnit — режим --print-unit: текст systemd-юнита в stdout и больше
// ничего. Ни заголовка, ни пояснений: stdout здесь машинный канал,
// установочный скрипт перенаправляет его прямо в файл.
//
// Конфиг читается с диска через Load, а не через EnsureConfig: запросный
// режим не должен ни создавать шаблон, ни мигрировать файл, ни писать .bak.
// Читать конфиг самостоятельно, а не получать пути от мастера в памяти, —
// требование команды `service on`: она добавляет службу к уже установленной
// программе, и мастер при этом не запускается.
//
// Валидация выполняется полная, хотя в текст юнита из конфига не попадает
// ничего, кроме пути к файлу..
func doPrintUnit(configPath, binaryPath string) int {
	if _, err := config.Load(configPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "no configuration file at %s\n"+
				"Run nfo_updater --setup first: the service starts with this file and cannot work without it.\n",
				configPath)
			return exitcode.Config
		}
		reportConfigError(configPath, err)
		return exitcode.Config
	}

	text, err := unit.Generate(configPath, binaryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot generate the unit file: %v\n", err)
		return exitcode.Error
	}
	fmt.Print(text)
	return exitcode.OK
}

// doShowInfo — режим -v: версия и пути, с которыми программа работает.
// Ничего не проверяет и ничего не создаёт.
func doShowInfo(configPath string) int {
	cfg, err := config.Load(configPath)
	if cfg != nil {
		// Конфиг прочитан.
		fmt.Println(cfg.Describe(configPath))
		return exitcode.OK
	}

	if errors.Is(err, os.ErrNotExist) {
		// Файла ещё нет. Заводить его не будем, но показать, куда программа
		// будет складывать базу, логи и бэкапы, полезно — это те же пути,
		// что появятся в конфиге при первом настоящем запуске.
		fmt.Println(config.Defaults().Describe(configPath))
		fmt.Printf("\nno configuration file yet, the paths above are the defaults\n")
		return exitcode.OK
	}

	// Файл есть, но не читается: нет прав, битая ссылка и тому подобное.
	fmt.Printf("NFO Updater\nVersion %s • %s\n", version.Version, version.BuildDate)
	fmt.Fprintf(os.Stderr, "\ncannot read the configuration file: %v\n", err)
	return exitcode.Config
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
// Проверка ключей OMDb стоит по одному настоящему запросу на ключ
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
		return exitcode.Error
	}

	fmt.Println("\nChecking media servers...")
	runner.CheckMediaServers(ctx)

	fmt.Println("Done.")
	return exitcode.OK
}

// doOneShot — обычный запуск без флагов: один прогон и выход.
func doOneShot(ctx context.Context, runner *processor.Runner) int {
	if err := runner.Run(ctx); err != nil {
		if errors.Is(err, lock.ErrBusy) {
			return exitcode.Busy
		}
		return exitcode.Error
	}
	return exitcode.OK
}

// doDaemon — режим -d: демон живёт постоянно, запускает прогоны по
// расписанию и слушает сигналы.
//
// Цикл расписания живёт ЗДЕСЬ, а не внутри пакета daemon, намеренно:
// так всё поведение демона — когда он просыпается, что делает по сигналам,
// когда перечитывает конфиг — читается в одном месте.
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
		return exitcode.Error
	}
	bootLogger.Event("[DAEMON_STOP] stopped")
	return exitcode.OK
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
			if sleepUntil(ctx, time.Now().Add(24*time.Hour), scheduleChanged) == wakeStop {
				return
			}
			continue
		}

		next := sched.Next(time.Now())
		if next.IsZero() {
			logger.Event("[ERROR] schedule %q never matches any time, no runs will be started", expr)
			if sleepUntil(ctx, time.Now().Add(24*time.Hour), scheduleChanged) == wakeStop {
				return
			}
			continue
		}

		logger.Event("[SCHEDULE] next run at %s", next.Format(time.RFC3339))
		switch sleepUntil(ctx, next, scheduleChanged) {
		case wakeStop:
			return
		case wakeReschedule:
			// Перезагрузка конфига сама по себе прогон не начинает: цикл
			// возвращается к началу и пересчитывает срок по новому
			// расписанию. Иначе любой reload запускал бы сканирование
			// медиатеки — а reload делают ради правки конфига.
			continue
		case wakeDue:
			// TriggerRun вызывается синхронно, чтобы wg.Wait() при остановке
			// дождался завершения прогона. Параллельные запуски исключены
			// самим демоном, а между процессами — flock внутри Run.
			d.TriggerRun(ctx)
		}
	}
}

// wake — причина, по которой закончилось ожидание в sleepUntil.
type wake int

const (
	wakeStop       wake = iota // контекст отменён, пора завершаться
	wakeDue                    // наступил срок по расписанию — запускать прогон
	wakeReschedule             // конфиг перезагружен — пересчитать срок и ждать дальше
)

// sleepUntil ждёт указанного момента и сообщает, почему ожидание закончилось.
func sleepUntil(ctx context.Context, until time.Time, scheduleChanged <-chan struct{}) wake {
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return wakeStop
	case <-scheduleChanged:
		return wakeReschedule
	case <-timer.C:
		return wakeDue
	}
}

// scheduleOf возвращает расписание из конфига или значение по умолчанию.
// Пустой SCHEDULE в режиме демона означает не "не запускать никогда",
// а "раз в неделю".
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

  --setup         Set everything up in a question-and-answer session: where
                  the media library lives, which API keys to use, whether to
                  work on a schedule. Nothing is written until the last
                  question has been answered, so leaving halfway changes
                  nothing. Safe to run again later to change any of it.

  -d              Run as a DAEMON: stay resident and start a pass on the
                  schedule set by SCHEDULE in the config file.

  -v              Show version and the paths this instance would use
                  (config, database, logs, media libraries, backups) along
                  with any media servers that are switched on.

  --config PATH   Path to the configuration file.
                  Default: %s

  -h, --help      Show this help.

API keys:

  All services must be configured. Every key is verified at the start
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

  %2d  success, and for --setup also: no scheduled operation was wanted
  %2d  error during the pass
  %2d  configuration problem
  %2d  another instance is already running
  %2d  setup was cancelled, nothing has been changed
  %2d  setup finished and scheduled operation was wanted
`, version.Version, version.BuildDate, configPathForHelp(),
		providers.FormatKeyHelp("  "),
		exitcode.OK, exitcode.Error, exitcode.Config, exitcode.Busy,
		exitcode.Aborted, exitcode.ServiceWanted)
	fmt.Fprintln(w)
}
