package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/plugin/wasmhost"
	"github.com/Cloud-Developer-Department/hwcloud/scheduler"
)

// Manager discovers and manages WASM plugins from a directory.
type Manager struct {
	dir string

	mu        sync.Mutex
	ldr       loader
	tools     []hwcloud.Tool
	observers []*wasmObserver

	hostAPI   *wasmhost.HostAPI
	scheduler *scheduler.Scheduler

	// scheduled tracks the job ids each plugin registered, so Close can
	// unregister them all. Guarded by mu.
	scheduled map[string][]string

	onAbort func(reason string)

	// onStageProcessed/onDecisionProcessed are test seams for observeLoop.
	// nil in production (zero overhead); when set by a test they fire
	// synchronously in the worker right after dispatching an event, letting a
	// test deterministically assert dispatch ORDER without loading a .wasm
	// binary (the real observer default returns empty Action and logs nothing,
	// so slog capture cannot reveal when a decision is processed). Mirrors the
	// onAbort callback-field pattern.
	onStageProcessed    func(hwcloud.StageEvent)
	onDecisionProcessed func(hwcloud.DecisionEvent)

	// stageCh / decisionCh feed the single background observer worker
	// (Observer lazily creates them; never closed — a close racing a send
	// would panic, and the worker lives for the manager's lifetime, draining
	// on process exit; Close only detaches both so no new events are
	// accepted). 两个 stream 各自 FIFO;单 worker 串行化 wazero 调用 + 隔离
	// plugin panic。decision 优先排空防止低频 decision 事件被高频 stage 事件
	// 饥饿。cross-stream 顺序不保证(也无消费者需要——stage 与 decision 路由
	// 到不同 export)。
	stageCh    chan hwcloud.StageEvent
	decisionCh chan hwcloud.DecisionEvent
}

// NewManager creates a Manager for the given plugin directory.
func NewManager(dir string) *Manager {
	return &Manager{dir: dir, scheduled: make(map[string][]string)}
}

// WithHostAPI configures the host exports (keyring_get/set, http_request,
// log_info/warn/error) that WASM plugins can import via the "host" module.
// Call before [Manager.Discover].
func (m *Manager) WithHostAPI(h *wasmhost.HostAPI) *Manager {
	m.hostAPI = h
	return m
}

// WithScheduler registers the cron jobs that plugins declare in their
// metadata with this scheduler at Discover time, and unregisters them at
// Close. nil (default) = scheduled jobs are ignored (the export is not
// called). Call before [Manager.Discover].
func (m *Manager) WithScheduler(s *scheduler.Scheduler) *Manager {
	m.scheduler = s
	return m
}

// OnAbort registers a callback invoked when a stage plugin returns action=abort.
func (m *Manager) OnAbort(fn func(reason string)) {
	m.mu.Lock()
	m.onAbort = fn
	m.mu.Unlock()
}

