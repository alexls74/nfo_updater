// internal/processor/paths.go
package processor

import (
	"path/filepath"
	"strings"

	"nfo_updater/internal/backup"
	"nfo_updater/internal/config"
)

// isUnder сообщает, лежит ли path внутри root (или совпадает с ним).
// Пути из конфига уже нормализованы (см. config.normalizePaths: абсолютные,
// Clean, без дублей), а path приходит из обхода по тому же корню — поэтому
// дополнительная нормализация здесь не нужна.
func isUnder(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// backupCategory определяет категорию файла (Movies/TVShows) по тому, под
// каким корнем из конфига он найден, и возвращает путь этого файла ВНУТРИ
// архива бэкапа.
//
// Порядок проверки (сначала Movies, потом TVShows) больше не может дать
// неверную категорию: пересечение корней запрещено на уровне config.Validate().
func backupCategory(cfg *config.Config, path string) (category, archivePath string, ok bool) {
	for _, root := range cfg.MoviesPaths {
		if isUnder(path, root) {
			return backup.CategoryMovies, ArchivePath(path), true
		}
	}
	for _, root := range cfg.TVShowsPaths {
		if isUnder(path, root) {
			return backup.CategoryTVShows, ArchivePath(path), true
		}
	}
	return "", "", false
}

// ArchivePath превращает абсолютный путь файла в путь внутри архива:
// /media1/Movies/Alien (1979)/movie.nfo -> media1/Movies/Alien (1979)/movie.nfo
func ArchivePath(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "/")
}
