// internal/backup/backup.go
package backup

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Категории — соответствуют MOVIES_PATH/TVSHOWS_PATH, определяют и подпапку
// в рабочей директории, и подпапку в архивах (BACKUP_DIR/{category}/...).
const (
	CategoryMovies  = "Movies"
	CategoryTVShows = "TVShows"
)

// WorkDir — рабочая папка текущего прогона: сюда складываются копии файлов
// ДО изменения, пока прогон идёт. В конце прогона содержимое каждой
// категории упаковывается в архив, и рабочая папка категории удаляется.
func WorkDir(backupDir string) string {
	return filepath.Join(backupDir, ".tmp")
}

// ArchiveDir — куда складываются готовые zip-архивы категории:
// BACKUP_DIR/Movies/ и BACKUP_DIR/TVShows/ по отдельности.
func ArchiveDir(backupDir, category string) string {
	return filepath.Join(backupDir, category)
}

// ResetWorkDir очищает рабочую папку в начале прогона.
func ResetWorkDir(backupDir string) error {
	dir := WorkDir(backupDir)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear backup work dir: %w", err)
	}
	return os.MkdirAll(dir, 0o755)
}

// Save сохраняет original (содержимое файла ДО изменений) в рабочую папку
// под backupDir/.tmp/{category}/{archivePath}.
//
// archivePath — путь файла внутри архива, его строит processor.ArchivePath:
// полный путь файла без ведущего слэша. Благодаря этому файлы с одинаковой
// структурой внутри разных корней одной категории не перетирают друг друга.
func Save(backupDir, category, archivePath string, original []byte) error {
	if err := checkArchivePath(archivePath); err != nil {
		return err
	}
	dest := filepath.Join(WorkDir(backupDir), category, archivePath)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	if err := os.WriteFile(dest, original, 0o644); err != nil {
		return fmt.Errorf("write backup file: %w", err)
	}
	return nil
}

// checkArchivePath — страховка от записи мимо рабочей папки. Корректно
// построенный archivePath такого содержать не может, но цена ошибки здесь
// слишком высока: путь идёт прямиком в filepath.Join с последующей записью.
func checkArchivePath(archivePath string) error {
	if archivePath == "" {
		return fmt.Errorf("backup: empty archive path")
	}
	if filepath.IsAbs(archivePath) {
		return fmt.Errorf("backup: archive path must be relative, got %q", archivePath)
	}
	for _, part := range strings.Split(filepath.ToSlash(archivePath), "/") {
		if part == ".." {
			return fmt.Errorf("backup: archive path escapes the work directory: %q", archivePath)
		}
	}
	return nil
}

// Finalize упаковывает backupDir/.tmp/{category} в backupDir/{category}/{timestamp}.zip,
// удаляет рабочую папку категории и ротирует старые архивы этой категории
// по limit (0 = безлимитно). Возвращает "", nil, если за прогон не было
// ни одного изменённого файла в этой категории.
func Finalize(backupDir, category string, limit int, at time.Time) (archivePath string, err error) {
	srcDir := filepath.Join(WorkDir(backupDir), category)
	entries, err := os.ReadDir(srcDir)
	if err != nil || len(entries) == 0 {
		return "", nil
	}

	archDir := ArchiveDir(backupDir, category)
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		return "", fmt.Errorf("create archive dir: %w", err)
	}
	name := at.Format("2006-01-02_15-04-05") + ".zip"
	archivePath = filepath.Join(archDir, name)

	if err := zipDir(srcDir, archivePath); err != nil {
		return "", fmt.Errorf("create archive: %w", err)
	}
	if err := os.RemoveAll(srcDir); err != nil {
		return archivePath, fmt.Errorf("clean up work dir: %w", err)
	}
	if err := rotate(archDir, limit); err != nil {
		return archivePath, fmt.Errorf("rotate archives: %w", err)
	}
	return archivePath, nil
}

func zipDir(srcDir, destZip string) error {
	f, err := os.Create(destZip)
	if err != nil {
		return err
	}
	defer f.Close()

	w := zip.NewWriter(f)
	defer w.Close()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		zf, err := w.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(zf, src)
		return err
	})
}

// rotate оставляет только последние limit архивов в archDir (имена архивов —
// timestamp в сортируемом формате, поэтому обычная сортировка строк =
// сортировка по времени создания).
func rotate(archDir string, limit int) error {
	if limit <= 0 {
		return nil
	}
	entries, err := os.ReadDir(archDir)
	if err != nil {
		return err
	}
	var matched []string
	for _, e := range entries {
		if !e.IsDir() {
			matched = append(matched, e.Name())
		}
	}
	sort.Strings(matched)
	if len(matched) <= limit {
		return nil
	}
	for _, name := range matched[:len(matched)-limit] {
		if err := os.Remove(filepath.Join(archDir, name)); err != nil {
			return err
		}
	}
	return nil
}
