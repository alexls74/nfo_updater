// internal/unit/unit.go
//
// Генератор текста systemd-юнита для флага --print-unit.
//
// Отдельный пакет, а не файл в internal/setup: мастер настройки — диалог,
// намертво привязанный к /dev/tty, а генератор — чистая функция из конфига
// в текст. Их единственный общий вызывающий — main.go. Команда
// `nfo_updater.sh service on` печатает юнит для уже установленной программы,
// не запуская мастер вовсе, и тащить за собой машинерию диалога ей незачем.
//
// Юнит нельзя положить в репозиторий готовым файлом: в нём путь к бинарнику,
// путь к конфигу, имя пользователя и список каталогов медиатеки. Всё, кроме
// первого, лежит в конфиге — и разбирать конфиг вторично, на POSIX sh,
// значит получить вторую, худшую реализацию правил, которые здесь уже
// написаны и проверены.
package unit

import (
	"fmt"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"nfo_updater/internal/config"
	"nfo_updater/internal/exitcode"
)

// FileName — имя файла юнита. Живёт здесь, чтобы генератор, установочный
// скрипт и README не разошлись в написании.
const FileName = "nfo_updater.service"

// stopTimeout — сколько systemd ждёт завершения после SIGTERM, в секундах.
//
// Умолчания (90 секунд) не хватает. Демон по SIGTERM отменяет контекст,
// а прогон при отмене не бросает работу на полуслове: он доупаковывает
// бэкапы уже изменённых файлов. Убить его в этот момент — оставить правки
// в .nfo без возможности откатиться, то есть ровно то, ради чего бэкапы
// и заведены.
//
// Пяти минут хватает с запасом: отменённый прогон новых файлов не
// начинает, ему остаётся закрыть архив. Бесконечность не годится —
// зависший процесс подвесил бы выключение машины.
const stopTimeout = 300

// Generate возвращает текст юнита, запускающего демона.
//
// binaryPath передаётся аргументом, а не берётся из os.Executable():
// при установке юнит генерируется бинарником, который ещё лежит во
// временном каталоге, и os.Executable() указал бы на путь, исчезающий
// через минуту.
//
// Вывод детерминирован — тот же конфиг и тот же путь дают тот же текст
// байт в байт. На этом стоит update: сгенерировать заново, сравнить
// с установленным, и трогать systemctl daemon-reload только при
// расхождении. Отсюда же сортировка путей: обход map дал бы случайный
// порядок, а эта ошибка в проекте уже случалась — она переписывала .nfo
// на каждом прогоне без единого изменения в содержимом.
func Generate(cfg *config.Config, configPath, binaryPath string) (string, error) {
	if !filepath.IsAbs(binaryPath) {
		return "", fmt.Errorf("the path to the binary must be absolute, got %q", binaryPath)
	}
	if !filepath.IsAbs(configPath) {
		return "", fmt.Errorf("the path to the configuration file must be absolute, got %q", configPath)
	}

	owner, group, err := currentOwner()
	if err != nil {
		return "", err
	}

	bin, err := unitValue(filepath.Clean(binaryPath))
	if err != nil {
		return "", err
	}
	conf, err := unitValue(filepath.Clean(configPath))
	if err != nil {
		return "", err
	}

	var b strings.Builder

	b.WriteString("[Unit]\n")
	// Версия в Description намеренно не подставляется: юнит менялся бы
	// после каждого обновления, и update был бы обязан переписывать файл
	// и звать daemon-reload там, где по существу ничего не изменилось.
	b.WriteString("Description=NFO Updater - rating updates for Kodi NFO files\n")
	b.WriteString("Documentation=https://github.com/alexls74/nfo_updater\n")
	// network-online, а не network: программе нужен не поднятый интерфейс,
	// а достижимые OMDb, MDBList и TMDb. Проверка ключей идёт в начале
	// прогона, и без сети прогон закончится ничем.
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	for _, p := range mountPaths(cfg, configPath, binaryPath) {
		v, err := unitValue(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "RequiresMountsFor=%s\n", v)
	}

	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "User=%s\n", owner)
	fmt.Fprintf(&b, "Group=%s\n", group)
	// --config подставляется всегда, даже когда путь совпадает с
	// умолчанием: под User= домашний каталог программе даёт systemd из
	// /etc/passwd, и совпадение он обычно даёт, но зависимость получается
	// невидимой. Юнит должен читаться как самодостаточный документ.
	fmt.Fprintf(&b, "ExecStart=%s -d --config %s\n", bin, conf)
	// Перечитывание конфига по `systemctl reload nfo_updater`. Без этой
	// строки systemd отвечает "Job type reload is not applicable", и
	// единственным способом подхватить правки остаётся перезапуск службы —
	// а он обрывает прогон, если тот идёт прямо сейчас. Демон по SIGHUP
	// откладывает перезагрузку до конца прогона и при ошибке в новом
	// конфиге продолжает работать со старым.
	b.WriteString("ExecReload=/bin/kill -HUP $MAINPID\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=30\n")
	// Сломанный конфиг перезапуском не чинится: программа будет падать
	// каждые тридцать секунд и засорять journal, пока человек не вмешается.
	fmt.Fprintf(&b, "RestartPreventExitStatus=%d\n", exitcode.Config)
	fmt.Fprintf(&b, "TimeoutStopSec=%d\n", stopTimeout)

	// Песочница — умеренная. ProtectSystem=full делает /usr, /boot и /etc
	// доступными только на чтение; медиатека и домашний каталог остаются
	// записываемыми, и списка ReadWritePaths не требуется.
	//
	// ProtectSystem=strict сознательно НЕ берётся. Выигрыша он не даёт:
	// служба работает под тем же пользователем, который владеет и
	// бинарником, и медиатекой, так что запрещать ей запись некуда.
	// А цена высока: ReadWritePaths реализованы через bind-mount, снятый
	// на момент старта службы, поэтому сетевое хранилище, примонтированное
	// или переподключившееся позже, внутри службы не появится вовсе.
	// Отладить это снаружи почти невозможно — каталог виден в оболочке
	// и пуст для демона.
	//
	// ProtectHome=no указан явно, хотя это и умолчание: он здесь
	// контринтуитивен, а конфиг, база, логи и бэкапы лежат именно в $HOME.
	b.WriteString("NoNewPrivileges=yes\n")
	b.WriteString("PrivateTmp=yes\n")
	b.WriteString("ProtectSystem=full\n")
	b.WriteString("ProtectHome=no\n")
	b.WriteString("ProtectControlGroups=yes\n")
	b.WriteString("ProtectKernelTunables=yes\n")
	b.WriteString("ProtectKernelModules=yes\n")
	b.WriteString("RestrictSUIDSGID=yes\n")
	b.WriteString("RestrictRealtime=yes\n")
	b.WriteString("LockPersonality=yes\n")

	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")

	return b.String(), nil
}

