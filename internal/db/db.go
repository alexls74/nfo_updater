// internal/db/db.go
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // чистый Go-драйвер SQLite, без cgo
)

// DB — обёртка над *sql.DB с методами, специфичными для схемы.
// БД тут не каталог медиатеки, а вспомогательный кэш для конвейера
// "прочитал .nfo -> сверился с онлайн-базами -> записал или пропустил".
type DB struct {
	sql *sql.DB
}

var migrations = []string{
	`
	CREATE TABLE ratings (
		media_key  TEXT NOT NULL,      -- imdb_id; иначе 'tmdb:<id>'; иначе 'tvdb:<id>' (см. MediaKey)
		imdb_id    TEXT,
		tmdb_id    TEXT,
		tvdb_id    TEXT,
		source     TEXT NOT NULL,
		value      TEXT,
		votes      INTEGER,
		-- found = 0 означает НЕГАТИВНУЮ пометку: провайдеры были опрошены
		-- и ответили, но значения для этого источника у них нет. Нужна,
		-- чтобы кэш-гейт считал тайтл полностью покрытым и не ходил в сеть
		-- за заведомо отсутствующим рейтингом на каждом прогоне.
		found      INTEGER NOT NULL DEFAULT 1,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (media_key, source)
	);

	CREATE TABLE pending (
		media_key    TEXT PRIMARY KEY,
		imdb_id      TEXT,
		tmdb_id      TEXT,
		tvdb_id      TEXT,
		reason       TEXT,
		first_seen   TEXT NOT NULL,
		last_attempt TEXT NOT NULL,
		attempts     INTEGER NOT NULL DEFAULT 1
	);

	CREATE TABLE api_usage (
		provider  TEXT NOT NULL,
		key_index INTEGER NOT NULL,
		date      TEXT NOT NULL,
		requests  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (provider, key_index, date)
	);

	CREATE TABLE schema_migrations (version INTEGER NOT NULL);
	INSERT INTO schema_migrations (version) VALUES (0);
	`,
}

// MediaKey строит единый ключ строки в БД из возможных ID тайтла.
// Приоритет: imdb_id -> 'tmdb:<id>' -> 'tvdb:<id>'.
//
// TVDb нужен потому, что в tvshow.nfo тег <id> может быть TVDB ID,
// и сериал может не иметь ни IMDb, ни TMDb идентификатора.
//
// Смена ключа при обогащении файла (был только tvdb, потом появился imdb)
// осиротит старую строку кэша. Это допустимо: ratings — вспомогательный
// кэш, а не каталог, потеря записи означает лишь один лишний запрос к API.
func MediaKey(imdbID, tmdbID, tvdbID string) (string, error) {
	if imdbID != "" {
		return imdbID, nil
	}
	if tmdbID != "" {
		return "tmdb:" + tmdbID, nil
	}
	if tvdbID != "" {
		return "tvdb:" + tvdbID, nil
	}
	return "", fmt.Errorf("imdb_id, tmdb_id and tvdb_id are all empty")
}

// Open открывает файл БД, при необходимости заводя каталог под него.
func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create database dir %s: %w", dir, err)
	}

	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}

	// sql.Open соединения не открывает — оно заводится первым
	// запросом. Поэтому все проблемы с самим файлом всплывают здесь,
	// на первой же PRAGMA, а не строкой выше. Отсюда и путь в сообщениях.
	if _, err := sqlDB.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("set journal_mode on %s: %w", path, err)
	}
	if _, err := sqlDB.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("set busy_timeout on %s: %w", path, err)
	}

	d := &DB{sql: sqlDB}
	if err := d.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate database %s: %w", path, err)
	}
	return d, nil
}

func (d *DB) Close() error {
	return d.sql.Close()
}

