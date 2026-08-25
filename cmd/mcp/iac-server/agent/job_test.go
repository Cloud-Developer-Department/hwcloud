package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestJobManager_BasicDone(t *testing.T) {
	m := NewJobManager(t.TempDir())
	id, err := m.Submit(context.Background(), "", "", "test", func(ctx context.Context) (string, error) {
		return `{"ok":true}`, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := m.Get(context.Background(), id, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != JobDone {
		t.Fatalf("expected done, got %s", job.Status)
	}
	if !strings.Contains(string(job.Result), `"ok": true`) {
		t.Fatalf("unexpected result: %s", job.Result)
	}
}

func TestJobManager_Failure(t *testing.T) {
	m := NewJobManager(t.TempDir())
	id, _ := m.Submit(context.Background(), "", "", "test", func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("boom")
	})
	job, _ := m.Get(context.Background(), id, 5*time.Second)
	if job.Status != JobFailed {
		t.Fatalf("expected failed, got %s", job.Status)
	}
	if job.Error != "boom" {
		t.Fatalf("unexpected error: %s", job.Error)
	}
}

func TestJobManager_PanicRecovery(t *testing.T) {
	m := NewJobManager(t.TempDir())
	id, _ := m.Submit(context.Background(), "", "", "test", func(ctx context.Context) (string, error) {
		panic("kaboom")
	})
	job, _ := m.Get(context.Background(), id, 5*time.Second)
	if job.Status != JobFailed {
		t.Fatalf("expected failed after panic, got %s", job.Status)
	}
	if job.Error == "" || !strings.Contains(job.Error, "panic") {
		t.Fatalf("error should mention panic, got: %s", job.Error)
	}
}

func TestJobManager_Supersession(t *testing.T) {
	m := NewJobManager(t.TempDir())
	dep := "d-supersede"

	// job1: blocks until we release the barrier
	barrier := make(chan struct{})
	id1, _ := m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
		<-barrier
		return `{"job":1}`, nil
	})

	// job2 for same dep: should supersede job1
	id2, _ := m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
		return `{"job":2}`, nil
	})

	if id1 == id2 {
		t.Fatal("expected different job ids")
	}

	// job1 should be cancelled
	job1, _ := m.Get(context.Background(), id1, 2*time.Second)
	if job1.Status != JobCancelled {
		t.Fatalf("job1 should be cancelled, got %s", job1.Status)
	}

	close(barrier) // let job1's fn return (it's already marked cancelled)

	// job2 should complete
	job2, _ := m.Get(context.Background(), id2, 5*time.Second)
	if job2.Status != JobDone {
		t.Fatalf("job2 should be done, got %s", job2.Status)
	}

	// Re-read job1 after its fn has returned — done() must NOT overwrite
	// the cancelled status. This is the bug where done() lacks the
	// JobCancelled guard that fail() has.
	job1Final, _ := m.Get(context.Background(), id1, 2*time.Second)
	if job1Final.Status != JobCancelled {
		t.Fatalf("job1 should remain cancelled after fn returns, got %s (result=%s) — done() overwrote cancelled", job1Final.Status, job1Final.Result)
	}
}

