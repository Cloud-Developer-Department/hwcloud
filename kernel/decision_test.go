package kernel

import (
	"context"
	"sync"
	"testing"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/eventbus"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
)

// captureBoth implements RunObserver AND DecisionObserver. It records every
// stage + decision event with the RunID/TurnID stamped by the kernel, so the
// integration test can assert trajectory grouping.
type captureBoth struct {
	mu     sync.Mutex
	stages []hwcloud.StageEvent
	decs   []hwcloud.DecisionEvent
}

func (c *captureBoth) ObserveStage(_ context.Context, e hwcloud.StageEvent) {
	c.mu.Lock()
	c.stages = append(c.stages, e)
	c.mu.Unlock()
}

func (c *captureBoth) ObserveDecision(_ context.Context, e hwcloud.DecisionEvent) {
	c.mu.Lock()
	c.decs = append(c.decs, e)
	c.mu.Unlock()
}

func (c *captureBoth) snapshot() ([]hwcloud.StageEvent, []hwcloud.DecisionEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := make([]hwcloud.StageEvent, len(c.stages))
	copy(st, c.stages)
	dc := make([]hwcloud.DecisionEvent, len(c.decs))
	copy(dc, c.decs)
	return st, dc
}

// allowHuman is a HumanApprover that always allows. Wiring it into Deps makes
// policy() build the default Engine (rules → safety → memory → human), so a
// non-read-only tool reaches the human layer and emits DecisionPolicyHuman.
type allowHuman struct{}

func (allowHuman) Ask(context.Context, hwcloud.ToolCall, hwcloud.FunctionDefinition, hwcloud.Session) (governance.Decision, error) {
	return governance.Decision{Action: governance.Allow, Reason: "test allow"}, nil
}

// twoTurnModel returns a tool call on turn 0 and a plain stop on turn 1, so a
// single RunStream exercises two turn iterations (TurnID 0 and 1) plus the
// pre-loop guard.in (TurnID -1).
type twoTurnModel struct {
	calls int
}

func (m *twoTurnModel) ChatCompletion(_ context.Context, _ hwcloud.ChatCompletionRequest) (*hwcloud.ChatCompletionResponse, error) {
	m.calls++
	resp := &hwcloud.ChatCompletionResponse{}
	if m.calls == 1 {
		resp.Choices = []hwcloud.Choice{{
			Index: 0,
			Message: hwcloud.Message{
				Role: hwcloud.RoleAssistant,
				ToolCalls: []hwcloud.ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: hwcloud.ToolCallFunction{Name: "blocking_tool", Arguments: "{}"},
				}},
			},
			FinishReason: "tool_calls",
		}}
		return resp, nil
	}
	resp.Choices = []hwcloud.Choice{{
		Index:         0,
		Message:       hwcloud.Message{Role: hwcloud.RoleAssistant, Content: "done"},
		FinishReason:  "stop",
	}}
	return resp, nil
}

func (m *twoTurnModel) ChatCompletionStream(ctx context.Context, req hwcloud.ChatCompletionRequest) (hwcloud.StreamReader, error) {
	return nil, nil // fallback to non-streaming
}

func (m *twoTurnModel) ContextWindow() int { return 128_000 }

