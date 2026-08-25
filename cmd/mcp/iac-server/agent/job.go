// Job-based async execution for long-running LLM tools.
//
// MCP has no native async tool calls, and tool execution timeouts are a
// client-side concern that varies per client — our agents routinely run
// 2-3 minutes (specify_resources querying flavors, estimate_cost pricing),
// which no client can be assumed to tolerate synchronously. The job
// pattern solves this without assuming anything about the client: the
// tool call returns immediately with a job_id, the client polls
// get_job_result (with optional long-poll wait), and the server runs the
// actual work in a background goroutine.
//
// Jobs persist to <workDir>/jobs/job-<id>.json so a server restart does
// not lose them. Same-deployment jobs from the same MCP session supersede:
// a new submission from that session cancels any running job for that
// deployment (the cancelled job's fn may still be mid-execution — it checks
// ctx.Err() and returns an interrupted error, but saveDag/saveCost are
// ctx-unaware so they complete their atomic write before the goroutine
// exits). Cross-session requests for the same deployment are rejected so
// the second client gets an error instead of silently cancelling the first.
// Different deployments run in parallel, bounded by a global semaphore.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// JobStatus is the lifecycle state of an async job.
type JobStatus string

const (
	JobRunning  JobStatus = "running"
	JobDone     JobStatus = "done"
	JobFailed   JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

// maxConcurrentJobs bounds parallel job execution across all deployments —
// each job runs a full LLM agent, so unbounded parallelism would flood the
// model API.
const maxConcurrentJobs = 4

// JobTimeout bounds a single job's execution.
const JobTimeout = 15 * time.Minute

// outputLimits bound the model output log: each entry is truncated and
// the log keeps only the most recent entries, so a long agent run cannot
// balloon the job file.
const (
	outputEntryMax = 2000
	outputMaxCount = 100
)

// Job is the persisted state of an async tool execution.
type Job struct {
	ID           string          `json:"id"`
	Tool         string          `json:"tool"`
	Status       JobStatus       `json:"status"`
	DeploymentID string          `json:"deployment_id,omitempty"`
	ProgressMsg  string          `json:"progress_msg,omitempty"`
	ProgressCur  float64         `json:"progress_cur,omitempty"`
	ProgressTot  float64         `json:"progress_tot,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        string          `json:"error,omitempty"`
	Outputs      []string        `json:"outputs,omitempty"` // server LLM's per-turn text
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

// runningJob tracks a job that has been submitted, with its cancel handle
// so a newer submission for the same deployment can supersede it.
type runningJob struct {
	job       *Job
	cancel    context.CancelFunc
	sessionID string
}

// JobManager owns job lifecycle: submission, progress, persistence, and
// per-deployment supersession.
type JobManager struct {
	mu      sync.Mutex // guards running, and serializes job file writes
	jobsDir string
	running map[string]*runningJob // deployment id -> latest submitted job
	sem     chan struct{}          // global concurrency limit
}

// NewJobManager creates a manager rooted at jobsDir (created if missing).
// Stale job files older than a day are cleaned up at startup, and any
// orphaned running jobs (left behind by a process crash mid-job) are marked
// failed so clients learn to retry instead of polling forever.
func NewJobManager(jobsDir string) *JobManager {
	_ = os.MkdirAll(jobsDir, 0o755)
	m := &JobManager{
		jobsDir: jobsDir,
		running: make(map[string]*runningJob),
		sem:     make(chan struct{}, maxConcurrentJobs),
	}
	// Fail orphaned running jobs BEFORE cleaning up stale files — a job
	// that crashed >24h ago would otherwise be deleted by cleanupStale
	// before failOrphanedRunning can mark it failed, and the client would
	// get a nil job (unable to distinguish "never existed" from "crashed").
	m.failOrphanedRunning()
	m.cleanupStale()
	return m
}

// failOrphanedRunning marks any persisted job with status=running as failed.
// A running job file on disk at startup means the previous process crashed
// (or was killed) mid-job — no worker will ever complete it. Failing it lets
// the client detect the interruption and retry, rather than long-polling a
// status that will never change.
func (m *JobManager) failOrphanedRunning() {
	entries, err := os.ReadDir(m.jobsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "job-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		job, err := m.load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil || job == nil {
			continue
		}
		if job.Status == JobRunning {
			job.Status = JobFailed
			job.Error = "orphaned by process restart"
			job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := m.saveLocked(job); err != nil {
				// The orphan stays status=running on disk; the client will
				// long-poll a job that never completes. Log so operators can
				// intervene (remove the stale file or fix the disk).
				slog.Error("failOrphanedRunning: persist failed — orphaned job stays running on disk", "job_id", job.ID, "err", err)
			}
		}
	}
}

// cleanupStale removes job files older than 24h — a client may poll a job
// for a while after it finishes, but past that the file is dead weight.
func (m *JobManager) cleanupStale() {
	entries, err := os.ReadDir(m.jobsDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "job-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(m.jobsDir, e.Name()))
		}
	}
}

// Submit registers a job and runs fn in a background goroutine.
// It returns the job id immediately. fn's context carries the progress
// callback (writing to the job file) and a 15-minute deadline; panics are
// recovered into a failed job.
//
// When deploymentID is non-empty, the submission is checked against any
// running job for that deployment:
//   - Same session (same sessionID): the prior job is superseded — its context
//     is cancelled so the client's retry acts on the latest request instead of
//     queueing behind a stale run.
//   - Different session: the submission is rejected with an error, so the
//     second client learns the deployment is busy instead of silently
//     cancelling the first client's work.
//
// The sessionID is the MCP session ID (empty for stdio, unique per HTTP
// connection). An empty deploymentID (propose_architecture, query_cloud) is
// stateless and bypasses this check entirely.
//
// The superseded job's goroutine may still be mid-saveDag/saveCost when
// cancelled — those writes are atomic (tmp+rename) and ctx-unaware, so they
// complete before the goroutine exits, preventing half-written files.
func (m *JobManager) Submit(ctx context.Context, deploymentID, sessionID, tool string, fn func(ctx context.Context) (string, error)) (string, error) {
	job := &Job{
		ID:           fmt.Sprintf("job-%d", time.Now().UnixNano()),
		Tool:         tool,
		Status:       JobRunning,
		DeploymentID: deploymentID,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := m.save(job); err != nil {
		return "", err
	}

	// The submit request's ctx is cancelled when the tool call returns
	// (which is immediate for async jobs) — the job runs on its own
	// context, bounded only by JobTimeout, and can be superseded by a
	// newer same-session submission for the same deployment.
	runCtx, cancel := context.WithTimeout(context.Background(), JobTimeout)

	// Register in the running map so done()/fail() can reclaim the cancel
	// handle (releasing the WithTimeout timer). For non-empty deploymentID,
	// this also enforces supersede/reject semantics. For empty deploymentID
	// (propose_architecture, query_cloud), the entry is purely for timer
	// cleanup — there is no deployment state to conflict over.
	m.mu.Lock()
	if deploymentID != "" {
		if prev, ok := m.running[deploymentID]; ok {
			if prev.sessionID == sessionID {
				// Same client retrying: supersede the prior job.
				prev.cancel()
				m.cancelLocked(prev.job)
			} else {
				// Different client: reject to avoid silent cancellation.
				m.mu.Unlock()
				cancel()
				return "", fmt.Errorf("deployment %q is busy — another session is operating on it; wait for that operation to finish or use a different deployment", deploymentID)
			}
		}
	}
	m.running[deploymentID] = &runningJob{job: job, cancel: cancel, sessionID: sessionID}
	m.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				m.fail(job, fmt.Sprintf("job panicked: %v", r))
			}
		}()
		// Global concurrency limit — wait for a slot (or supersession).
		select {
		case m.sem <- struct{}{}:
		case <-runCtx.Done():
			m.fail(job, "cancelled before start")
			return
		}
		defer func() { <-m.sem }()

		// Progress + model outputs write to the job file so get_job_result
		// reports live intermediate output.
		runCtx = hwcloud.WithProgress(runCtx, func(msg string, cur, tot float64) {
			m.progress(job, msg, cur, tot)
		})
		runCtx = WithJobOutputs(runCtx, func(content string) {
			m.appendOutput(job, content)
		})

		result, err := fn(runCtx)
		if err != nil {
			m.fail(job, err.Error())
			return
		}
		m.done(job, result)
	}()
	return job.ID, nil
}

// Get returns the job's current state, or nil if it does not exist.
// When wait > 0, it long-polls: blocks until the job finishes or the wait
// elapses, then returns the latest state (clients poll less often).
//
// While polling, the job's latest progress (message/cur/tot written by the
// running job goroutine) is forwarded to the caller via the ProgressFunc in
// ctx. This lets MCP clients render live progress during get_job_result's
// wait instead of staring at a blocked call — the async submit tool already
// returned, so this polling request is the only live MCP request the client
// associates with the work.
func (m *JobManager) Get(ctx context.Context, id string, wait time.Duration) (*Job, error) {
	progress := hwcloud.ProgressFromContext(ctx)
	deadline := time.Now().Add(wait)
	var lastMsg string
	for {
		job, err := m.load(id)
		if err != nil {
			return nil, err
		}
		if job == nil {
			return nil, nil
		}
		// Forward progress to the client whenever the job's progress message
		// changes (avoids spamming the same notification every 500ms).
		if progress != nil && job.Status == JobRunning && job.ProgressMsg != "" && job.ProgressMsg != lastMsg {
			lastMsg = job.ProgressMsg
			progress(job.ProgressMsg, job.ProgressCur, job.ProgressTot)
		}
		if job.Status != JobRunning || wait <= 0 || time.Now().After(deadline) {
			return job, nil
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return job, nil
		}
	}
}

// cancelLocked marks a superseded job cancelled and persists it. Caller
// holds m.mu. A job that already reached a terminal state (done/failed) is
// NOT overwritten — its real outcome must survive for clients polling the
// old job_id. This is the symmetric guard to the JobCancelled checks in
// done()/fail().
func (m *JobManager) cancelLocked(job *Job) {
	if job.Status == JobDone || job.Status == JobFailed {
		return
	}
	job.Status = JobCancelled
	job.Error = "superseded by a newer submission for this deployment"
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := m.saveLocked(job); err != nil {
		// The in-memory status is cancelled but the on-disk file may still
		// show "running" — a client polling the old job_id would long-poll
		// until restart (failOrphanedRunning). Log so operators can diagnose.
		slog.Warn("job cancel: persist failed — client may see stale running status", "job_id", job.ID, "err", err)
	}
}

func (m *JobManager) save(job *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked(job)
}

// saveLocked writes the job file atomically (write to temp + rename) so a
// concurrent load never sees a half-written file. Caller holds m.mu, which
// serializes all saveLocked calls — no two saveLocked calls run concurrently.
func (m *JobManager) saveLocked(job *Job) error {
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}
	path := filepath.Join(m.jobsDir, job.ID+".json")
	if err := atomicWrite(path, data); err != nil {
		return fmt.Errorf("write job (jobs dir %s — check it exists and is writable): %w", m.jobsDir, err)
	}
	return nil
}

func (m *JobManager) load(id string) (*Job, error) {
	if id == "" || filepath.Base(id) != id {
		return nil, fmt.Errorf("invalid job id %q", id)
	}
	data, err := os.ReadFile(filepath.Join(m.jobsDir, id+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read job: %w", err)
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("parse job: %w", err)
	}
	return &job, nil
}

// jobOutputKey is the ctx value carrying a job's output sink.
type jobOutputKey struct{}

// WithJobOutputs returns a ctx whose model outputs are appended to the
// given job's log. The runtime's observer reads this via
// JobOutputsFromContext; absence is fine (synchronous calls).
func WithJobOutputs(ctx context.Context, sink func(string)) context.Context {
	return context.WithValue(ctx, jobOutputKey{}, sink)
}

// JobOutputsFromContext returns the job output sink from ctx, or nil.
func JobOutputsFromContext(ctx context.Context) func(string) {
	f, _ := ctx.Value(jobOutputKey{}).(func(string))
	return f
}

// appendOutput adds the server LLM's per-turn text to the job's output
// log (truncated, bounded).
func (m *JobManager) appendOutput(job *Job, content string) {
	m.mu.Lock()
	job.Outputs = append(job.Outputs, truncate(content, outputEntryMax))
	if len(job.Outputs) > outputMaxCount {
		job.Outputs = job.Outputs[len(job.Outputs)-outputMaxCount:]
	}
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	m.mu.Unlock()
	if err := m.save(job); err != nil {
		slog.Warn("job appendOutput: persist failed", "job_id", job.ID, "err", err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut at a rune boundary.
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "..."
}

func (m *JobManager) progress(job *Job, msg string, cur, tot float64) {
	m.mu.Lock()
	job.ProgressMsg = msg
	job.ProgressCur = cur
	job.ProgressTot = tot
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	m.mu.Unlock()
	if err := m.save(job); err != nil {
		slog.Warn("job progress: persist failed", "job_id", job.ID, "err", err)
	}
}

func (m *JobManager) done(job *Job, result string) {
	m.mu.Lock()
	if job.Status == JobCancelled {
		// Superseded jobs keep their "cancelled" status — a late nil-error
		// return from the fn must not overwrite the supersession marker
		// with "done" + a stale result the client would act on.
		m.mu.Unlock()
		return
	}
	job.Status = JobDone
	job.Result = json.RawMessage(result)
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	// Reclaim the running entry: a terminal job will never be superseded
	// again, and calling cancel releases the context.WithTimeout timer.
	// Without this, m.running grows unbounded over the server lifetime
	// (one entry per distinct deploymentID that was ever submitted).
	if entry, ok := m.running[job.DeploymentID]; ok && entry.job == job {
		entry.cancel()
		delete(m.running, job.DeploymentID)
	}
	m.mu.Unlock()
	if err := m.save(job); err != nil {
		// The job is done in memory but the on-disk file may still show
		// "running" — Get reads from disk, so the client long-polling
		// get_job_result would block until restart (failOrphanedRunning).
		// Log so operators can diagnose; we cannot return the error here
		// because the fn already succeeded and the result is in memory.
		slog.Error("job done: persist failed — client may see stale running status", "job_id", job.ID, "err", err)
	}
}

func (m *JobManager) fail(job *Job, errMsg string) {
	m.mu.Lock()
	if job.Status == JobCancelled {
		// Superseded jobs keep their "cancelled" status — the fn's
		// context-cancelled error must not overwrite the supersession
		// marker.
		m.mu.Unlock()
		return
	}
	job.Status = JobFailed
	job.Error = errMsg
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	// Same reclamation as done().
	if entry, ok := m.running[job.DeploymentID]; ok && entry.job == job {
		entry.cancel()
		delete(m.running, job.DeploymentID)
	}
	m.mu.Unlock()
	if err := m.save(job); err != nil {
		slog.Error("job fail: persist failed — client may see stale running status", "job_id", job.ID, "err", err)
	}
}
