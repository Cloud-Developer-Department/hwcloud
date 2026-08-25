// Package scheduler provides cron-based job scheduling for WASM plugin
// scheduled tasks.
//
// A plugin declares cron jobs in its metadata (schedules); the host
// registers them here at load time and fires a callback when a job's
// schedule matches. Schedules are in-memory and process-local: a restart
// re-registers every job from the plugin metadata it rediscovers.
//
// The expression format is the standard 5-field cron:
//
//	minute hour day-of-month month day-of-week
//	 0-59   0-23      1-31      1-12      0-6 (0=Sunday)
//
// Each field supports `*`, a single value, a range `a-b`, a step `*/n`
// (or `a-b/n`), and comma lists of any of those. day-of-month and
// day-of-week are OR-ed (Vixie cron semantics): when both are
// restricted, either matching day fires the job.
package scheduler

import (
	"fmt"
	"strings"
	"time"
)

// field masks — each field compiles into a 64-bit bitmap.
const (
	fieldMinute = 0
	fieldHour   = 1
	fieldDOM    = 2
	fieldMonth  = 3
	fieldDOW    = 4
)

var fieldLimits = [5][2]int{
	fieldMinute: {0, 59},
	fieldHour:   {0, 23},
	fieldDOM:    {1, 31},
	fieldMonth:  {1, 12},
	fieldDOW:    {0, 6},
}

// Schedule is a parsed 5-field cron expression.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	// Whether the day fields were given as `*` — day-of-month and
	// day-of-week are OR-ed ONLY when both are restricted (Vixie cron).
	// If either is a star, the other one alone selects the day.
	domStar, dowStar bool
}

// Parse compiles a 5-field cron expression.
func Parse(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: want 5 fields, got %d: %q", len(fields), expr)
	}
	s := &Schedule{}
	masks := []*uint64{&s.minute, &s.hour, &s.dom, &s.month, &s.dow}
	for i, field := range fields {
		mask, star, err := parseField(field, fieldLimits[i][0], fieldLimits[i][1])
		if err != nil {
			return nil, fmt.Errorf("cron: field %d (%q): %w", i+1, field, err)
		}
		*masks[i] = mask
		switch i {
		case fieldDOM:
			s.domStar = star
		case fieldDOW:
			s.dowStar = star
		}
	}
	if s.dom == 0 || s.month == 0 || s.dow == 0 {
		return nil, fmt.Errorf("cron: empty range: %q", expr)
	}
	return s, nil
}

// parseField compiles one cron field into a bitmap. Supports `*`, `n`,
// `a-b`, `*/n`, `a-b/n`, and comma-separated lists of those. The bool
// reports whether the whole field was a bare `*`.
func parseField(field string, lo, hi int) (mask uint64, star bool, err error) {
	star = field == "*"
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return 0, false, fmt.Errorf("empty element in %q", field)
		}
		step := 1
		base := part
		if i := strings.IndexByte(base, '/'); i >= 0 {
			step, err = parseNum(base[i+1:], 1, hi-lo)
			if err != nil {
				return 0, false, fmt.Errorf("step: %w", err)
			}
			base = base[:i]
		}
		var from, to int
		if base == "*" {
			from, to = lo, hi
		} else if i := strings.IndexByte(base, '-'); i >= 0 {
			from, err = parseNum(base[:i], lo, hi)
			if err != nil {
				return 0, false, err
			}
			to, err = parseNum(base[i+1:], from, hi)
			if err != nil {
				return 0, false, err
			}
		} else {
			n, err := parseNum(base, lo, hi)
			if err != nil {
				return 0, false, err
			}
			from, to = n, n
		}
		for v := from; v <= to; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, star, nil
}

func parseNum(s string, lo, hi int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad number %q", s)
		}
		n = n*10 + int(c-'0')
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%d out of range [%d,%d]", n, lo, hi)
	}
	return n, nil
}

// Match reports whether t (minute precision) satisfies the schedule.
func (s *Schedule) Match(t time.Time) bool {
	if s.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	if s.minute&(1<<uint(t.Minute())) == 0 {
		return false
	}
	if s.hour&(1<<uint(t.Hour())) == 0 {
		return false
	}
	// Day selection (Vixie cron): when one day field is `*`, the other
	// alone selects the day; only when both are restricted are they OR-ed.
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(int(t.Weekday()))) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowOK
	case s.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}

// Next returns the first time strictly after `after` matching the
// schedule, or the zero time if none matches within the next 8 years.
//
// The window must cover a full leap-year cycle: "0 0 29 2 *" registered
// in a non-leap year finds its first match in the next leap year. (A
// narrower window would make such jobs silently dead — NextRun would be
// zero and the scheduler skips zero NextRuns forever.) Expressions that
// never match — "0 0 30 2 *" — return zero after the window; callers
// (Register) reject those.
func (s *Schedule) Next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.AddDate(8, 0, 0)
	for !t.After(limit) {
		if s.Match(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}