// TestJobManager_CrossSessionReject verifies that a second session operating
// on the same deployment is rejected rather than silently superseding the
// first session's job. Same-session retry still supersedes (tested above).
func TestJobManager_CrossSessionReject(t *testing.T) {
	m := NewJobManager(t.TempDir())
	dep := "d-cross-session"

	// Session A starts a long-running job.
	barrier := make(chan struct{})
	idA, err := m.Submit(context.Background(), dep, "session-A", "test", func(ctx context.Context) (string, error) {
		<-barrier
		return `{"session":"A"}`, nil
	})
	if err != nil {
		t.Fatalf("session A submit: %v", err)
	}

	// Session B tries the same deployment — must be rejected.
	_, err = m.Submit(context.Background(), dep, "session-B", "test", func(ctx context.Context) (string, error) {
		return `{"session":"B"}`, nil
	})
	if err == nil {
		t.Fatal("session B should be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Fatalf("error should mention busy, got: %s", err)
	}

	// Session A retrying (same session ID) should supersede its own prior job.
	idA2, err := m.Submit(context.Background(), dep, "session-A", "test", func(ctx context.Context) (string, error) {
		return `{"session":"A-retry"}`, nil
	})
	if err != nil {
		t.Fatalf("session A retry submit: %v", err)
	}

	// Original job A should be cancelled (superseded by A retry).
	jobA, _ := m.Get(context.Background(), idA, 2*time.Second)
	if jobA.Status != JobCancelled {
		t.Fatalf("job A should be cancelled by same-session retry, got %s", jobA.Status)
	}

	close(barrier)

	// A retry should complete.
	jobA2, _ := m.Get(context.Background(), idA2, 5*time.Second)
	if jobA2.Status != JobDone {
		t.Fatalf("job A retry should be done, got %s", jobA2.Status)
	}
}

// TestJobManager_SupersessionDoneOverwritesCancelled_Bug is an adversarial
// reproduction of the done()-overwrites-cancelled bug. It deterministically
// forces the superseded job's fn to return nil error AFTER cancelLocked has
// persisted cancelled, then checks the final persisted status.
func TestJobManager_SupersessionDoneOverwritesCancelled_Bug(t *testing.T) {
	m := NewJobManager(t.TempDir())
	dep := "d-bug"

	// job1's fn blocks on a channel we control, then returns nil error
	// (the done path — the one without a JobCancelled guard).
	release := make(chan struct{})
	id1, _ := m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
		<-release
		return `{"job":1}`, nil
	})

	// job2 supersedes job1 — cancelLocked writes Status=cancelled to disk.
	id2, _ := m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
		return `{"job":2}`, nil
	})
	if id1 == id2 {
		t.Fatal("expected different job ids")
	}

	// Wait for job1 to be marked cancelled.
	job1Mid, _ := m.Get(context.Background(), id1, 2*time.Second)
	if job1Mid.Status != JobCancelled {
		t.Fatalf("job1 should be cancelled after supersession, got %s", job1Mid.Status)
	}

	// Release job1's fn — it returns nil error → m.done(job1) runs.
	// done() has no JobCancelled guard, so it overwrites cancelled → done.
	close(release)

	// Give the goroutine time to call done() and persist.
	time.Sleep(200 * time.Millisecond)

	job1Final, _ := m.Get(context.Background(), id1, 0)
	if job1Final.Status == JobDone {
		t.Fatalf("BUG CONFIRMED: done() overwrote cancelled → done (result=%s). "+
			"done() needs `if job.Status == JobCancelled { return }` guard like fail().",
			job1Final.Result)
	}
	if job1Final.Status != JobCancelled {
		t.Fatalf("job1 should still be cancelled, got %s", job1Final.Status)
	}
}

func TestJobManager_ConcurrencyLimit(t *testing.T) {
	m := NewJobManager(t.TempDir())

	var active, maxActive int32
	release := make(chan struct{})

	// Submit maxConcurrentJobs+1 jobs with distinct deployments (no supersession).
	for i := 0; i < maxConcurrentJobs+1; i++ {
		dep := "d-" + string(rune('a'+i))
		_, _ = m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
			cur := atomic.AddInt32(&active, 1)
			for {
				old := atomic.LoadInt32(&maxActive)
				if cur <= old || atomic.CompareAndSwapInt32(&maxActive, old, cur) {
					break
				}
			}
			<-release
			atomic.AddInt32(&active, -1)
			return `{}`, nil
		})
	}

	// Wait until maxConcurrentJobs are active (or timeout). A fixed sleep
	// is flaky on slow CI; this deterministically waits for the sem to fill.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&maxActive) == maxConcurrentJobs {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&maxActive); got != maxConcurrentJobs {
		t.Fatalf("expected exactly %d active jobs, got %d (sem not filled)", maxConcurrentJobs, got)
	}

	close(release) // let all jobs finish

	// Wait for all job goroutines to finish writing their job files before
	// t.TempDir cleanup races with a .tmp write. Fail on timeout — a silent
	// pass here (as the original did) masks a sem-leak bug.
	finishDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(finishDeadline) {
		if atomic.LoadInt32(&active) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&active); got != 0 {
		t.Fatalf("%d job(s) still active after release — sem leak or goroutine stuck", got)
	}
}