// migrate применяет недостающие миграции. Целевая версия схемы — это просто
// len(migrations): отдельная константа с номером версии тут не нужна, она
// была бы вторым источником правды и рано или поздно разошлась бы со списком.
func (d *DB) migrate() error {
	var version int
	err := d.sql.QueryRow(`SELECT version FROM schema_migrations LIMIT 1`).Scan(&version)
	if err != nil {
		version = 0
	}

	for i := version; i < len(migrations); i++ {
		tx, err := d.sql.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`UPDATE schema_migrations SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("update schema_migrations after %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Rating — строка кэша. Found == false означает негативную пометку:
// источник опрашивали, значения нет. У такой строки Value пустой.
type Rating struct {
	Source    string
	Value     string
	Votes     int
	Found     bool
	UpdatedAt time.Time
}

// UpsertRating сохраняет полученное от провайдера значение.
func (d *DB) UpsertRating(imdbID, tmdbID, tvdbID, source, value string, votes int) error {
	if err := d.upsert(imdbID, tmdbID, tvdbID, source, value, votes, true); err != nil {
		return fmt.Errorf("upsert rating: %w", err)
	}
	return nil
}

// UpsertMissingRating ставит негативную пометку: источник опрошен, значения
// нет. Вызывать можно ТОЛЬКО когда провайдеры действительно ответили —
// иначе недоступность сети или исчерпанная квота были бы записаны как
// "рейтинга не существует" и отравили бы кэш на сутки вперёд.
func (d *DB) UpsertMissingRating(imdbID, tmdbID, tvdbID, source string) error {
	if err := d.upsert(imdbID, tmdbID, tvdbID, source, "", 0, false); err != nil {
		return fmt.Errorf("upsert missing rating: %w", err)
	}
	return nil
}

func (d *DB) upsert(imdbID, tmdbID, tvdbID, source, value string, votes int, found bool) error {
	key, err := MediaKey(imdbID, tmdbID, tvdbID)
	if err != nil {
		return err
	}
	foundInt := 0
	if found {
		foundInt = 1
	}
	_, err = d.sql.Exec(`
		INSERT INTO ratings (media_key, imdb_id, tmdb_id, tvdb_id, source, value, votes, found, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (media_key, source) DO UPDATE SET
			imdb_id    = excluded.imdb_id,
			tmdb_id    = excluded.tmdb_id,
			tvdb_id    = excluded.tvdb_id,
			value      = excluded.value,
			votes      = excluded.votes,
			found      = excluded.found,
			updated_at = excluded.updated_at
	`, key, nullIfEmpty(imdbID), nullIfEmpty(tmdbID), nullIfEmpty(tvdbID),
		source, value, votes, foundInt, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (d *DB) GetRatings(imdbID, tmdbID, tvdbID string) (map[string]Rating, error) {
	key, err := MediaKey(imdbID, tmdbID, tvdbID)
	if err != nil {
		return nil, fmt.Errorf("get ratings: %w", err)
	}
	rows, err := d.sql.Query(`
		SELECT source, COALESCE(value, ''), COALESCE(votes, 0), found, updated_at
		FROM ratings WHERE media_key = ?
	`, key)
	if err != nil {
		return nil, fmt.Errorf("query ratings: %w", err)
	}
	defer rows.Close()

	out := make(map[string]Rating)
	for rows.Next() {
		var r Rating
		var foundInt int
		var updatedAt string
		if err := rows.Scan(&r.Source, &r.Value, &r.Votes, &foundInt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan rating: %w", err)
		}
		r.Found = foundInt != 0
		r.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		out[r.Source] = r
	}
	return out, rows.Err()
}

func (d *DB) MarkPending(imdbID, tmdbID, tvdbID, reason string) error {
	key, err := MediaKey(imdbID, tmdbID, tvdbID)
	if err != nil {
		return fmt.Errorf("mark pending: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = d.sql.Exec(`
		INSERT INTO pending (media_key, imdb_id, tmdb_id, tvdb_id, reason, first_seen, last_attempt, attempts)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT (media_key) DO UPDATE SET
			imdb_id      = excluded.imdb_id,
			tmdb_id      = excluded.tmdb_id,
			tvdb_id      = excluded.tvdb_id,
			reason       = excluded.reason,
			last_attempt = excluded.last_attempt,
			attempts     = pending.attempts + 1
	`, key, nullIfEmpty(imdbID), nullIfEmpty(tmdbID), nullIfEmpty(tvdbID), reason, now, now)
	if err != nil {
		return fmt.Errorf("mark pending: %w", err)
	}
	return nil
}

func (d *DB) ClearPending(imdbID, tmdbID, tvdbID string) error {
	key, err := MediaKey(imdbID, tmdbID, tvdbID)
	if err != nil {
		return fmt.Errorf("clear pending: %w", err)
	}
	_, err = d.sql.Exec(`DELETE FROM pending WHERE media_key = ?`, key)
	if err != nil {
		return fmt.Errorf("clear pending: %w", err)
	}
	return nil
}

type PendingEntry struct {
	MediaKey    string
	ImdbID      string
	TmdbID      string
	TvdbID      string
	Reason      string
	FirstSeen   time.Time
	LastAttempt time.Time
	Attempts    int
}

func (d *DB) ListPending() ([]PendingEntry, error) {
	rows, err := d.sql.Query(`
		SELECT media_key, COALESCE(imdb_id, ''), COALESCE(tmdb_id, ''), COALESCE(tvdb_id, ''),
		       reason, first_seen, last_attempt, attempts
		FROM pending
	`)
	if err != nil {
		return nil, fmt.Errorf("query pending: %w", err)
	}
	defer rows.Close()

	var out []PendingEntry
	for rows.Next() {
		var e PendingEntry
		var firstSeen, lastAttempt string
		if err := rows.Scan(&e.MediaKey, &e.ImdbID, &e.TmdbID, &e.TvdbID,
			&e.Reason, &firstSeen, &lastAttempt, &e.Attempts); err != nil {
			return nil, fmt.Errorf("scan pending: %w", err)
		}
		e.FirstSeen, _ = time.Parse(time.RFC3339, firstSeen)
		e.LastAttempt, _ = time.Parse(time.RFC3339, lastAttempt)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) IncrementUsage(provider string, keyIndex int) (int, error) {
	today := time.Now().UTC().Format("2006-01-02")
	_, err := d.sql.Exec(`
		INSERT INTO api_usage (provider, key_index, date, requests)
		VALUES (?, ?, ?, 1)
		ON CONFLICT (provider, key_index, date) DO UPDATE SET
			requests = api_usage.requests + 1
	`, provider, keyIndex, today)
	if err != nil {
		return 0, fmt.Errorf("increment usage: %w", err)
	}

	var requests int
	err = d.sql.QueryRow(`
		SELECT requests FROM api_usage WHERE provider = ? AND key_index = ? AND date = ?
	`, provider, keyIndex, today).Scan(&requests)
	if err != nil {
		return 0, fmt.Errorf("read usage after increment: %w", err)
	}
	return requests, nil
}

// SetUsageToday выставляет счётчик расхода принудительно.
//
// Нужен для одного случая: сервис ответил "суточный лимит исчерпан", хотя
// наш локальный счётчик до лимита не дошёл — значит ключом пользовались
// и вне нашей программы. Без этой отметки каждый следующий прогон в те же
// сутки заново тратил бы запрос, чтобы услышать тот же отказ.
//
// Значение только повышается: MAX со старым не даёт случайно обнулить
// уже учтённый расход.
func (d *DB) SetUsageToday(provider string, keyIndex int, requests int) error {
	today := time.Now().UTC().Format("2006-01-02")
	_, err := d.sql.Exec(`
		INSERT INTO api_usage (provider, key_index, date, requests)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (provider, key_index, date) DO UPDATE SET
			requests = MAX(api_usage.requests, excluded.requests)
	`, provider, keyIndex, today, requests)
	if err != nil {
		return fmt.Errorf("set usage: %w", err)
	}
	return nil
}

func (d *DB) GetUsageToday(provider string, keyIndex int) (int, error) {
	today := time.Now().UTC().Format("2006-01-02")
	var requests int
	err := d.sql.QueryRow(`
		SELECT requests FROM api_usage WHERE provider = ? AND key_index = ? AND date = ?
	`, provider, keyIndex, today).Scan(&requests)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read usage: %w", err)
	}
	return requests, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