// Discover scans the plugin directory for .wasm files, instantiates each one,
// reads its metadata, and registers it as a Tool or Stage plugin.
func (m *Manager) Discover(ctx context.Context) error {
	if m.dir == "" {
		return nil
	}

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("plugin dir: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Lazy-init wazero runtime.
	if m.ldr.runtime == nil {
		ldr, err := newLoader(ctx)
		if err != nil {
			return fmt.Errorf("init wazero: %w", err)
		}
		// Register host exports BEFORE loading any plugin module.
		if m.hostAPI != nil {
			if err := m.hostAPI.RegisterHostModule(ctx, ldr.runtime); err != nil {
				ldr.Close(ctx)
				return fmt.Errorf("register host module: %w", err)
			}
		}
		m.ldr = ldr
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".wasm" {
			continue
		}
		path := filepath.Join(m.dir, entry.Name())
		if err := m.loadOne(ctx, path); err != nil {
			// One broken plugin must not disable the rest: skip it and
			// keep discovering.
			slog.Error("wasm plugin load failed, skipping", "plugin", entry.Name(), "error", err)
		}
	}

	return nil
}

func (m *Manager) loadOne(ctx context.Context, path string) error {
	wasmBytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	mod, err := m.ldr.loadModule(ctx, filepath.Base(path), wasmBytes)
	if err != nil {
		return err
	}

	meta, err := mod.parseMeta(ctx)
	if err != nil {
		return err
	}

	switch meta.Type {
	case PluginTypeTools:
		m.tools = append(m.tools, &wasmTool{mod: mod, meta: meta})
	case PluginTypeObservers:
		m.observers = append(m.observers, &wasmObserver{mod: mod, meta: meta, name: meta.Name})
	default:
		slog.Debug("wasm skipping non-agent plugin type", "file", filepath.Base(path), "type", meta.Type)
		return nil
	}

	m.registerSchedules(meta, mod)

	return nil
}

// registerSchedules registers the plugin's declared cron jobs with the
// scheduler (if one is wired). Job ids are namespaced by plugin name —
// "name/jobid" — so two plugins can never clobber each other.
//
// CALLER HOLDS m.mu (Discover calls this inside its lock) — this must
// never take m.mu again: sync.Mutex is not re-entrant and a second lock
// here deadlocks Discover for any plugin that declares schedules.
func (m *Manager) registerSchedules(meta PluginMeta, mod *module) {
	if m.scheduler == nil || len(meta.Schedules) == 0 {
		return
	}
	// Job ids must be unique per plugin.
	seen := make(map[string]bool, len(meta.Schedules))
	for _, sc := range meta.Schedules {
		if sc.ID == "" || sc.Cron == "" {
			slog.Warn("wasm plugin declared an invalid schedule", "plugin", meta.Name, "schedule", sc)
			continue
		}
		if seen[sc.ID] {
			slog.Warn("wasm plugin declared duplicate schedule id", "plugin", meta.Name, "id", sc.ID)
			continue
		}
		seen[sc.ID] = true
		id := meta.Name + "/" + sc.ID
		if err := m.scheduler.Register(id, sc.Cron, func(ctx context.Context, at time.Time) {
			m.invokeScheduled(ctx, mod, sc.ID, at)
		}); err != nil {
			slog.Warn("wasm plugin schedule rejected", "plugin", meta.Name, "id", sc.ID, "cron", sc.Cron, "error", err)
			continue
		}
		slog.Info("registered wasm scheduled job", "plugin", meta.Name, "job", sc.ID, "cron", sc.Cron)
		// m.mu is already held by Discover — appending without re-locking.
		m.scheduled[meta.Name] = append(m.scheduled[meta.Name], id)
	}
}

// invokeScheduled fires one plugin job: call its run_scheduled export
// with {"id", "scheduled_at"}. The scheduler already bounds this with
// its own timeout context.
func (m *Manager) invokeScheduled(ctx context.Context, mod *module, jobID string, at time.Time) {
	payload, err := json.Marshal(map[string]any{
		"id":           jobID,
		"scheduled_at": at.Format(time.RFC3339),
	})
	if err != nil {
		slog.Error("wasm scheduled job marshal failed", "job", jobID, "error", err)
		return
	}
	out, err := mod.invoke(ctx, "run_scheduled", payload)
	if err != nil {
		slog.Error("wasm scheduled job failed", "job", jobID, "error", err)
		return
	}
	// The result is structured (ScheduledJobResult) — the error string
	// travels in the "error" field, so a failure is never logged as a
	// successful result.
	var r struct {
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		slog.Error("wasm scheduled job returned unparseable result", "job", jobID, "raw", string(out), "error", err)
		return
	}
	if r.Error != "" {
		slog.Error("wasm scheduled job failed", "job", jobID, "error", r.Error)
		return
	}
	if r.Result != "" {
		slog.Info("wasm scheduled job ran", "job", jobID, "result", r.Result)
	}
}

// Tools returns loaded Tool plugins as hwcloud.Tool values (a copy —
// callers must not mutate the manager's internal slice).
func (m *Manager) Tools() []hwcloud.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]hwcloud.Tool, len(m.tools))
	copy(out, m.tools)
	return out
}

// Observer returns a RunObserver that dispatches to matching WASM plugins.
// The returned router implements BOTH RunObserver and DecisionObserver:
// stage events route to plugins exporting "run", decision events to plugins
// exporting "observe_decision" (probed at load). A RunObserver-only consumer
// is unaffected — ObserveDecision is an optional interface.
func (m *Manager) Observer() hwcloud.RunObserver {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.observers) == 0 {
		return nil
	}
	if m.stageCh == nil {
		m.stageCh = make(chan hwcloud.StageEvent, 64)
		m.decisionCh = make(chan hwcloud.DecisionEvent, 64)
		go m.observeLoop()
	}
	return &observerRouter{mgr: m}
}

// Close releases the wazero runtime, unregisters all scheduled jobs, and
// detaches the observer queue (no new events accepted; the worker drains
// and exits with the process).
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scheduler != nil {
		for _, ids := range m.scheduled {
			for _, id := range ids {
				m.scheduler.Unregister(id)
			}
		}
		m.scheduled = make(map[string][]string)
	}
	m.stageCh = nil
	m.decisionCh = nil
	if m.ldr.runtime == nil {
		return nil
	}
	return m.ldr.Close(context.Background())
}

// observerRouter dispatches stage and decision events to matching WASM
// plugins on a background worker: a slow or broken plugin must not block
// the agent main loop. Events stay ordered (single worker); a panic in one
// plugin is contained and logged. An abort action stops dispatch for that
// stage event and fires the registered callback.
//
// Implements RunObserver (ObserveStage) AND DecisionObserver (ObserveDecision).
// The latter is an optional interface — old Go code holding the router as a
// RunObserver is unaffected; the kernel's type assertion opts into decision
// routing only when it asks for it.
type observerRouter struct {
	mgr *Manager
}

func (o *observerRouter) ObserveStage(_ context.Context, event hwcloud.StageEvent) {
	o.mgr.dispatchStage(event)
}