func TestJobManager_PersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	m1 := NewJobManager(dir)
	id, _ := m1.Submit(context.Background(), "", "", "test", func(ctx context.Context) (string, error) {
		return `{"persist":true}`, nil
	})
	job, _ := m1.Get(context.Background(), id, 5*time.Second)
	if job.Status != JobDone {
		t.Fatalf("expected done, got %s", job.Status)
	}

	// New manager reading the same dir should find the old job.
	m2 := NewJobManager(dir)
	job2, err := m2.Get(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if job2 == nil {
		t.Fatal("new manager should find persisted job")
	}
	if job2.Status != JobDone {
		t.Fatalf("persisted job should be done, got %s", job2.Status)
	}
}

func TestJobManager_LongPoll(t *testing.T) {
	m := NewJobManager(t.TempDir())
	id, _ := m.Submit(context.Background(), "", "", "test", func(ctx context.Context) (string, error) {
		time.Sleep(200 * time.Millisecond)
		return `{"done":true}`, nil
	})
	// Long-poll should return done after the job finishes.
	job, _ := m.Get(context.Background(), id, 3*time.Second)
	if job.Status != JobDone {
		t.Fatalf("expected done, got %s", job.Status)
	}
}

func TestJobManager_OutputTruncation(t *testing.T) {
	m := NewJobManager(t.TempDir())
	id, _ := m.Submit(context.Background(), "", "", "test", func(ctx context.Context) (string, error) {
		sink := JobOutputsFromContext(ctx)
		if sink == nil {
			t.Fatal("output sink should be in ctx")
		}
		// Send a very long output
		long := make([]byte, outputEntryMax*2)
		for i := range long {
			long[i] = 'x'
		}
		sink(string(long))
		// Send many outputs
		for i := 0; i < outputMaxCount+10; i++ {
			sink("msg")
		}
		return `{}`, nil
	})
	job, _ := m.Get(context.Background(), id, 5*time.Second)
	if job.Status != JobDone {
		t.Fatalf("expected done, got %s", job.Status)
	}
	if len(job.Outputs) > outputMaxCount {
		t.Fatalf("outputs %d exceed max %d", len(job.Outputs), outputMaxCount)
	}
	// First output should be truncated to outputEntryMax + "..."
	if len(job.Outputs[0]) > outputEntryMax+10 {
		t.Fatalf("first output not truncated: len=%d", len(job.Outputs[0]))
	}
}

func TestJobManager_UnknownJob(t *testing.T) {
	m := NewJobManager(t.TempDir())
	job, err := m.Get(context.Background(), "job-nonexistent", 0)
	if err != nil {
		t.Fatalf("unknown job should not error: %v", err)
	}
	if job != nil {
		t.Fatal("unknown job should return nil")
	}
}

