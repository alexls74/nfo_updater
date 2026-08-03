// internal/processor/stats.go
package processor

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// RunStats — сводка одного прогона, аналог итогового отчёта из исходного
// питон-скрипта, расширенная под наши новые категории.
type RunStats struct {
	mu sync.Mutex

	StartedAt  time.Time
	FinishedAt time.Time

	MoviesProcessed   int
	TVShowsProcessed  int
	EpisodesProcessed int

	Updated   int
	Unchanged int
	Pending   int
	NoID      int
	Skipped   int
	Errors    int

	// NoAccess — файлы, пропущенные из-за отсутствия прав на запись.
	// Считаются отдельно от Skipped: Skipped означает "нам нечего было
	// здесь делать по настройкам", а NoAccess — "дело было, но мы не
	// смогли", то есть это повод посмотреть на права в медиатеке.
	NoAccess int

	// FromCache — файлы, для которых значения взяты из кэша без единого
	// сетевого запроса (свежий кэш или недоступные провайдеры). Полезно
	// как индикатор: если это число близко к общему, значит либо прогоны
	// идут слишком часто, либо провайдеры недоступны.
	FromCache int

	BackupsCreated    int
	DefaultOverridden int

	providerRequests map[string]int
}

func NewRunStats() *RunStats {
	return &RunStats{
		StartedAt:        time.Now(),
		providerRequests: make(map[string]int),
	}
}

func (s *RunStats) IncMovie()             { s.mu.Lock(); s.MoviesProcessed++; s.mu.Unlock() }
func (s *RunStats) IncTVShow()            { s.mu.Lock(); s.TVShowsProcessed++; s.mu.Unlock() }
func (s *RunStats) IncEpisode()           { s.mu.Lock(); s.EpisodesProcessed++; s.mu.Unlock() }
func (s *RunStats) IncUpdated()           { s.mu.Lock(); s.Updated++; s.mu.Unlock() }
func (s *RunStats) IncUnchanged()         { s.mu.Lock(); s.Unchanged++; s.mu.Unlock() }
func (s *RunStats) IncPending()           { s.mu.Lock(); s.Pending++; s.mu.Unlock() }
func (s *RunStats) IncNoID()              { s.mu.Lock(); s.NoID++; s.mu.Unlock() }
func (s *RunStats) IncSkipped()           { s.mu.Lock(); s.Skipped++; s.mu.Unlock() }
func (s *RunStats) IncNoAccess()          { s.mu.Lock(); s.NoAccess++; s.mu.Unlock() }
func (s *RunStats) IncFromCache()         { s.mu.Lock(); s.FromCache++; s.mu.Unlock() }
func (s *RunStats) IncError()             { s.mu.Lock(); s.Errors++; s.mu.Unlock() }
func (s *RunStats) IncBackup()            { s.mu.Lock(); s.BackupsCreated++; s.mu.Unlock() }
func (s *RunStats) IncDefaultOverridden() { s.mu.Lock(); s.DefaultOverridden++; s.mu.Unlock() }

// UpdatedCount — сколько файлов реально изменено за прогон. По нему
// решается, дёргать ли медиасерверы: если на диске ничего не поменялось,
// просить сервер пересканировать библиотеку не за чем.
func (s *RunStats) UpdatedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Updated
}

func (s *RunStats) IncProviderRequest(provider string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providerRequests[provider]++
}

func (s *RunStats) Finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.FinishedAt = time.Now()
}

func (s *RunStats) Summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var b strings.Builder
	duration := s.FinishedAt.Sub(s.StartedAt).Round(time.Second)

	fmt.Fprintf(&b, "===== RUN SUMMARY =====\n")
	fmt.Fprintf(&b, "Duration: %s\n", duration)
	fmt.Fprintf(&b, "Movies processed: %d | TV shows: %d | Episodes: %d\n",
		s.MoviesProcessed, s.TVShowsProcessed, s.EpisodesProcessed)
	fmt.Fprintf(&b, "Updated: %d | Unchanged: %d | Pending: %d | No ID: %d\n",
		s.Updated, s.Unchanged, s.Pending, s.NoID)
	fmt.Fprintf(&b, "Skipped: %d | No write access: %d | Served from cache: %d | Errors: %d\n",
		s.Skipped, s.NoAccess, s.FromCache, s.Errors)
	fmt.Fprintf(&b, "Backups created: %d | Default rating overridden: %d\n",
		s.BackupsCreated, s.DefaultOverridden)

	if len(s.providerRequests) > 0 {
		// Сортировка по имени: map в Go отдаёт ключи в случайном порядке,
		// и строки отчёта прыгали бы между прогонами, мешая сравнивать логи.
		names := make([]string, 0, len(s.providerRequests))
		for provider := range s.providerRequests {
			names = append(names, provider)
		}
		sort.Strings(names)

		fmt.Fprintf(&b, "Provider requests this run:\n")
		for _, provider := range names {
			fmt.Fprintf(&b, "  %s: %d\n", provider, s.providerRequests[provider])
		}
	}
	fmt.Fprintf(&b, "=======================")
	return b.String()
}
