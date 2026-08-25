package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegisterInvalidExpression(t *testing.T) {
	s := New()
	if err := s.Register("bad", "not a cron", func(ctx context.Context, at time.Time) {}); err == nil {
		t.Fatal("Register with invalid cron must fail")
	}
	if len(s.Jobs()) != 0 {
		t.Fatal("failed register must not leave a job behind")
	}
}

// "0 0 30 2 *" parses fine but has no valid date — Register must reject
// it instead of installing a job that never fires (silently dead).
func TestRegisterRejectsNeverMatching(t *testing.T) {
	s := New()
	if err := s.Register("never", "0 0 30 2 *", func(ctx context.Context, at time.Time) {}); err == nil {
		t.Fatal("Register must reject a schedule that never matches")
	}
	if len(s.Jobs()) != 0 {
		t.Fatal("rejected register must not leave a job behind")
	}
	// The leap-year case must NOT be rejected: 2/29 fires in leap years.
	if err := s.Register("leap", "0 0 29 2 *", func(ctx context.Context, at time.Time) {}); err != nil {
		t.Fatalf("leap-year schedule rejected: %v", err)
	}
	if j := s.Jobs()[0]; j.NextRun.IsZero() {
		t.Fatal("leap-year job must have a NextRun")
	}
}

func TestTickFiresAndAdvancesNextRun(t *testing.T) {
	s := New()
	var fired atomic.Int32
	if err := s.Register("everymin", "* * * * *", func(ctx context.Context, at time.Time) {
		fired.Add(1)
	}); err != nil {
		t.Fatal(err)
	}
	jobs := s.Jobs()
	if len(jobs) != 1 || jobs[0].NextRun.IsZero() {
		t.Fatalf("Jobs = %+v, want one job with a NextRun", jobs)
	}
	// One tick past NextRun fires the job.
	s.tick(jobs[0].NextRun.Add(time.Second))
	waitFor(t, func() bool { return fired.Load() == 1 })
	if s.Jobs()[0].Runs != 1 {
		t.Fatalf("Runs = %d, want 1", s.Jobs()[0].Runs)
	}
	// NextRun advanced: the same tick must not fire again.
	prev := s.Jobs()[0].NextRun
	s.tick(prev.Add(-time.Second)) // nothing due
	waitFor(t, func() bool { return fired.Load() == 1 })
}

func TestNoReentrancy(t *testing.T) {
	s := New()
	var fired atomic.Int32
	started := make(chan struct{})
	if err := s.Register("blocked", "* * * * *", func(ctx context.Context, at time.Time) {
		fired.Add(1)
		close(started)
		<-ctx.Done() // block until the JobTimeout cancels the context
	}); err != nil {
		t.Fatal(err)
	}
	first := s.Jobs()[0].NextRun
	s.tick(first.Add(time.Second)) // start the blocked fire goroutine
	<-started                      // it is now running (blocked on ctx.Done)
	// A due tick while the job is still running must skip it.
	s.tick(first.Add(time.Minute))
	if fired.Load() != 1 {
		t.Fatalf("fired = %d, want 1 (no re-entry)", fired.Load())
	}
	if got := s.Jobs()[0].Running; !got {
		t.Fatal("job should still be running")
	}
	// NextRun advanced despite the skip — missed runs are dropped.
	if next := s.Jobs()[0].NextRun; !next.After(first.Add(time.Minute)) {
		t.Fatalf("NextRun did not advance past the skip: %v", next)
	}
	// The fire goroutine finishes itself once JobTimeout elapses; nothing
	// to assert here beyond the scheduler staying consistent.
}

func TestUnregisterStopsFires(t *testing.T) {
	s := New()
	var fired atomic.Int32
	if err := s.Register("gone", "* * * * *", func(ctx context.Context, at time.Time) {
		fired.Add(1)
	}); err != nil {
		t.Fatal(err)
	}
	s.Unregister("gone")
	if len(s.Jobs()) != 0 {
		t.Fatal("job must be gone after Unregister")
	}
	s.tick(time.Now().Add(time.Hour))
	if fired.Load() != 0 {
		t.Fatal("unregistered job must not fire")
	}
}

func TestRegisterReplaces(t *testing.T) {
	s := New()
	var fired atomic.Int32
	replace := func() {
		s.Register("dup", "* * * * *", func(ctx context.Context, at time.Time) { fired.Add(1) })
	}
	replace()
	replace() // same id again
	if len(s.Jobs()) != 1 {
		t.Fatalf("Jobs = %d, want 1 after replacement", len(s.Jobs()))
	}
	s.tick(time.Now().Add(time.Hour))
	waitFor(t, func() bool { return fired.Load() == 1 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
