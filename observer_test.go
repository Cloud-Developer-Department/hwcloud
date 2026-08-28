package hwcloud

import (
	"context"
	"sync"
	"testing"
)

// captureObserver records every event it receives. It implements BOTH
// RunObserver and DecisionObserver — to prove a single type can opt into
// decision events, and to serve as the fan-out target in multiObserver tests.
type captureObserver struct {
	mu      sync.Mutex
	events  []DecisionEvent
	stageEv []StageEvent
}

func (c *captureObserver) ObserveDecision(_ context.Context, e DecisionEvent) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *captureObserver) ObserveStage(_ context.Context, e StageEvent) {
	c.mu.Lock()
	c.stageEv = append(c.stageEv, e)
	c.mu.Unlock()
}

func (c *captureObserver) decisions() []DecisionEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]DecisionEvent, len(c.events))
	copy(out, c.events)
	return out
}

// runOnlyObserver implements RunObserver but NOT DecisionObserver. It stands
// in for the existing run-only RunObserver implementers (compactionObserver,
// rest/handler.go stageObserver, examples' progressObserver/stageObserver,
// iac-server jobObserver, …) that never opted into decision events. The
// slog hooks.Observer is NOT in this set — it implements both interfaces.
type runOnlyObserver struct {
	stages []StageEvent
}

func (r *runOnlyObserver) ObserveStage(_ context.Context, e StageEvent) {
	r.stages = append(r.stages, e)
}

// asDecision asserts mo satisfies DecisionObserver and returns it, failing
// the test otherwise. multiObserver implements both interfaces, but
// MultiObserver's return type is RunObserver — callers opt into decision
// routing via this same type assertion (the kernel does it at run() entry).
func asDecision(t *testing.T, mo RunObserver) DecisionObserver {
	t.Helper()
	d, ok := mo.(DecisionObserver)
	if !ok {
		t.Fatalf("%T does not implement DecisionObserver", mo)
	}
	return d
}

// TestObserveDecision_NoOpForRunOnlyObserver: the helper must silently drop
// decision events when the observer implements RunObserver but not
// DecisionObserver. This is the graceful-compat contract for old Go
// implementers — they compile unchanged and skip decisions.
func TestObserveDecision_NoOpForRunOnlyObserver(t *testing.T) {
	r := &runOnlyObserver{}
	ObserveDecision(context.Background(), r, DecisionEvent{Layer: DecisionPolicyRule, Outcome: OutcomeAllow})
	if len(r.stages) != 0 {
		t.Fatalf("RunObserver-only impl received stage events; want none")
	}
}

// TestObserveDecision_NilSafe: a nil observer must not panic.
func TestObserveDecision_NilSafe(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("ObserveDecision(nil) panicked: %v", rec)
		}
	}()
	ObserveDecision(context.Background(), nil, DecisionEvent{Layer: DecisionPolicyRule})
}

// TestObserveDecision_StampsRunInfoFromContext: when the caller leaves RunID
// blank (the leaf-package case — they build the event from local data), the
// helper stamps RunID/TurnID from ctx. When the caller already set RunID, the
// helper must not overwrite it.
func TestObserveDecision_StampsRunInfoFromContext(t *testing.T) {
	c := &captureObserver{}
	ctx := WithRunInfo(context.Background(), RunInfo{RunID: "run-7", TurnID: 3})

	// Blank RunID → stamped from ctx.
	ObserveDecision(ctx, c, DecisionEvent{Layer: DecisionVectorRecall, Outcome: OutcomeHit})
	got := c.decisions()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].RunID != "run-7" || got[0].TurnID != 3 {
		t.Errorf("stamped RunID/TurnID = %q/%d, want run-7/3", got[0].RunID, got[0].TurnID)
	}

	// Pre-set RunID → preserved (not overwritten by ctx).
	c.events = nil
	ObserveDecision(ctx, c, DecisionEvent{Layer: DecisionVectorRecall, Outcome: OutcomeMiss, RunID: "caller-set", TurnID: 99})
	got = c.decisions()
	if got[0].RunID != "caller-set" || got[0].TurnID != 99 {
		t.Errorf("pre-set RunID/TurnID = %q/%d, want caller-set/99 (helper overwrote)", got[0].RunID, got[0].TurnID)
	}
}