// TestJobManager_OrphanedRunningJobAfterCrash simulates a process crash
// mid-job: a job file with status "running" is left on disk. A new JobManager
// (post-restart) detects the orphan and marks it failed so the client can
// retry instead of polling a status that will never change.
func TestJobManager_OrphanedRunningJobAfterCrash(t *testing.T) {
	dir := t.TempDir()

	// Simulate a crash: write a job file directly with status=running.
	orphan := Job{
		ID:        "job-orphan",
		Tool:      "specify_resources",
		Status:    JobRunning,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, _ := json.MarshalIndent(orphan, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "job-orphan.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// New manager after restart — it should fail the orphan.
	m := NewJobManager(dir)
	job, err := m.Get(context.Background(), "job-orphan", 0)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Fatal("orphaned job file should be readable")
	}
	if job.Status != JobFailed {
		t.Fatalf("orphaned running job should be marked failed on restart, got %s", job.Status)
	}
	if !strings.Contains(job.Error, "orphaned") {
		t.Fatalf("error should mention orphaned, got: %s", job.Error)
	}
}

// TestJobManager_AppliedJobNotResumedOnRestart verifies that a done job
// stays done across restart (positive control for the orphan test above).
func TestJobManager_AppliedJobNotResumedOnRestart(t *testing.T) {
	dir := t.TempDir()
	m1 := NewJobManager(dir)
	id, _ := m1.Submit(context.Background(), "", "", "test", func(ctx context.Context) (string, error) {
		return `{"ok":true}`, nil
	})
	j1, _ := m1.Get(context.Background(), id, 5*time.Second)
	if j1.Status != JobDone {
		t.Fatalf("expected done, got %s", j1.Status)
	}

	m2 := NewJobManager(dir)
	j2, _ := m2.Get(context.Background(), id, 0)
	if j2 == nil || j2.Status != JobDone {
		t.Fatalf("done job should remain done after restart, got %+v", j2)
	}
}

// TestJobManager_CleanupStaleRemovesOldJobs verifies that job files older
// than 24h are deleted at startup, while fresh ones survive.
func TestJobManager_CleanupStaleRemovesOldJobs(t *testing.T) {
	dir := t.TempDir()

	// Write an old job file (>24h) by backdating its mtime.
	old := Job{ID: "job-old", Tool: "test", Status: JobDone,
		CreatedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)}
	data, _ := json.MarshalIndent(old, "", "  ")
	oldPath := filepath.Join(dir, "job-old.json")
	if err := os.WriteFile(oldPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldPath, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Write a fresh job file (<24h).
	fresh := Job{ID: "job-fresh", Tool: "test", Status: JobDone,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	data, _ = json.MarshalIndent(fresh, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "job-fresh.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	NewJobManager(dir) // triggers cleanupStale

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("old job file should be removed by cleanupStale")
	}
	if _, err := os.Stat(filepath.Join(dir, "job-fresh.json")); err != nil {
		t.Fatal("fresh job file should survive cleanupStale")
	}
}

// TestJobManager_OrphanedRunningNotCleanedUpBeforeFailing verifies the fix
// for the cleanupStale/failOrphanedRunning ordering bug: a running job file
// older than 24h must be marked failed (not silently deleted) so the client
// can distinguish "crashed" from "never existed".
func TestJobManager_OrphanedRunningNotCleanedUpBeforeFailing(t *testing.T) {
	dir := t.TempDir()

	// A running job that crashed >24h ago — backdated mtime.
	orphan := Job{ID: "job-old-orphan", Tool: "test", Status: JobRunning,
		CreatedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)}
	data, _ := json.MarshalIndent(orphan, "", "  ")
	path := filepath.Join(dir, "job-old-orphan.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// NewJobManager runs failOrphanedRunning BEFORE cleanupStale.
	m := NewJobManager(dir)
	job, err := m.Get(context.Background(), "job-old-orphan", 0)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Fatal("orphaned running job should NOT be deleted before being marked failed — " +
			"failOrphanedRunning must run before cleanupStale")
	}
	if job.Status != JobFailed {
		t.Fatalf("orphaned running job should be marked failed, got %s", job.Status)
	}
}

// ── concurrency ──

// TestJobManager_HighConcurrentDistinctDeployments submits many jobs across
// many distinct deployments concurrently and verifies all complete. This
// stresses the global semaphore and the running map under -race.
func TestJobManager_HighConcurrentDistinctDeployments(t *testing.T) {
	dir := t.TempDir()
	m := NewJobManager(dir)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	var done, failed int32
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			dep := fmt.Sprintf("d-%03d", i)
			id, err := m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
				time.Sleep(10 * time.Millisecond) // simulate work
				return `{}`, nil
			})
			if err != nil {
				atomic.AddInt32(&failed, 1)
				return
			}
			job, _ := m.Get(context.Background(), id, 10*time.Second)
			if job != nil && job.Status == JobDone {
				atomic.AddInt32(&done, 1)
			} else {
				atomic.AddInt32(&failed, 1)
			}
		}(i)
	}
	wg.Wait()
	if d := atomic.LoadInt32(&done); d != N {
		t.Fatalf("expected %d done, got %d (failed=%d)", N, d, atomic.LoadInt32(&failed))
	}
}

// ── regression: cancelLocked / runningMap ──

// TestCancelLocked_DoesNotOverwriteDone is the regression test for the
// adversarial finding: cancelLocked must NOT clobber a job that already
// reached JobDone. Without the guard, a second Submit for the same
// deployment overwrites the done job's status to cancelled, losing the
// result the client was polling for.
func TestCancelLocked_DoesNotOverwriteDone(t *testing.T) {
	m := NewJobManager(t.TempDir())
	dep := "d-done-then-supersede"

	// job1 completes successfully (done).
	id1, _ := m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
		return `{"result":"ok"}`, nil
	})
	job1, _ := m.Get(context.Background(), id1, 5*time.Second)
	if job1.Status != JobDone {
		t.Fatalf("job1 should be done, got %s", job1.Status)
	}

	// job2 supersedes job1 — but job1 is already done. cancelLocked must
	// NOT overwrite job1's status to cancelled.
	id2, _ := m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
		return `{}`, nil
	})
	if id1 == id2 {
		t.Fatal("expected different job ids")
	}

	job1Final, _ := m.Get(context.Background(), id1, 0)
	if job1Final.Status != JobDone {
		t.Fatalf("job1 should remain done after supersession, got %s (error=%s) — cancelLocked overwrote the terminal status",
			job1Final.Status, job1Final.Error)
	}
	if string(job1Final.Result) == "" || string(job1Final.Result) == "null" {
		t.Fatal("job1 result should be preserved, got empty")
	}
	// Let lingering goroutines finalize so t.TempDir cleanup doesn't race.
	time.Sleep(100 * time.Millisecond)
}

