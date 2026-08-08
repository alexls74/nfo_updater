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

func (s *RunStats) IncMovie()     { s.mu.Lock(); s.MoviesProcessed++; s.mu.Unlock() }
func (s *RunStats) IncTVShow()    { s.mu.Lock(); s.TVShowsProcessed++; s.mu.Unlock() }
func (s *RunStats) IncEpisode()   { s.mu.Lock(); s.EpisodesProcessed++; s.mu.Unlock() }
func (s *RunStats) IncUpdated()   { s.mu.Lock(); s.Updated++; s.mu.Unlock() }
func (s *RunStats) IncUnchanged() { s.mu.Lock(); s.Unchanged++; s.mu.Unlock() }
func (s *RunStats) IncPending()   { s.mu.Lock(); s.Pending++; s.mu.Unlock() }
func (s *RunStats) IncNoID()      { s.mu.Lock(); s.NoID++; s.mu.Unlock() }
func (s *RunStats) IncSkipped()   { s.mu.Lock(); s.Skipped++; s.mu.Unlock() }
func (s *RunStats) IncNoAccess()  { s.mu.Lock(); s.NoAccess++; s.mu.Unlock() }
func (s *RunStats) IncFromCache() { s.mu.Lock(); s.FromCache++; s.mu.Unlock() }
func (s *RunStats) IncError()     { s.mu.Lock(); s.Errors++; s.mu.Unlock() }
func (s *RunStats) IncBackup()    { s.mu.Lock(); s.BackupsCreated++; s.mu.Unlock() }

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

// Оформление сводки. Ширина рамки и колонки подписей вынесены в константы:
// самая длинная подпись ("default rating overridden") задаёт колонку,
// и при добавлении новой строки менять придётся только эти числа.
const (
	summaryWidth      = 65
	summaryLabelWidth = 25
)

func summaryRule(title string) string {
	if title == "" {
		return strings.Repeat("=", summaryWidth)
	}
	t := " " + title + " "
	left := (summaryWidth - len(t)) / 2
	return strings.Repeat("=", left) + t + strings.Repeat("=", summaryWidth-left-len(t))
}

func statLine(b *strings.Builder, label string, value int) {
	fmt.Fprintf(b, "  %-*s  %d\n", summaryLabelWidth, label, value)
}

// sortedProviders — имена провайдеров по алфавиту. Map в Go отдаёт ключи
// в случайном порядке, и строки отчёта прыгали бы между прогонами, мешая
// сравнивать логи. Вызывать при взятом мьютексе.
func (s *RunStats) sortedProviders() []string {
	names := make([]string, 0, len(s.providerRequests))
	for provider := range s.providerRequests {
		names = append(names, provider)
	}
	sort.Strings(names)
	return names
}

// Summary собирает подробный блок для файла лога.
//
// Группировка отвечает на три отдельных вопроса: что программа увидела,
// что она сделала и куда надо посмотреть человеку. Прежний вид — четыре
// плотные строки через вертикальные черты — формально содержал то же
// самое, но читать его приходилось по слогам, и главное (есть ли повод
// беспокоиться) терялось среди второстепенного.
//
// Блок Needs attention намеренно печатается целиком, вместе с нулями:
// строка "errors 0" — это подтверждение, что ошибок не было, тогда как
// отсутствие строки означало бы всего лишь, что её не напечатали.
func (s *RunStats) Summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var b strings.Builder
	duration := s.FinishedAt.Sub(s.StartedAt).Round(time.Second)

	b.WriteString(summaryRule("SUMMARY"))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Duration: %s\n\n", duration)

	b.WriteString("Scanned\n")
	statLine(&b, "movies", s.MoviesProcessed)
	statLine(&b, "tv shows", s.TVShowsProcessed)
	statLine(&b, "episodes", s.EpisodesProcessed)
	b.WriteString("\n")

	b.WriteString("Changed\n")
	statLine(&b, "updated", s.Updated)
	statLine(&b, "unchanged", s.Unchanged)
	statLine(&b, "backups created", s.BackupsCreated)
	statLine(&b, "default rating overridden", s.DefaultOverridden)
	b.WriteString("\n")

	b.WriteString("Needs attention\n")
	statLine(&b, "pending", s.Pending)
	statLine(&b, "no id", s.NoID)
	statLine(&b, "no write access", s.NoAccess)
	statLine(&b, "skipped", s.Skipped)
	statLine(&b, "errors", s.Errors)
	b.WriteString("\n")

	b.WriteString("Providers\n")
	for _, provider := range s.sortedProviders() {
		statLine(&b, provider+" requests", s.providerRequests[provider])
	}
	statLine(&b, "served from cache", s.FromCache)
	b.WriteString("\n")

	b.WriteString(summaryRule(""))
	return b.String()
}

// SummaryLine — та же сводка одной строкой, для системного журнала.
//
// Формат key=value выбран не ради краткости, а ради пригодности к разбору:
// journalctl, grep и любой сборщик логов работают со строкой, а не с
// многострочным блоком. Ключи в snake_case и без пробелов по той же причине.
func (s *RunStats) SummaryLine() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "[RUN_SUMMARY] duration=%s", s.FinishedAt.Sub(s.StartedAt).Round(time.Second))
	fmt.Fprintf(&b, " movies=%d tv_shows=%d episodes=%d", s.MoviesProcessed, s.TVShowsProcessed, s.EpisodesProcessed)
	fmt.Fprintf(&b, " updated=%d unchanged=%d backups=%d default_overridden=%d",
		s.Updated, s.Unchanged, s.BackupsCreated, s.DefaultOverridden)
	fmt.Fprintf(&b, " pending=%d no_id=%d no_access=%d skipped=%d errors=%d",
		s.Pending, s.NoID, s.NoAccess, s.Skipped, s.Errors)
	fmt.Fprintf(&b, " cache=%d", s.FromCache)
	for _, provider := range s.sortedProviders() {
		fmt.Fprintf(&b, " %s=%d", provider, s.providerRequests[provider])
	}
	return b.String()
}