// TestObserveDecision_StampsParentRunIDAndSessionFromContext: #2/#4 — the
// helper must auto-stamp ParentRunID (from ctx RunInfo) and SessionID (from
// ctx Session) when the caller leaves them blank, and preserve caller-set
// values when present. Leaf-package emit sites (provider/memory, kernel
// compress/prepare) build events from local data and rely on this fallback
// to complete the four-tuple join key without threading session/parent
// through every leaf signature.
func TestObserveDecision_StampsParentRunIDAndSessionFromContext(t *testing.T) {
	c := &captureObserver{}
	ctx := WithSession(WithRunInfo(context.Background(), RunInfo{
		RunID:       "leaf-run",
		TurnID:      4,
		ParentRunID: "parent-run-xyz",
	}), Session{ID: "sess-leaf"})

	// Blank ParentRunID + SessionID → stamped from ctx.
	ObserveDecision(ctx, c, DecisionEvent{Layer: DecisionVectorRecall, Outcome: OutcomeHit})
	got := c.decisions()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].ParentRunID != "parent-run-xyz" {
		t.Errorf("ParentRunID = %q, want parent-run-xyz (from ctx RunInfo)", got[0].ParentRunID)
	}
	if got[0].SessionID != "sess-leaf" {
		t.Errorf("SessionID = %q, want sess-leaf (from ctx Session)", got[0].SessionID)
	}

	// Pre-set ParentRunID + SessionID → preserved (not overwritten).
	c.events = nil
	ObserveDecision(ctx, c, DecisionEvent{
		Layer:       DecisionVectorRecall,
		Outcome:     OutcomeMiss,
		ParentRunID: "caller-parent",
		SessionID:   "caller-sess",
	})
	got = c.decisions()
	if got[0].ParentRunID != "caller-parent" {
		t.Errorf("pre-set ParentRunID = %q, want caller-parent (helper overwrote)", got[0].ParentRunID)
	}
	if got[0].SessionID != "caller-sess" {
		t.Errorf("pre-set SessionID = %q, want caller-sess (helper overwrote)", got[0].SessionID)
	}
}

// TestObserveDecision_NoSessionInCtxLeavesBlank: when ctx carries no Session
// (a leaf site reached via a path that didn't WithSession), the helper must
// leave SessionID blank rather than panic — the eventbus join key is then
// incomplete for that one event, which is acceptable (the direct emit sites
// in policy.go stamp SessionID explicitly). This guards the nil-session path.
func TestObserveDecision_NoSessionInCtxLeavesBlank(t *testing.T) {
	c := &captureObserver{}
	// RunInfo but NO WithSession.
	ctx := WithRunInfo(context.Background(), RunInfo{RunID: "r", TurnID: 0})
	ObserveDecision(ctx, c, DecisionEvent{Layer: DecisionVectorRecall, Outcome: OutcomeHit})
	got := c.decisions()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].SessionID != "" {
		t.Errorf("SessionID = %q, want empty (no session in ctx)", got[0].SessionID)
	}
	// RunID still stamped from ctx — RunInfo is independent of Session.
	if got[0].RunID != "r" {
		t.Errorf("RunID = %q, want r (from ctx RunInfo)", got[0].RunID)
	}
}

// TestMultiObserver_ImplementsDecisionObserver: the *multiObserver returned
// by MultiObserver must satisfy DecisionObserver even though MultiObserver's
// signature returns RunObserver. This is what lets the kernel wire a mixed
// observer set into DecisionObserverFromContext via one type assertion.
func TestMultiObserver_ImplementsDecisionObserver(t *testing.T) {
	mo := MultiObserver(&runOnlyObserver{}, &captureObserver{})
	if mo == nil {
		t.Fatal("MultiObserver returned nil for two observers")
	}
	if _, ok := mo.(DecisionObserver); !ok {
		t.Fatal("multiObserver does not implement DecisionObserver")
	}
}

// TestMultiObserver_DecisionFanOutSubset: a MultiObserver combining a
// RunObserver-only impl and a DecisionObserver impl must fan decision events
// ONLY to the DecisionObserver subset, while stage events go to both. This is
// the core graceful-compat behavior at the composition boundary.
func TestMultiObserver_DecisionFanOutSubset(t *testing.T) {
	r := &runOnlyObserver{}
	c := &captureObserver{}

	mo := MultiObserver(r, c)
	ctx := context.Background()

	mo.ObserveStage(ctx, StageEvent{Name: StageModelCall, Phase: "leave"})
	asDecision(t, mo).ObserveDecision(ctx, DecisionEvent{Layer: DecisionPolicyRule, Outcome: OutcomeAllow})

	// Stage event reached BOTH.
	if len(r.stages) != 1 || r.stages[0].Name != StageModelCall {
		t.Errorf("run-only stage events = %v, want 1 StageModelCall", r.stages)
	}
	c.mu.Lock()
	if len(c.stageEv) != 1 {
		t.Errorf("capture stage events = %d, want 1", len(c.stageEv))
	}
	c.mu.Unlock()

	// Decision event reached ONLY the capture observer (run-only skipped).
	if got := c.decisions(); len(got) != 1 || got[0].Layer != DecisionPolicyRule || got[0].Outcome != OutcomeAllow {
		t.Errorf("capture decision events = %+v, want 1 policy.rule/allow", got)
	}
}