// TestCancelLocked_DoesNotOverwriteFailed is the symmetric regression for
// the failed case: a job that panicked (failed) must keep its error message
// even if a new submission arrives for the same deployment.
func TestCancelLocked_DoesNotOverwriteFailed(t *testing.T) {
	m := NewJobManager(t.TempDir())
	dep := "d-failed-then-supersede"

	id1, _ := m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("terraform plan failed: provider not found")
	})
	job1, _ := m.Get(context.Background(), id1, 5*time.Second)
	if job1.Status != JobFailed {
		t.Fatalf("job1 should be failed, got %s", job1.Status)
	}
	wantErr := job1.Error

	id2, _ := m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
		return `{}`, nil
	})
	_, _ = m.Get(context.Background(), id2, 5*time.Second)

	job1Final, _ := m.Get(context.Background(), id1, 0)
	if job1Final.Status != JobFailed {
		t.Fatalf("job1 should remain failed after supersession, got %s", job1Final.Status)
	}
	if job1Final.Error != wantErr {
		t.Fatalf("job1 error changed from %q to %q — cancelLocked overwrote the failure", wantErr, job1Final.Error)
	}
	time.Sleep(100 * time.Millisecond)
}

// TestRunningMap_ReclaimedAfterDone verifies the resource-leak fix: after a
// job completes (done), its entry is removed from m.running and the cancel
// func is called. Without this, m.running grows unbounded over the server
// lifetime. We check reclamation indirectly: a subsequent Submit for the
// same deployment should NOT find a prev to cancel (no spurious cancel).
func TestRunningMap_ReclaimedAfterDone(t *testing.T) {
	m := NewJobManager(t.TempDir())
	dep := "d-reclaim"

	// job1 completes.
	id1, _ := m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
		return `{}`, nil
	})
	_, _ = m.Get(context.Background(), id1, 5*time.Second)

	// job2 for the same dep: since job1's entry was reclaimed, there is no
	// prev to cancel. job1 should still be done (not clobbered).
	id2, _ := m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
		return `{}`, nil
	})
	_, _ = m.Get(context.Background(), id2, 5*time.Second)

	job1, _ := m.Get(context.Background(), id1, 0)
	if job1.Status != JobDone {
		t.Fatalf("job1 should be done (not cancelled — its entry was reclaimed so no supersession occurred), got %s", job1.Status)
	}
}

// TestRunningMap_NoUnboundedGrowth submits many jobs across distinct
// deployments and verifies m.running does not retain all of them. After
// all jobs complete, m.running should be empty (every terminal job reclaims
// its entry).
func TestRunningMap_NoUnboundedGrowth(t *testing.T) {
	m := NewJobManager(t.TempDir())
	const N = 100

	var done int32
	for i := 0; i < N; i++ {
		dep := fmt.Sprintf("d-%03d", i)
		_, _ = m.Submit(context.Background(), dep, "", "test", func(ctx context.Context) (string, error) {
			return `{}`, nil
		})
	}
	// Wait for all to finish and verify m.running is fully reclaimed.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		remaining := len(m.running)
		m.mu.Unlock()
		if remaining == 0 {
			done = int32(N)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&done) != int32(N) {
		m.mu.Lock()
		remaining := len(m.running)
		m.mu.Unlock()
		t.Fatalf("m.running still has %d entries after all jobs completed — unbounded growth not fixed", remaining)
	}
}

// ── fuzz ──