// ObserveDecision routes a decision event to plugins exporting
// "observe_decision". Old binaries (no export) are skipped inside
// runDecisionObservers — the event is never delivered to a plugin that
// cannot accept it.
func (o *observerRouter) ObserveDecision(_ context.Context, event hwcloud.DecisionEvent) {
	o.mgr.dispatchDecision(event)
}

// dispatchStage enqueues a stage event for the background worker. When the
// queue is full (a stuck observer), the event is dropped with a warning —
// observing must never stall the run.
func (m *Manager) dispatchStage(event hwcloud.StageEvent) {
	m.mu.Lock()
	ch := m.stageCh
	m.mu.Unlock()
	if ch == nil {
		return // manager closed
	}
	select {
	case ch <- event:
	default:
		slog.Warn("wasm observer queue full, dropping stage event", "stage", event.Name, "phase", event.Phase)
	}
}

// dispatchDecision enqueues a decision event for the same single worker as
// stage events. observeLoop priority-drains decisionCh before blocking on
// either channel, so a low-frequency decision event cannot be starved by a
// flood of high-frequency stage events. Same drop-on-full policy.
func (m *Manager) dispatchDecision(event hwcloud.DecisionEvent) {
	m.mu.Lock()
	ch := m.decisionCh
	m.mu.Unlock()
	if ch == nil {
		return // manager closed
	}
	select {
	case ch <- event:
	default:
		slog.Warn("wasm observer queue full, dropping decision event", "layer", event.Layer, "outcome", event.Outcome)
	}
}

// observeLoop is the single worker consuming stage and decision events. One
// worker keeps wazero calls serialized (module concurrency safety is unknown)
// and isolates a panicking plugin from corrupting another event's dispatch.
// decisionCh is drained with priority each iteration so low-frequency decision
// events are never starved by a flood of stage events; per-stream FIFO still
// holds within each channel. The loop ends when both channels are detached
// (Close nils them) and drained — the range-style exit below handles that.
func (m *Manager) observeLoop() {
	for {
		// Priority drain: process any pending decision first so a burst of
		// stage events cannot indefinitely defer a decision.
		select {
		case d, ok := <-m.decisionCh:
			if !ok {
				return
			}
			m.runDecisionObservers(d)
			if m.onDecisionProcessed != nil {
				m.onDecisionProcessed(d)
			}
			continue
		default:
		}
		// No decision pending — block for the next event on either stream.
		select {
		case d, ok := <-m.decisionCh:
			if !ok {
				return
			}
			m.runDecisionObservers(d)
			if m.onDecisionProcessed != nil {
				m.onDecisionProcessed(d)
			}
		case s, ok := <-m.stageCh:
			if !ok {
				return
			}
			m.runObservers(s)
			if m.onStageProcessed != nil {
				m.onStageProcessed(s)
			}
		}
	}
}

func (m *Manager) runObservers(event hwcloud.StageEvent) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("wasm observer panicked", "panic", rec)
		}
	}()
	m.mu.Lock()
	stages := m.observers
	onAbort := m.onAbort
	m.mu.Unlock()

	for _, s := range stages {
		if !s.matches(event) {
			continue
		}
		out, err := s.invoke(context.Background(), event)
		if err != nil {
			slog.Error("wasm observer error", "plugin", s.meta.Name, "stage", event.Name, "phase", event.Phase, "error", err)
			continue
		}
		if out != nil && out.Action == ActionAbort {
			slog.Info("wasm observer abort", "plugin", s.meta.Name, "stage", event.Name, "reason", out.Reason)
			if onAbort != nil {
				onAbort(out.Reason)
			}
			return // abort stops dispatch for this event
		}
		slog.Info("wasm observer", "plugin", s.meta.Name, "stage", event.Name, "phase", event.Phase, "action", out.Action)
	}
}

// runDecisionObservers fans a decision event out to every plugin that
// exported "observe_decision" at load time. Plugins compiled before the
// DecisionObserver ABI (hasDecision == false) are skipped — they cannot
// accept the event, so delivering it would be a wasted call into a missing
// export. There is no stage/phase filter (unlike runObservers): a decision
// is not tied to a stage, and a decision-aware plugin sees every decision.
// No abort path — decisions are observations, not control flow.
func (m *Manager) runDecisionObservers(event hwcloud.DecisionEvent) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("wasm decision observer panicked", "panic", rec)
		}
	}()
	m.mu.Lock()
	observers := m.observers
	m.mu.Unlock()

	for _, s := range observers {
		if !s.mod.hasDecision {
			continue // old binary — silently skip
		}
		out, err := s.invokeDecision(context.Background(), event)
		if err != nil {
			slog.Error("wasm decision observer error", "plugin", s.meta.Name, "layer", event.Layer, "outcome", event.Outcome, "error", err)
			continue
		}
		// Decision output is advisory (a plugin may surface or record the
		// event); there is no action that alters the run. Log only when the
		// plugin returned something non-empty, to avoid noise.
		if out != nil && out.Action != "" {
			slog.Info("wasm decision observer", "plugin", s.meta.Name, "layer", event.Layer, "action", out.Action)
		}
	}
}
