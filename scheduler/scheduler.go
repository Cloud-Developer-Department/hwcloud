package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// JobTimeout bounds one job invocation. A stuck job must never wedge the
// scheduler: the fire goroutine is skipped on the next tick if still
// running (no catch-up — missed runs are dropped by design).
const JobTimeout = 30 * time.Second

// Job is a registered scheduled job.
type Job struct {
	ID      string // unique registration id (e.g. "pluginname/jobid")
	Cron    string // original expression, for status/debugging
	NextRun time.Time
	LastRun time.Time
	Running bool
	Runs    uint64 // invocations that returned (contained panics not counted)

	sched *Schedule
	fn    func(ctx context.Context, scheduledAt time.Time)
}

// Snapshot is a race-free view of a job for status endpoints and tests.
type Snapshot struct {
	ID      string
	Cron    string
	NextRun time.Time
	LastRun time.Time
	Running bool
	Runs    uint64
}

// Scheduler fires registered jobs when their cron expression matches.
// Jobs never run concurrently with themselves (a still-running job is
// skipped at its next fire time — missed runs are dropped, not caught up);
// different jobs run in parallel. Zero value is not usable — use New.
type Scheduler struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

// New creates an empty scheduler. Jobs are in-memory only; callers
// re-register on restart (plugins re-register via Discover).
func New() *Scheduler {
	return &Scheduler{jobs: make(map[string]*Job)}
}

// Register adds a job. fn is called on fire in its own goroutine with a
// timeout context; it must not block indefinitely (the 30s JobTimeout
// cancels it, and the fire goroutine is skipped while still running).
// Registering an existing id replaces the job.
//
// Expressions that never match — "0 0 30 2 *" has no valid date — are
// rejected here: a job with a zero NextRun would be skipped forever in
// silence, so it is better to fail registration loudly.
func (s *Scheduler) Register(id, expr string, fn func(ctx context.Context, scheduledAt time.Time)) error {
	sched, err := Parse(expr)
	if err != nil {
		return err
	}
	j := &Job{
		ID:      id,
		Cron:    expr,
		NextRun: sched.Next(time.Now()),
		sched:   sched,
		fn:      fn,
	}
	if j.NextRun.IsZero() {
		return fmt.Errorf("cron %q never matches (no fire time within 8 years)", expr)
	}
	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()
	return nil
}

// Unregister removes a job. A fire already in flight completes normally.
func (s *Scheduler) Unregister(id string) {
	s.mu.Lock()
	delete(s.jobs, id)
	s.mu.Unlock()
}

// Jobs returns a snapshot of the registered jobs.
func (s *Scheduler) Jobs() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Snapshot, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, Snapshot{
			ID:      j.ID,
			Cron:    j.Cron,
			NextRun: j.NextRun,
			LastRun: j.LastRun,
			Running: j.Running,
			Runs:    j.Runs,
		})
	}
	return out
}

// Run drives the scheduler until ctx is cancelled. Second-precision tick:
// jobs fire when the current time passes NextRun (minute granularity).
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.tick(now)
		}
	}
}

// tick fires every job whose NextRun has passed. NextRun always advances
// (missed runs are dropped), even when the previous run is still in
// flight — a long-running job never piles up.
func (s *Scheduler) tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.NextRun.IsZero() || now.Before(j.NextRun) {
			continue
		}
		j.NextRun = j.sched.Next(now)
		if j.Running {
			slog.Warn("scheduled job skipped (previous run still active)", "job", j.ID)
			continue
		}
		j.Running = true
		j.LastRun = now
		go s.fire(j, now)
	}
}

// fire runs one job invocation under the timeout, clears Running, and
// contains panics — a crashing job must not take down the process.
func (s *Scheduler) fire(j *Job, at time.Time) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("scheduled job panicked", "job", j.ID, "panic", rec)
		}
		s.mu.Lock()
		j.Running = false
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), JobTimeout)
	defer cancel()
	j.fn(ctx, at)
	s.mu.Lock()
	j.Runs++
	s.mu.Unlock()
}
