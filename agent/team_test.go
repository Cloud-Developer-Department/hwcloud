package agent

import (
	"context"
	"sync"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// fakeModel is a minimal hwcloud.Model that returns a fixed response and never calls tools.
type fakeModel struct {
	resp *hwcloud.ChatCompletionResponse
}

func (m *fakeModel) ChatCompletion(ctx context.Context, req hwcloud.ChatCompletionRequest) (*hwcloud.ChatCompletionResponse, error) {
	return m.resp, nil
}

func (m *fakeModel) ChatCompletionStream(ctx context.Context, req hwcloud.ChatCompletionRequest) (hwcloud.StreamReader, error) {
	return nil, nil
}

func (m *fakeModel) ContextWindow() int { return 128_000 }

func fakeNoopModel() hwcloud.Model {
	return &fakeModel{
		resp: &hwcloud.ChatCompletionResponse{
			Choices: []hwcloud.Choice{{
				Message: hwcloud.Message{
					Role:    hwcloud.RoleAssistant,
					Content: "Done.",
				},
				FinishReason: "stop",
			}},
			Usage: hwcloud.Usage{},
		},
	}
}

// fakeRunner is a minimal AgentRunner test double.
type fakeRunner struct{}

func (f *fakeRunner) RunWithPrefix(ctx context.Context, session hwcloud.Session, prefix []hwcloud.Message, input hwcloud.Message) (*hwcloud.RunResult, error) {
	return &hwcloud.RunResult{FinalOutput: "done"}, nil
}

func (f *fakeRunner) RunStreamWithPrefix(ctx context.Context, session hwcloud.Session, prefix []hwcloud.Message, input hwcloud.Message) <-chan hwcloud.StreamEvent {
	ch := make(chan hwcloud.StreamEvent, 1)
	ch <- hwcloud.StreamEvent{Type: hwcloud.StreamDone, Result: &hwcloud.RunResult{FinalOutput: "done"}}
	close(ch)
	return ch
}

func TestAddAgent(t *testing.T) {
	t.Run("add to empty team", func(t *testing.T) {
		team := NewTeam()
		if err := team.AddAgent("a", "agent A", &fakeRunner{}); err != nil {
			t.Fatalf("AddAgent: %v", err)
		}
		if _, ok := team.agents["a"]; !ok {
			t.Fatal("agent not in map")
		}
		if len(team.order) != 1 || team.order[0] != "a" {
			t.Fatalf("order: %v", team.order)
		}
	})

	t.Run("duplicate name", func(t *testing.T) {
		team := NewTeam()
		if err := team.AddAgent("x", "first", &fakeRunner{}); err != nil {
			t.Fatalf("first AddAgent: %v", err)
		}
		if err := team.AddAgent("x", "second", &fakeRunner{}); err == nil {
			t.Fatal("expected error on duplicate name")
		}
	})

	t.Run("multiple agents", func(t *testing.T) {
		team := NewTeam()
		if err := team.AddAgent("a", "A", &fakeRunner{}); err != nil {
			t.Fatalf("AddAgent a: %v", err)
		}
		if err := team.AddAgent("b", "B", &fakeRunner{}); err != nil {
			t.Fatalf("AddAgent b: %v", err)
		}
		if len(team.agents) != 2 {
			t.Fatalf("expected 2 agents, got %d", len(team.agents))
		}
	})
}

func TestRemoveAgent(t *testing.T) {
	t.Run("remove existing", func(t *testing.T) {
		team := NewTeam()
		team.AddAgent("a", "A", &fakeRunner{})
		team.AddAgent("b", "B", &fakeRunner{})

		team.RemoveAgent("a")
		if _, ok := team.agents["a"]; ok {
			t.Fatal("agent a still in map")
		}
		if len(team.order) != 1 || team.order[0] != "b" {
			t.Fatalf("order: %v", team.order)
		}
	})

	t.Run("remove non-existing", func(t *testing.T) {
		team := NewTeam()
		team.AddAgent("a", "A", &fakeRunner{})
		team.RemoveAgent("nonexistent") // no-op, no panic
		if len(team.agents) != 1 {
			t.Fatal("agent count changed")
		}
	})

	t.Run("remove last agent", func(t *testing.T) {
		team := NewTeam()
		team.AddAgent("a", "A", &fakeRunner{})
		team.RemoveAgent("a")
		if len(team.agents) != 0 {
			t.Fatal("team not empty")
		}
		if len(team.order) != 0 {
			t.Fatal("order not empty")
		}
	})
}

// captureRunner is an AgentRunner double that records the context's RunInfo
// (so the test can verify the child saw the team's RunID as ParentRunID) and
// the session it received, then returns a RunResult carrying a distinct child
// RunID. It also implements RunStreamWithPrefix for the streaming team path.
type captureRunner struct {
	mu       sync.Mutex
	seenRI   []hwcloud.RunInfo
	seenSess []hwcloud.Session
	childRun string
}

func (c *captureRunner) RunWithPrefix(ctx context.Context, session hwcloud.Session, _ []hwcloud.Message, _ hwcloud.Message) (*hwcloud.RunResult, error) {
	c.mu.Lock()
	c.seenRI = append(c.seenRI, hwcloud.RunInfoFromContext(ctx))
	c.seenSess = append(c.seenSess, session)
	c.mu.Unlock()
	return &hwcloud.RunResult{RunID: c.childRun, FinalOutput: "child-done"}, nil
}

func (c *captureRunner) RunStreamWithPrefix(ctx context.Context, session hwcloud.Session, _ []hwcloud.Message, _ hwcloud.Message) <-chan hwcloud.StreamEvent {
	c.mu.Lock()
	c.seenRI = append(c.seenRI, hwcloud.RunInfoFromContext(ctx))
	c.seenSess = append(c.seenSess, session)
	c.mu.Unlock()
	ch := make(chan hwcloud.StreamEvent, 1)
	ch <- hwcloud.StreamEvent{Type: hwcloud.StreamDone, Result: &hwcloud.RunResult{RunID: c.childRun, FinalOutput: "child-done"}}
	close(ch)
	return ch
}

func (c *captureRunner) snapshot() ([]hwcloud.RunInfo, []hwcloud.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ri := make([]hwcloud.RunInfo, len(c.seenRI))
	copy(ri, c.seenRI)
	ss := make([]hwcloud.Session, len(c.seenSess))
	copy(ss, c.seenSess)
	return ri, ss
}

// captureStageObs records every StageEvent the team observer receives, with
// the RunID/ParentRunID stamped by teamRunner.observeStage.
type captureStageObs struct {
	mu     sync.Mutex
	stages []hwcloud.StageEvent
}

func (o *captureStageObs) ObserveStage(_ context.Context, e hwcloud.StageEvent) {
	o.mu.Lock()
	o.stages = append(o.stages, e)
	o.mu.Unlock()
}

func (o *captureStageObs) snapshot() []hwcloud.StageEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]hwcloud.StageEvent, len(o.stages))
	copy(out, o.stages)
	return out
}