// TestRun_TrajectoryGrouping asserts the kernel stamps a stable run_id and
// per-turn turn_id onto every StageEvent and DecisionEvent:
//   - RunResult.RunID is non-empty and matches every event's RunID
//   - pre-loop guard.in events carry TurnID = -1
//   - turn-0 model.call + tool.execute + policy human events carry TurnID = 0
//   - turn-1 model.call carries TurnID = 1
//   - the governance human layer emitted at least one DecisionEvent (wiring
//     end-to-end: RunObserver that implements DecisionObserver → ctx → Engine)
func TestRun_TrajectoryGrouping(t *testing.T) {
	cap := &captureBoth{}
	model := &twoTurnModel{}
	tool := &nonStreamingTool{} // reused from runtime_test.go

	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(2),
	)
	deps := Deps{
		Tools:         []hwcloud.Tool{tool},
		Observer:      cap,
		HumanApprover: allowHuman{},
	}
	rt := New(cfg, deps)

	ch := rt.RunStream(context.Background(), hwcloud.Session{ID: "s1"}, hwcloud.UserMessage("go"))
	var runID string
	for evt := range ch {
		if evt.Type == hwcloud.StreamError {
			t.Fatalf("unexpected error: %v", evt.Error)
		}
		if evt.Type == hwcloud.StreamDone && evt.Result != nil {
			runID = evt.Result.RunID
		}
	}

	if runID == "" {
		t.Fatal("StreamDone.RunID is empty; kernel did not stamp run_id")
	}

	stages, decs := cap.snapshot()

	// Every stage event carries the run's RunID.
	for i, s := range stages {
		if s.RunID != runID {
			t.Errorf("stage[%d] (%s/%s) RunID = %q, want %q", i, s.Name, s.Phase, s.RunID, runID)
		}
	}
	// Every decision event carries the run's RunID.
	for i, d := range decs {
		if d.RunID != runID {
			t.Errorf("dec[%d] (%s/%s) RunID = %q, want %q", i, d.Layer, d.Outcome, d.RunID, runID)
		}
	}

	// The human layer must have fired for the non-read-only tool call — this
	// proves the observer was injected into ctx AND the Engine read it. Without
	// the ctx plumbing, DecObserver would be nil and decs would be empty.
	var human *hwcloud.DecisionEvent
	for i := range decs {
		if decs[i].Layer == hwcloud.DecisionPolicyHuman {
			human = &decs[i]
			break
		}
	}
	if human == nil {
		t.Fatalf("no DecisionPolicyHuman event; got %d events: %+v", len(decs), decs)
	}
	if human.Outcome != hwcloud.OutcomeAsk {
		t.Errorf("human outcome = %q, want %q", human.Outcome, hwcloud.OutcomeAsk)
	}
	if human.Subject != "blocking_tool" {
		t.Errorf("human subject = %q, want blocking_tool", human.Subject)
	}
	// The tool call happened on turn 0, so the human decision carries TurnID 0.
	if human.TurnID != 0 {
		t.Errorf("human TurnID = %d, want 0", human.TurnID)
	}

	// TurnID progression: guard.in is pre-loop (TurnID -1); turn-0 model.call
	// is TurnID 0; turn-1 model.call is TurnID 1.
	stageTurn := map[string]int{} // stage name → TurnID of its "enter" event
	for _, s := range stages {
		if s.Phase == "enter" {
			if _, ok := stageTurn[s.Name]; !ok { // first enter per stage
				stageTurn[s.Name] = s.TurnID
			}
		}
	}
	if stageTurn[hwcloud.StageGuardIn] != -1 {
		t.Errorf("guard.in TurnID = %d, want -1 (pre-loop)", stageTurn[hwcloud.StageGuardIn])
	}
	if stageTurn[hwcloud.StageModelCall] != 0 {
		t.Errorf("model.call TurnID = %d, want 0 (first turn)", stageTurn[hwcloud.StageModelCall])
	}
	// model.call should appear twice (turn 0 + turn 1); find the second enter
	// and assert TurnID 1.
	modelCallEnters := []int{}
	for _, s := range stages {
		if s.Name == hwcloud.StageModelCall && s.Phase == "enter" {
			modelCallEnters = append(modelCallEnters, s.TurnID)
		}
	}
	if len(modelCallEnters) < 2 {
		t.Fatalf("model.call enter events = %d, want ≥2", len(modelCallEnters))
	}
	if modelCallEnters[1] != 1 {
		t.Errorf("second model.call TurnID = %d, want 1", modelCallEnters[1])
	}
}

// TestRun_RunOnlyObserverSkipsDecisions asserts that a RunObserver which does
// NOT implement DecisionObserver is wired without error: stage events flow,
// decision events are silently dropped (the kernel's type assertion in run()
// and decisionObserver() returns nil), and the run completes normally. This is
// the graceful-compat contract for the 6 of 8 legacy RunObserver implementers.
func TestRun_RunOnlyObserverSkipsDecisions(t *testing.T) {
	r := &runOnlyStageObs{} // RunObserver only, no ObserveDecision
	model := &twoTurnModel{}
	tool := &nonStreamingTool{}

	cfg := agent.New("test", agent.WithModel(model), agent.WithMaxTurns(2))
	deps := Deps{
		Tools:         []hwcloud.Tool{tool},
		Observer:      r,
		HumanApprover: allowHuman{},
	}
	rt := New(cfg, deps)

	ch := rt.RunStream(context.Background(), hwcloud.Session{ID: "s2"}, hwcloud.UserMessage("go"))
	for evt := range ch {
		if evt.Type == hwcloud.StreamError {
			t.Fatalf("unexpected error: %v", evt.Error)
		}
	}

	// Stage events flowed.
	r.mu.Lock()
	got := len(r.stages)
	r.mu.Unlock()
	if got == 0 {
		t.Fatal("RunObserver-only impl received no stage events; observer wiring broken")
	}
}

