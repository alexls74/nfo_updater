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
// путь к конфигу и имя пользователя, под которым программа установлена.
// Всё это выясняется только в момент установки.
package unit

import (
	"fmt"
	"os/user"
	"path/filepath"
	"strings"

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
// расхождении.
//
// Конфиг аргументом не передаётся: из него в юнит не попадает ничего,
// кроме пути к самому файлу. Так текст юнита зависит только от того,
// куда установлена программа, и правка настроек его не устаревает.
func Generate(configPath, binaryPath string) (string, error) {
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
	// не хуже.
	group = u.Gid
	if g, gerr := user.LookupGroupId(u.Gid); gerr == nil && g.Name != "" {
		group = g.Name
	}
	return u.Username, group, nil
}

// unitValue готовит путь к подстановке в юнит.
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