// FuzzTruncate_RuneBoundary verifies truncate never produces invalid UTF-8
// — the byte-walk-back must land on a rune boundary for any input/cut point.
func FuzzTruncate_RuneBoundary(f *testing.F) {
	f.Add("hello世界", 5)
	f.Add("abc", 10)
	f.Add("🇨🇳国旗", 3)
	f.Add("", 0)
	f.Add("a", 0)
	for _, seed := range []string{"x", "日", "🎉"} {
		for _, n := range []int{0, 1, 2, 3, 4} {
			f.Add(seed, n)
		}
	}
	f.Fuzz(func(t *testing.T, s string, n int) {
		// truncate's contract is "cut at a rune boundary" — it assumes valid
		// UTF-8 input (model output). Skip garbage input; that's not what we
		// test here.
		if !utf8.ValidString(s) {
			return
		}
		if n < 0 {
			n = -n
		}
		out := truncate(s, n)
		if !utf8.ValidString(out) {
			t.Fatalf("truncate(%q,%d) = %q (invalid UTF-8)", s, n, out)
		}
		// If no truncation happened, output must equal input.
		if len(s) <= n && out != s {
			t.Fatalf("truncate(%q,%d) = %q, want %q", s, n, out, s)
		}
	})
}

// ── benchmarks ──

// BenchmarkJobManager_ConcurrentSubmit measures job submission throughput
// under contention — multiple deployments submitting in parallel, each
// completing immediately. Stresses the sem + mu + save path.
func BenchmarkJobManager_ConcurrentSubmit(b *testing.B) {
	dir := b.TempDir()
	m := NewJobManager(dir)
	var pending int32
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dep := fmt.Sprintf("d-%d", i%100) // 100 distinct deployments
		atomic.AddInt32(&pending, 1)
		_, err := m.Submit(context.Background(), dep, "", "bench", func(ctx context.Context) (string, error) {
			defer atomic.AddInt32(&pending, -1)
			return `{}`, nil
		})
		if err != nil {
			atomic.AddInt32(&pending, -1)
			b.Fatal(err)
		}
	}
	b.StopTimer()
	// Wait for all job goroutines to finish so t.TempDir cleanup doesn't race
	// with a job file write.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&pending) > 0 {
		time.Sleep(time.Millisecond)
	}
}

// BenchmarkJobManager_SupersededSubmit measures the supersession path —
// repeated submits to the SAME deployment cancel the prior job. This is
// the worst case for the mu + cancel path.
func BenchmarkJobManager_SupersededSubmit(b *testing.B) {
	dir := b.TempDir()
	m := NewJobManager(dir)
	var pending int32
	// Block all jobs so they pile up and get superseded.
	release := make(chan struct{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		atomic.AddInt32(&pending, 1)
		_, err := m.Submit(context.Background(), "d-same", "", "bench", func(ctx context.Context) (string, error) {
			defer atomic.AddInt32(&pending, -1)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return `{}`, nil
		})
		if err != nil {
			atomic.AddInt32(&pending, -1)
			b.Fatal(err)
		}
	}
	b.StopTimer()
	close(release)
	// Drain: wait for all goroutines to finish so they don't outlive the benchmark.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&pending) > 0 {
		time.Sleep(time.Millisecond)
	}
}

// BenchmarkJobManager_ParallelSubmit is the -race-friendly parallel version:
// goroutines submit to distinct deployments concurrently.
func BenchmarkJobManager_ParallelSubmit(b *testing.B) {
	dir := b.TempDir()
	m := NewJobManager(dir)
	var pending int32
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var i int
		for pb.Next() {
			atomic.AddInt32(&pending, 1)
			dep := fmt.Sprintf("d-%d", i%50)
			_, err := m.Submit(context.Background(), dep, "", "bench", func(ctx context.Context) (string, error) {
				defer atomic.AddInt32(&pending, -1)
				return `{}`, nil
			})
			if err != nil {
				atomic.AddInt32(&pending, -1)
				b.Fatal(err)
			}
			i++
		}
	})
	b.StopTimer()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&pending) > 0 {
		time.Sleep(time.Millisecond)
	}
}

// BenchmarkTruncate measures the rune-boundary truncation used for job outputs.
func BenchmarkTruncate(b *testing.B) {
	s := make([]byte, 10000)
	for i := range s {
		s[i] = 'x'
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = truncate(string(s), 2000)
	}
}

// BenchmarkTruncate_Multibyte exercises the rune-boundary backwalk with
// multibyte UTF-8 (CJK characters), the worst case for truncate.
func BenchmarkTruncate_Multibyte(b *testing.B) {
	// 4000 CJK chars = 12000 bytes; truncating at 2000 bytes must walk back
	// to a rune boundary.
	s := ""
	for i := 0; i < 4000; i++ {
		s += "中"
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = truncate(s, 2000)
	}
}