// TestMultiObserver_NoDecisionObservers: when NONE of the children implement
// DecisionObserver, ObserveDecision is a no-op (no panic, no event). This is
// the all-old-implementers case.
func TestMultiObserver_NoDecisionObservers(t *testing.T) {
	mo := MultiObserver(&runOnlyObserver{}, &runOnlyObserver{})
	if mo == nil {
		t.Fatal("MultiObserver returned nil")
	}
	d := asDecision(t, mo)
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("ObserveDecision panicked with no DecisionObserver children: %v", rec)
		}
	}()
	d.ObserveDecision(context.Background(), DecisionEvent{Layer: DecisionPolicyRule})
}

// TestMultiObserver_AllDecisionObservers: when every child implements
// DecisionObserver, the event fans out to all of them.
func TestMultiObserver_AllDecisionObservers(t *testing.T) {
	a, b := &captureObserver{}, &captureObserver{}
	mo := MultiObserver(a, b)
	asDecision(t, mo).ObserveDecision(context.Background(), DecisionEvent{Layer: DecisionVectorRecall, Outcome: OutcomeHit})

	if len(a.decisions()) != 1 || len(b.decisions()) != 1 {
		t.Errorf("fan-out a=%d b=%d, want 1/1", len(a.decisions()), len(b.decisions()))
	}
}

// ── context round-trips ──

// TestWithDecisionObserver_RoundTrip: inject + extract preserves the exact
// observer (same pointer), and nil extract from an unprepared ctx returns nil.
func TestWithDecisionObserver_RoundTrip(t *testing.T) {
	if got := DecisionObserverFromContext(context.Background()); got != nil {
		t.Errorf("unprepared ctx returned %v, want nil", got)
	}

	c := &captureObserver{}
	ctx := WithDecisionObserver(context.Background(), c)
	if got := DecisionObserverFromContext(ctx); got != c {
		t.Errorf("round-trip returned %v, want same pointer", got)
	}
}

// TestWithDecisionObserver_NilNoOp: injecting nil must return the original ctx
// unchanged (no value set, no wrapper layer).
func TestWithDecisionObserver_NilNoOp(t *testing.T) {
	base := context.Background()
	ctx := WithDecisionObserver(base, nil)
	if ctx != base {
		t.Error("WithDecisionObserver(nil) wrapped ctx; want identity")
	}
	if got := DecisionObserverFromContext(ctx); got != nil {
		t.Errorf("nil-injected ctx returned %v, want nil", got)
	}
}

// TestWithRunInfo_RoundTrip: inject + extract preserves RunID/TurnID, and an
// unprepared ctx returns the zero value (RunID="", TurnID=0).
func TestWithRunInfo_RoundTrip(t *testing.T) {
	if got := RunInfoFromContext(context.Background()); got != (RunInfo{}) {
		t.Errorf("unprepared ctx RunInfo = %+v, want zero", got)
	}

	ctx := WithRunInfo(context.Background(), RunInfo{RunID: "run-9", TurnID: -1})
	got := RunInfoFromContext(ctx)
	if got.RunID != "run-9" || got.TurnID != -1 {
		t.Errorf("round-trip RunInfo = %+v, want {run-9 -1}", got)
	}
}

// TestWithRunInfo_UpdatedPerTurn: the kernel re-stamps ctx at the top of each
// turn iteration; the latest stamp wins (ctx values shadow, they don't
// accumulate).
func TestWithRunInfo_UpdatedPerTurn(t *testing.T) {
	ctx := WithRunInfo(context.Background(), RunInfo{RunID: "run-r", TurnID: -1})
	if got := RunInfoFromContext(ctx).TurnID; got != -1 {
		t.Errorf("pre-loop TurnID = %d, want -1", got)
	}
	ctx = WithRunInfo(ctx, RunInfo{RunID: "run-r", TurnID: 0})
	if got := RunInfoFromContext(ctx).TurnID; got != 0 {
		t.Errorf("turn 0 TurnID = %d, want 0", got)
	}
	ctx = WithRunInfo(ctx, RunInfo{RunID: "run-r", TurnID: 2})
	if got := RunInfoFromContext(ctx).TurnID; got != 2 {
		t.Errorf("turn 2 TurnID = %d, want 2", got)
	}
}
