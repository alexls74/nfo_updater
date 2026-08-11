// internal/scheduler/cron.go
package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule — cron-выражение из 5 полей: minute hour dom month dow.
type Schedule struct {
	minutes, hours, doms, months, dows map[int]bool
	domIsWildcard, dowIsWildcard       bool
}

// ParseCron разбирает стандартную 5-польную cron-строку, например "0 3 * * 1".
// День недели: 0 и 7 оба означают воскресенье.
func ParseCron(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression must have 5 fields (minute hour day month weekday), got %d: %q", len(fields), expr)
	}

	s := &Schedule{}
	var err error
	if s.minutes, err = parseField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute field: %w", err)
	}
	if s.hours, err = parseField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour field: %w", err)
	}
	if s.doms, err = parseField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("day-of-month field: %w", err)
	}
	if s.months, err = parseField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("month field: %w", err)
	}
	if s.dows, err = parseField(fields[4], 0, 7); err != nil {
		return nil, fmt.Errorf("day-of-week field: %w", err)
	}
	if s.dows[7] {
		s.dows[0] = true
	}
	s.domIsWildcard = fields[2] == "*"
	s.dowIsWildcard = fields[4] == "*"
	return s, nil
}

func parseField(field string, min, max int) (map[int]bool, error) {
	out := make(map[int]bool)
	for _, part := range strings.Split(field, ",") {
		rangePart, step := part, 1
		if i := strings.Index(part, "/"); i >= 0 {
			var err error
			step, err = strconv.Atoi(part[i+1:])
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("invalid step in %q", part)
			}
			rangePart = part[:i]
		}

		lo, hi := min, max
		if rangePart != "*" {
			if i := strings.Index(rangePart, "-"); i >= 0 {
				var err error
				lo, err = strconv.Atoi(rangePart[:i])
				if err != nil {
					return nil, fmt.Errorf("invalid range start in %q", part)
				}
				hi, err = strconv.Atoi(rangePart[i+1:])
				if err != nil {
					return nil, fmt.Errorf("invalid range end in %q", part)
				}
			} else {
				v, err := strconv.Atoi(rangePart)
				if err != nil {
					return nil, fmt.Errorf("invalid value %q", rangePart)
				}
				lo, hi = v, v
			}
		}
		if lo < min || hi > max || lo > hi {
			return nil, fmt.Errorf("value out of range [%d-%d] in %q", min, max, part)
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	return out, nil
}

// Next возвращает ближайший момент времени строго после after, удовлетворяющий расписанию.
func (s *Schedule) Next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	deadline := after.AddDate(4, 0, 0)
	for t.Before(deadline) {
		if s.matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

func (s *Schedule) matches(t time.Time) bool {
	if !s.minutes[t.Minute()] || !s.hours[t.Hour()] || !s.months[int(t.Month())] {
		return false
	}
	domMatch := s.doms[t.Day()]
	dowMatch := s.dows[int(t.Weekday())]
	switch {
	case s.domIsWildcard && s.dowIsWildcard:
		return true
	case s.domIsWildcard:
		return dowMatch
	case s.dowIsWildcard:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}