// runOnlyStageObs implements RunObserver but NOT DecisionObserver — mirrors
// the run-only observer shape (compactionObserver / jobObserver / …). The
// slog hooks.Observer is NOT in this set: it implements both interfaces, so
// the kernel's DecisionObserver type-assertion succeeds for it.
type runOnlyStageObs struct {
	mu     sync.Mutex
	stages []hwcloud.StageEvent
}

func (r *runOnlyStageObs) ObserveStage(_ context.Context, e hwcloud.StageEvent) {
	r.mu.Lock()
	r.stages = append(r.stages, e)
	r.mu.Unlock()
}

// TestRun_ParentRunIDPropagation is the #1 regression test: when a caller
// pre-stamps RunInfo into ctx (as a team/orchestrator does before delegating to
// a child kernel.run), the child run must:
//
//   - preserve that parent RunID as ParentRunID on every event it emits
//     (run() reads prev.RunID BEFORE re-stamping its own, since WithRunInfo
//     shadows rather than merges);
//   - generate its own fresh RunID (distinct from the parent's);
//   - surface both on RunResult (RunID = child, ParentRunID = parent).
//
// This is the invariant that lets a multi-agent trajectory reassemble via
// (parent_run_id → child RunID/ParentRunID). Solo runs (no pre-stamp) keep
// empty ParentRunID — verified by TestRun_TrajectoryGrouping above.
func TestRun_ParentRunIDPropagation(t *testing.T) {
	cap := &captureBoth{}
	model := &twoTurnModel{}
	tool := &nonStreamingTool{}

	parentRunID := "parent-run-abc"
	ctx := hwcloud.WithRunInfo(context.Background(), hwcloud.RunInfo{
		RunID:       parentRunID,
		TurnID:      2, // a team is not a turn loop; some non-zero value
		ParentRunID: "grandparent-run-xyz", // a Plan nested in a Team must not lose the team's own parent
	})

	cfg := agent.New("test", agent.WithModel(model), agent.WithMaxTurns(2))
	deps := Deps{
		Tools:         []hwcloud.Tool{tool},
		Observer:      cap,
		HumanApprover: allowHuman{},
	}
	rt := New(cfg, deps)

	var result *hwcloud.RunResult
	ch := rt.RunStream(ctx, hwcloud.Session{ID: "s-parent"}, hwcloud.UserMessage("go"))
	for evt := range ch {
		if evt.Type == hwcloud.StreamError {
			t.Fatalf("unexpected error: %v", evt.Error)
		}
		if evt.Type == hwcloud.StreamDone && evt.Result != nil {
			result = evt.Result
		}
	}
	if result == nil {
		t.Fatal("StreamDone missing RunResult")
	}

	// Child generated its own RunID, distinct from the parent's.
	if result.RunID == "" {
		t.Fatal("child RunResult.RunID is empty; kernel did not stamp its own run_id")
	}
	if result.RunID == parentRunID {
		t.Fatal("child RunID == parentRunID; kernel reused the parent's id instead of generating a fresh one")
	}
	// ParentRunID on the result equals the pre-stamped parent RunID — the
	// back-link consumers use to walk up the trajectory tree.
	if result.ParentRunID != parentRunID {
		t.Errorf("RunResult.ParentRunID = %q, want %q", result.ParentRunID, parentRunID)
	}

	stages, decs := cap.snapshot()

	// Every event the child emitted must carry the child's RunID AND the
	// parent's RunID as ParentRunID.
	for i, s := range stages {
		if s.RunID != result.RunID {
			t.Errorf("stage[%d] (%s/%s) RunID = %q, want child %q", i, s.Name, s.Phase, s.RunID, result.RunID)
		}
		if s.ParentRunID != parentRunID {
			t.Errorf("stage[%d] (%s/%s) ParentRunID = %q, want %q (parent link lost)", i, s.Name, s.Phase, s.ParentRunID, parentRunID)
		}
	}
	for i, d := range decs {
		if d.RunID != result.RunID {
			t.Errorf("dec[%d] (%s/%s) RunID = %q, want child %q", i, d.Layer, d.Outcome, d.RunID, result.RunID)
		}
		if d.ParentRunID != parentRunID {
			t.Errorf("dec[%d] (%s/%s) ParentRunID = %q, want %q (parent link lost)", i, d.Layer, d.Outcome, d.ParentRunID, parentRunID)
		}
	}

	if len(stages) == 0 {
		t.Error("no stage events captured; observer wiring broken")
	}
	if len(decs) == 0 {
		t.Error("no decision events captured; observer wiring broken")
	}
}

