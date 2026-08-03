package lock

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const Path = "/run/nfo_updater/nfo_updater.lock"

var ErrBusy = errors.New("another instance is already running")

type Lock struct {
	file *os.File
}

func Acquire() (*Lock, error) {
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