// TestTeam_RunIDPropagation is the #1 regression test for the team layer.
// A team run must:
//
//   - generate a team-level RunID (TeamResult.RunID non-empty);
//   - stamp that RunID into ctx so the child agent's kernel.run() reads it
//     as ParentRunID (the child's RunInfoFromContext(ctx).RunID == team RunID);
//   - forward the team RunID on every TeamEvent;
//   - stamp the team RunID (as RunID, not ParentRunID — the team IS the run)
//     onto stage events emitted via teamRunner.observeStage.
//
// The child RunResult.RunID is distinct from the team RunID, and the ctx the
// child receives carries the team RunID — the link that lets a multi-agent
// trajectory reassemble. A single child is sufficient to prove the ctx
// stamping invariant; the handoff-to-second-agent path is exercised by the
// team's integration tests and adds no new propagation logic.
func TestTeam_RunIDPropagation(t *testing.T) {
	obs := &captureStageObs{}
	team := NewTeam(WithTeamObserver(obs))

	ca := &captureRunner{childRun: "child-a-run"}
	team.AddAgent("a", "agent A", ca)

	var teamRunID string
	var events []TeamEvent
	ch := team.RunStream(context.Background(), hwcloud.Session{ID: "team-sess"}, hwcloud.UserMessage("start"))
	for evt := range ch {
		events = append(events, evt)
		if evt.Type == TeamError {
			t.Fatalf("team error: %v", evt.Error)
		}
		if evt.Type == TeamDone && evt.Result != nil {
			teamRunID = evt.Result.RunID
		}
	}

	if teamRunID == "" {
		t.Fatal("TeamResult.RunID is empty; teamRunner.run did not generate a team-level run_id")
	}

	// Every TeamEvent carries the team RunID.
	for i, e := range events {
		if e.RunID != teamRunID {
			t.Errorf("TeamEvent[%d] (%s) RunID = %q, want team %q", i, e.Type, e.RunID, teamRunID)
		}
	}

	// The child saw the team RunID as its ctx's RunID — which kernel.run()
	// reads as ParentRunID (the team is the parent). We assert at the team
	// layer: the ctx handed to RunStreamWithPrefix carries the team RunID,
	// proving the team stamped ctx before delegating.
	ris, _ := ca.snapshot()
	if len(ris) == 0 {
		t.Fatal("agent a: captureRunner saw no RunInfo in ctx")
	}
	ri := ris[0]
	if ri.RunID != teamRunID {
		t.Errorf("agent a: ctx RunInfo.RunID = %q, want team %q (child kernel.run reads this as ParentRunID)", ri.RunID, teamRunID)
	}
	// The child's own RunID is distinct from the team's.
	if ca.childRun == teamRunID {
		t.Errorf("agent a: child RunID == team RunID (%q); not distinct", ca.childRun)
	}

	// Stage events the team itself emitted (via observeStage) carry the team
	// RunID as their RunID — the team is the run, so its own events are keyed
	// to it, not to a parent.
	stages := obs.snapshot()
	if len(stages) == 0 {
		t.Fatal("team observer received no stage events; observeStage not forwarding")
	}
	for i, s := range stages {
		if s.RunID != teamRunID {
			t.Errorf("team stage[%d] (%s/%s) RunID = %q, want team %q", i, s.Name, s.Phase, s.RunID, teamRunID)
		}
	}
}

func TestConcurrentAddRemove(t *testing.T) {
	// Verify AddAgent/RemoveAgent don't race with reads.
	team := NewTeam()
	team.AddAgent("a", "A", &fakeRunner{})

	var wg sync.WaitGroup

	// Writer goroutine: add and remove agents
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			name := string(rune('b' + i%5))
			team.AddAgent(name, "dynamic", &fakeRunner{})
			team.RemoveAgent(name)
		}
	}()

	// Reader goroutine: simulate agentInfos / prepareAgent pattern
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			team.mu.Lock()
			_ = len(team.agents)
			for _, name := range team.order {
				_ = team.agents[name]
			}
			team.mu.Unlock()
		}
	}()

	wg.Wait()
}
