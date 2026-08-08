// internal/lock/lock.go
package lock

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"nfo_updater/internal/config"
)

// lockFileName — имя файла блокировки внутри каталога данных.
const lockFileName = "nfo_updater.lock"

// Path — файл блокировки: ~/.local/share/nfo_updater/nfo_updater.lock
//
// Раньше здесь была константа /run/nfo_updater/nfo_updater.lock, и каталог
// заводила директива RuntimeDirectory= в systemd-юните. С переходом на запуск
// от обычной учётной записи это перестало работать: создать каталог в /run
// может только root, а ручной запуск (без сервиса) root'а не имеет вовсе.
//
// $XDG_RUNTIME_DIR (/run/user/1000) на роль замены не годится, хотя выглядит
// подходяще: systemd выставляет эту переменную пользовательским юнитам, а наш
// юнит системный, с User=. Ручной запуск из шелла получил бы /run/user/1000,
// сервис — пустое значение и запасной путь, и два процесса перестали бы
// видеть блокировку друг друга. Это сломало бы ровно то, ради чего она есть.
//
// Каталог данных таким свойством не обладает: он выводится из $HOME, а его
// systemd берёт из passwd, так что путь совпадает в обоих режимах.
//
// Оговорка про сетевые ФС: flock поверх NFS и SMB ведёт себя непредсказуемо
// (на NFSv3 без lockd — молча не работает вовсе). Домашний каталог на NAS —
// случай не экзотический, и если два прогона однажды пойдут внахлёст, искать
// причину следует здесь.
var Path = defaultLockPath()

func defaultLockPath() string {
	dir := config.DefaultDataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, lockFileName)
}

var ErrBusy = errors.New("another instance is already running")

type Lock struct {
	file *os.File
}

func Acquire() (*Lock, error) {
	// Пустой путь означает неизвестный домашний каталог. До сюда дело дойти
	// не должно — main.go отказывается стартовать раньше, — но собирать из
	// пустой строки относительный путь и класть файл блокировки в рабочий
	// каталог было бы худшим из вариантов: у сервиса и у шелла он разный.
	if Path == "" {
		return nil, errors.New("cannot determine the lock file path: the home directory of the current user is unknown")
	}

	if err := os.MkdirAll(filepath.Dir(Path), 0o755); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}

	f, err := os.OpenFile(Path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		info := readLockInfo(f)
		f.Close()
		if info != "" {
			return nil, fmt.Errorf("%w (%s)", ErrBusy, info)
		}
		return nil, ErrBusy
	}

	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	fmt.Fprintf(f, "pid=%d started=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	_ = f.Sync()

	return &Lock{file: f}, nil
}

func (l *Lock) Release() error {
	defer l.file.Close()
	return syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
}

func readLockInfo(f *os.File) string {
	_, _ = f.Seek(0, 0)
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}