// currentOwner — от чьего имени будет работать служба.
//
// Берётся текущий пользователь: юнит генерируется тем, кто ставит
// программу, а конфиг, база и бэкапы лежат в его домашнем каталоге.
// Под sudo здесь оказался бы root — за этим следит вызывающий, main.go
// отказывается выполнять --print-unit из-под sudo.
func currentOwner() (owner, group string, err error) {
	u, err := user.Current()
	if err != nil {
		return "", "", fmt.Errorf("determine the current user: %w", err)
	}
	if u.Username == "" {
		return "", "", fmt.Errorf("the current user has no name, cannot fill in User= in the unit file")
	}

	// Первичная группа по имени читается приятнее, но её имя может и не
	// найтись (LDAP, урезанный NSS на NAS). Числовой GID systemd понимает
	// не хуже, поэтому это не повод отказываться от генерации.
	group = u.Gid
	if g, gerr := user.LookupGroupId(u.Gid); gerr == nil && g.Name != "" {
		group = g.Name
	}
	return u.Username, group, nil
}

// mountPaths — каталоги, без которых службе нечего делать.
//
// Главный случай — медиатека на сетевом хранилище. Служба, стартовавшая
// раньше монтирования, увидит пустой каталог и честно отчитается о нуле
// обработанных файлов; понять по такому отчёту, что дело в монтировании,
// нельзя ниоткуда.
//
// RequiresMountsFor на пути, который не покрыт ни одним mount-юнитом,
// безвреден: он раскрывается в корневой -.mount, существующий всегда.
func mountPaths(cfg *config.Config, configPath, binaryPath string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, 8)

	add := func(p string) {
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		p = filepath.Clean(p)
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}

	// Каталог бинарника: при установке с --user он лежит в домашнем
	// каталоге, а тот бывает на отдельном или сетевом разделе.
	add(filepath.Dir(binaryPath))
	add(filepath.Dir(configPath))
	// У базы в конфиге путь к ФАЙЛУ, и файла может ещё не быть —
	// спрашиваем про каталог.
	add(filepath.Dir(cfg.DatabasePath))
	// Логи и бэкапы — только когда включены: выключенных программа
	// не касается, и ждать их монтирования незачем.
	if cfg.LogEnabled {
		add(cfg.LogDir)
	}
	if cfg.BackupEnabled {
		add(cfg.BackupDir)
	}
	for _, p := range cfg.MoviesPaths {
		add(p)
	}
	for _, p := range cfg.TVShowsPaths {
		add(p)
	}

	sort.Strings(out)
	return out
}

// unitValue готовит путь к подстановке в юнит.
//
// Две опасности. Знак процента: systemd раскрывает %-спецификаторы прямо
// в значениях, и каталог с процентом в имени превратился бы в чужой путь
// или в ошибку разбора; лечится удвоением. Пробел: значения разбираются
// как список слов, поэтому путь с пробелом берётся в кавычки — в названиях
// фильмов пробелы обычное дело.
//
// Кавычка, обратная косая черта и перевод строки внутри пути — отказ,
// а не повод изобретать экранирование. В медиатеке такие имена
// не встречаются, а тихо испорченный юнит обнаружился бы только при
// systemctl start, где сообщение было бы совсем о другом.
func unitValue(s string) (string, error) {
	if strings.ContainsAny(s, "\"'\\\n\r") {
		return "", fmt.Errorf(
			"path %q contains a quote, a backslash or a line break, which cannot be placed in a systemd unit file", s)
	}
	s = strings.ReplaceAll(s, "%", "%%")
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`, nil
	}
	return s, nil
}
