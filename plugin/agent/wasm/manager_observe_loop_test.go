package wasm

import (
	"sync"
	"testing"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// TestObserveLoop_DecisionPriorityOverStageFlood is the #6 regression test for
// the dual-channel priority select.
//
// observeLoop priority-drains decisionCh before blocking on either channel, so
// a low-frequency decision event cannot be starved by a flood of high-frequency
// stage events. We stage the exact starvation scenario the old single-union-queue
// design was vulnerable to: fill stageCh with N events, then enqueue ONE
// decision, then verify the decision is processed BEFORE any stage event.
//
// No .wasm binary is loaded — observeLoop's dispatch targets are nil on an
// empty Manager (runObservers/runDecisionObservers both no-op when
// m.observers is empty), and the test uses onStageProcessed/onDecisionProcessed
// test seams to observe dispatch order deterministically. This isolates the
// channel-routing invariant from plugin behavior.
func TestObserveLoop_DecisionPriorityOverStageFlood(t *testing.T) {
	const stageCount = 100

	mgr := &Manager{
		stageCh:    make(chan hwcloud.StageEvent, stageCount+1),
		decisionCh: make(chan hwcloud.DecisionEvent, 8),
	}

	var mu sync.Mutex
	var order []string // "decision" / "stage"

	// Decision seam appends "decision"; the very first append MUST be the
	// decision (priority drain runs before the blocking select ever reads
	// stageCh).
	mgr.onDecisionProcessed = func(hwcloud.DecisionEvent) {
		mu.Lock()
		order = append(order, "decision")
		mu.Unlock()
	}
	mgr.onStageProcessed = func(hwcloud.StageEvent) {
		mu.Lock()
		order = append(order, "stage")
		mu.Unlock()
		// Once we've seen enough stages to prove the decision went first, stop
		// the loop by detaching both channels (mirrors Close). Without this the
		// worker blocks forever on the next blocking select after the queue
		// drains.
		mu.Lock()
		if len(order) >= 5 { // 1 decision + 4 stages is conclusive
			mgr.stageCh = nil
			mgr.decisionCh = nil
		}
		mu.Unlock()
	}

	// Pre-fill stageCh with a flood BEFORE the worker starts, then enqueue the
	// single decision. The priority drain's first iteration must pick the
	// decision despite stageCh being full.
	for i := 0; i < stageCount; i++ {
		mgr.stageCh <- hwcloud.StageEvent{Name: hwcloud.StageToolExecute, Phase: "enter"}
	}
	mgr.decisionCh <- hwcloud.DecisionEvent{
		Layer:   hwcloud.DecisionPolicyHuman,
		Outcome: hwcloud.OutcomeAsk,
		Subject: "blocking_tool",
		RunID:   "run-1",
		TurnID:  0,
	}

	go mgr.observeLoop()

	// Wait for at least one decision + one stage to be recorded. The loop
	// self-terminates once order >= 5 (seam nils the channels → blocking select
	// reads nil channel blocks forever → no busy-loop; the goroutine leaks
	// harmlessly for the test's lifetime, same as a real Manager worker that
	// outlives Close via process exit).
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) == 0 {
		t.Fatal("observeLoop processed no events within timeout")
	}
	if order[0] != "decision" {
		t.Errorf("priority drain failed: first processed event = %q, want %q (full stageCh must not starve a queued decision)", order[0], "decision")
	}
}

// TestObserveLoop_PerStreamFIFO verifies per-stream ordering within stageCh is
// preserved: stages arrive at the worker in enqueue order. Decisions are not
// interleaved (decisionCh empty), so this exercises the blocking-select stage
// arm only.
func TestObserveLoop_PerStreamFIFO(t *testing.T) {
	const n = 8
	mgr := &Manager{
		stageCh:    make(chan hwcloud.StageEvent, n),
		decisionCh: make(chan hwcloud.DecisionEvent, 4),
	}

	var mu sync.Mutex
	var got []string
	mgr.onStageProcessed = func(ev hwcloud.StageEvent) {
		mu.Lock()
		got = append(got, ev.Detail["seq"].(string))
		if len(got) == n {
			mgr.stageCh = nil
			mgr.decisionCh = nil
		}
		mu.Unlock()
	}

	for i := 0; i < n; i++ {
		mgr.stageCh <- hwcloud.StageEvent{
			Name:   hwcloud.StageToolExecute,
			Phase:  "enter",
			Detail: map[string]any{"seq": string(rune('a' + i))},
		}
	}

	go mgr.observeLoop()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		l := len(got)
		mu.Unlock()
		if l == n || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != n {
		t.Fatalf("processed %d stages, want %d (got=%v)", len(got), n, got)
	}
	want := "abcdefgh"
	if s := joinStrings(got); s != want {
		t.Errorf("per-stream FIFO broken: got order %q, want %q", s, want)
	}
}

func joinStrings(ss []string) string {
	var b []byte
	for _, s := range ss {
		b = append(b, s...)
	}
	return string(b)
}

// TestObserveLoop_EmptyManagerNoPanic confirms observeLoop handles an empty
// Manager (nil observers) without panicking on both the stage and decision
// arms — runObservers/runDecisionObservers must be nil-safe. This is the
// no-binary contract the priority test relies on.
func TestObserveLoop_EmptyManagerNoPanic(t *testing.T) {
	mgr := &Manager{
		stageCh:    make(chan hwcloud.StageEvent, 2),
		decisionCh: make(chan hwcloud.DecisionEvent, 2),
	}
	done := make(chan struct{})
	mgr.onDecisionProcessed = func(hwcloud.DecisionEvent) {
		mgr.stageCh = nil
		mgr.decisionCh = nil
		close(done)
	}

	mgr.decisionCh <- hwcloud.DecisionEvent{Layer: "x", Outcome: "y"}

	go mgr.observeLoop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observeLoop did not process the decision on an empty Manager within timeout")
	}
}
