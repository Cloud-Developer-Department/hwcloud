package scheduler

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestParseRejectsBadExpressions(t *testing.T) {
	bad := []string{
		"", "* * *", "* * * * * *",        // wrong field count
		"61 * * * *", "* 24 * * *",        // out of range
		"* * 0 * *", "* * 32 * *",         // dom range
		"* * * 0 *", "* * * 13 *",         // month range
		"* * * * 7",                       // dow range
		"*/0 * * * *",                     // zero step
		"a * * * *", "1- * * * *",         // garbage
		"1,,3 * * * *",                    // empty list element
		"* * * * ",                        // trailing space
	}
	for _, expr := range bad {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) = nil error, want error", expr)
		}
	}
}

func TestParseFieldForms(t *testing.T) {
	// every minute
	s := mustParse(t, "* * * * *")
	if !s.Match(time.Date(2026, 8, 8, 12, 34, 0, 0, time.Local)) {
		t.Fatal("every-minute must match anything")
	}
	// step
	s = mustParse(t, "*/5 * * * *")
	for _, m := range []int{0, 5, 55} {
		if !s.Match(time.Date(2026, 8, 8, 12, m, 0, 0, time.Local)) {
			t.Errorf("*/5 must match minute %d", m)
		}
	}
	for _, m := range []int{1, 4, 59} {
		if s.Match(time.Date(2026, 8, 8, 12, m, 0, 0, time.Local)) {
			t.Errorf("*/5 must NOT match minute %d", m)
		}
	}
	// list
	s = mustParse(t, "1,15,30 * * * *")
	for _, m := range []int{1, 15, 30} {
		if !s.Match(time.Date(2026, 8, 8, 12, m, 0, 0, time.Local)) {
			t.Errorf("list must match minute %d", m)
		}
	}
	// range with step
	s = mustParse(t, "0-30/10 * * * *")
	for _, m := range []int{0, 10, 20, 30} {
		if !s.Match(time.Date(2026, 8, 8, 12, m, 0, 0, time.Local)) {
			t.Errorf("range-step must match minute %d", m)
		}
	}
	if s.Match(time.Date(2026, 8, 8, 12, 40, 0, 0, time.Local)) {
		t.Error("range-step must NOT match minute 40")
	}
}

func TestNextDaily(t *testing.T) {
	s := mustParse(t, "0 9 * * *")
	// 8:30 today → 9:00 today
	next := s.Next(time.Date(2026, 8, 8, 8, 30, 0, 0, time.Local))
	want := time.Date(2026, 8, 8, 9, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v", next, want)
	}
	// 9:00 sharp → tomorrow 9:00 (strictly after)
	next = s.Next(time.Date(2026, 8, 8, 9, 0, 0, 0, time.Local))
	want = time.Date(2026, 8, 9, 9, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("Next(9:00) = %v, want %v", next, want)
	}
	// 23:59 → tomorrow 9:00
	next = s.Next(time.Date(2026, 8, 8, 23, 59, 0, 0, time.Local))
	if !next.Equal(want) {
		t.Fatalf("Next(23:59) = %v, want %v", next, want)
	}
}

func TestNextStepMinutes(t *testing.T) {
	s := mustParse(t, "*/5 * * * *")
	// 12:03 → 12:05
	next := s.Next(time.Date(2026, 8, 8, 12, 3, 0, 0, time.Local))
	want := time.Date(2026, 8, 8, 12, 5, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v", next, want)
	}
	// 12:55 → 13:00 (hour rollover)
	next = s.Next(time.Date(2026, 8, 8, 12, 55, 0, 0, time.Local))
	want = time.Date(2026, 8, 8, 13, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v", next, want)
	}
}

func TestNextMonthEnd(t *testing.T) {
	// "0 0 31 * *" fires only in months with 31 days: Aug 31 → Sep 31
	// does not exist → Oct 31.
	s := mustParse(t, "0 0 31 * *")
	next := s.Next(time.Date(2026, 8, 8, 0, 0, 0, 0, time.Local))
	want := time.Date(2026, 8, 31, 0, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v", next, want)
	}
	next = s.Next(time.Date(2026, 8, 31, 0, 0, 0, 0, time.Local))
	want = time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v (Sep has no 31st)", next, want)
	}
}

func TestNextYearBoundary(t *testing.T) {
	s := mustParse(t, "0 0 1 1 *") // Jan 1, yearly
	next := s.Next(time.Date(2026, 12, 31, 23, 59, 0, 0, time.Local))
	want := time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v", next, want)
	}
}

func TestNextLeapYear(t *testing.T) {
	// "0 0 29 2 *" registered in a NON-leap year (2026, 2027) must find
	// the next leap-year Feb 29 — the 8-year window covers the cycle.
	s := mustParse(t, "0 0 29 2 *")
	next := s.Next(time.Date(2026, 8, 8, 0, 0, 0, 0, time.Local))
	want := time.Date(2028, 2, 29, 0, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v (2028 is a leap year)", next, want)
	}
	// Fired in the leap year, the next fire is four years later.
	next = s.Next(time.Date(2028, 2, 29, 0, 0, 0, 0, time.Local))
	want = time.Date(2032, 2, 29, 0, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v", next, want)
	}
}

// Expressions with no valid date at all — Feb 30 — must return zero so
// Register can reject them loudly instead of silently never firing.
func TestNextNeverMatches(t *testing.T) {
	s := mustParse(t, "0 0 30 2 *")
	if next := s.Next(time.Now()); !next.IsZero() {
		t.Fatalf("Next = %v, want zero (Feb 30 does not exist)", next)
	}
}

func TestNextWeekday(t *testing.T) {
	// "0 9 * * 1" — Mondays. 2026-08-08 is a Saturday.
	s := mustParse(t, "0 9 * * 1")
	next := s.Next(time.Date(2026, 8, 8, 0, 0, 0, 0, time.Local))
	want := time.Date(2026, 8, 10, 9, 0, 0, 0, time.Local) // next Monday
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v", next, want)
	}
}

func TestDOMDOWAreORed(t *testing.T) {
	// "0 0 13 * 5" — Vixie semantics: the 13th OR any Friday.
	s := mustParse(t, "0 0 13 * 5")
	// 2026-08-08 is a Saturday; the 13th of August 2026 is a Thursday.
	// Either match must fire the job; the FIRST of them: 13th.
	next := s.Next(time.Date(2026, 8, 8, 0, 0, 0, 0, time.Local))
	want := time.Date(2026, 8, 13, 0, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v (13th)", next, want)
	}
	// After the 13th, the next Friday (14th) fires.
	next = s.Next(time.Date(2026, 8, 13, 0, 0, 0, 0, time.Local))
	want = time.Date(2026, 8, 14, 0, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v (Friday)", next, want)
	}
}