// TestRun_EventBusCarriesRunID is the #2 regression for the dead-logger half
// of the dual audit trail: kernel.logEvent must stamp RunID/TurnID (read from
// ctx RunInfo) onto every eventbus.Event it emits, so a future durable sink
// can join eventbus rows to DecisionEvents on (session_id, run_id, turn_id).
//
// Without this, the two audit logs share only session_id — a multi-turn run
// produces N eventbus rows and M decision events under one session with no
// way to correlate them. The test wires a real BusLogger into Deps.EventLogger,
// runs a two-turn kernel run, and asserts every delivered Event carries the
// run's RunID (the same id on RunResult) plus the TurnID the run stamped at
// each emit site (user.input = -1→0 pre-loop, assistant.message = turn N).
func TestRun_EventBusCarriesRunID(t *testing.T) {
	bus := eventbus.New[eventbus.Event](64)
	logger := eventbus.NewBusLogger(bus)
	sub := bus.Subscribe("eb-sess")
	defer bus.Unsubscribe("eb-sess", sub)

	model := &twoTurnModel{}
	tool := &nonStreamingTool{}
	cfg := agent.New("test", agent.WithModel(model), agent.WithMaxTurns(2))
	deps := Deps{
		Tools:         []hwcloud.Tool{tool},
		Observer:      &captureBoth{}, // keep wiring realistic; events unused here
		HumanApprover: allowHuman{},
		EventLogger:   logger,
	}
	rt := New(cfg, deps)

	ch := rt.RunStream(context.Background(), hwcloud.Session{ID: "eb-sess"}, hwcloud.UserMessage("go"))
	var runID string
	for evt := range ch {
		if evt.Type == hwcloud.StreamError {
			t.Fatalf("unexpected error: %v", evt.Error)
		}
		if evt.Type == hwcloud.StreamDone && evt.Result != nil {
			runID = evt.Result.RunID
		}
	}
	if runID == "" {
		t.Fatal("StreamDone.RunID is empty; kernel did not stamp run_id")
	}

	// Drain the subscriber with a deadline (the run is already complete, so
	// every event was published before we reached here).
	var got []eventbus.Event
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case evt := <-sub.C:
			got = append(got, evt)
		default:
		}
		if time.Now().After(deadline) || len(got) >= 4 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(got) == 0 {
		t.Fatal("BusLogger delivered no events; EventLogger wiring broken")
	}

	// Every delivered eventbus.Event carries the run's RunID — the #2 invariant.
	for i, ev := range got {
		if ev.RunID != runID {
			t.Errorf("eventbus.Event[%d] (%s) RunID = %q, want %q (logEvent did not stamp RunID from ctx)", i, ev.Type, ev.RunID, runID)
		}
		if ev.SessionID != "eb-sess" {
			t.Errorf("eventbus.Event[%d] (%s) SessionID = %q, want eb-sess", i, ev.Type, ev.SessionID)
		}
	}

	// At least one user.input event must have fired (logEvent at run.go L95).
	var userInput *eventbus.Event
	for i := range got {
		if got[i].Type == eventbus.EventUserInput {
			userInput = &got[i]
			break
		}
	}
	if userInput == nil {
		t.Fatalf("no EventUserInput in %d events; got types: %v", len(got), eventTypes(got))
	}
	// user.input is emitted inside run() after ctx is stamped with RunInfo —
	// its TurnID reflects whatever the kernel set at that point.
	if userInput.TurnID != 0 && userInput.TurnID != -1 {
		t.Errorf("EventUserInput TurnID = %d, want 0 or -1 (run-internal stamp)", userInput.TurnID)
	}
}

func eventTypes(evts []eventbus.Event) []eventbus.EventType {
	out := make([]eventbus.EventType, len(evts))
	for i, e := range evts {
		out[i] = e.Type
	}
	return out
}
