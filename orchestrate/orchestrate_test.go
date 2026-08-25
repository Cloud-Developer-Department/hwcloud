package orchestrate

import (
	"context"
	"sync"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
)

// staticPlanner is a Planner test double that returns a fixed PlanDef — the
// executor only needs Plan() to produce the DAG; it never calls Replan when
// no step fails.
type staticPlanner struct {
	def *PlanDef
}

func (s *staticPlanner) Plan(context.Context, string, []agent.AgentInfo, []hwcloud.Message) (*PlanDef, error) {
	return s.def, nil
}

func (s *staticPlanner) Replan(context.Context, ReplanInput, func(string)) ([]StepDef, error) {
	return nil, nil
}

// capturePlanRunner is an AgentRunner double that records the ctx RunInfo the
// executor handed it (so the test can prove the child saw planRunID as the
// ctx RunID — which kernel.run reads as ParentRunID) and returns a RunResult
// with a distinct child RunID.
type capturePlanRunner struct {
	mu       sync.Mutex
	seenRI   []hwcloud.RunInfo
	childRun string
}

func (c *capturePlanRunner) RunWithPrefix(ctx context.Context, _ hwcloud.Session, _ []hwcloud.Message, _ hwcloud.Message) (*hwcloud.RunResult, error) {
	c.mu.Lock()
	c.seenRI = append(c.seenRI, hwcloud.RunInfoFromContext(ctx))
	c.mu.Unlock()
	return &hwcloud.RunResult{RunID: c.childRun, FinalOutput: "step-output"}, nil
}

func (c *capturePlanRunner) RunStreamWithPrefix(ctx context.Context, _ hwcloud.Session, _ []hwcloud.Message, _ hwcloud.Message) <-chan hwcloud.StreamEvent {
	c.mu.Lock()
	c.seenRI = append(c.seenRI, hwcloud.RunInfoFromContext(ctx))
	c.mu.Unlock()
	ch := make(chan hwcloud.StreamEvent, 1)
	ch <- hwcloud.StreamEvent{Type: hwcloud.StreamDone, Result: &hwcloud.RunResult{RunID: c.childRun, FinalOutput: "step-output"}}
	close(ch)
	return ch
}

func (c *capturePlanRunner) snapshot() []hwcloud.RunInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]hwcloud.RunInfo, len(c.seenRI))
	copy(out, c.seenRI)
	return out
}

// TestPlan_RunIDPropagation is the #1 regression test for the orchestrate
// layer. A plan execution must:
//
//   - generate a plan-level RunID (PlanResult.PlanRunID non-empty);
//   - stamp it into ctx so every step's child agent kernel.run() reads it as
//     ParentRunID (the child's RunInfoFromContext(ctx).RunID == PlanRunID);
//   - capture each step's child RunID on StepResult.RunID, distinct from
//     PlanRunID;
//   - echo PlanRunID on PlanState (so a resumed run joins the same trajectory).
//
// This is the plan-level half of the multi-agent trajectory reassembly link;
// the team half is covered by agent.TestTeam_RunIDPropagation.
func TestPlan_RunIDPropagation(t *testing.T) {
	runner := &capturePlanRunner{childRun: "step-child-run"}

	def := &PlanDef{
		Goal: "do the thing",
		Steps: []StepDef{
			{ID: "s1", Agent: "worker", Task: "first step", Final: true},
		},
	}

	plan := NewPlan(
		WithPlanner(&staticPlanner{def: def}),
		WithAgent("worker", "the worker", runner),
	)

	result, err := plan.Execute(context.Background(), hwcloud.Session{ID: "plan-sess"}, def)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.PlanRunID == "" {
		t.Fatal("PlanResult.PlanRunID is empty; Plan.execute did not stamp a plan-level run id")
	}
	if len(result.Steps) != 1 {
		t.Fatalf("expected 1 step result, got %d", len(result.Steps))
	}
	sr := result.Steps[0]
	if sr.RunID == "" {
		t.Fatal("StepResult.RunID is empty; executeStep did not capture the child RunResult.RunID")
	}
	// The child's RunID is distinct from the plan's — the plan is the parent.
	if sr.RunID == result.PlanRunID {
		t.Errorf("step RunID == PlanRunID (%q); child id not distinct from plan id", sr.RunID)
	}

	// The child saw the plan RunID as its ctx RunID — kernel.run reads that
	// as ParentRunID. Asserted at the orchestrate layer: the ctx handed to
	// RunStreamWithPrefix/RunWithPrefix carries planRunID.
	ris := runner.snapshot()
	if len(ris) == 0 {
		t.Fatal("capturePlanRunner saw no RunInfo in ctx; executor did not delegate to the agent")
	}
	ri := ris[0]
	if ri.RunID != result.PlanRunID {
		t.Errorf("child ctx RunInfo.RunID = %q, want plan %q (child kernel.run reads this as ParentRunID)", ri.RunID, result.PlanRunID)
	}
}

// TestPlan_RunIDPropagation_StampsCtxFromGrandparent verifies the plan
// preserves a grandparent RunID (e.g. a Plan launched from inside a Team) by
// reading the incoming ctx BEFORE re-stamping — WithRunInfo shadows, so a
// naive re-stamp would erase the team's RunID and split the trajectory at the
// plan boundary. This is the nested-link half of #1.
func TestPlan_RunIDPropagation_StampsCtxFromGrandparent(t *testing.T) {
	runner := &capturePlanRunner{childRun: "nested-child-run"}
	grandparentRunID := "team-grandparent-run"

	def := &PlanDef{
		Goal:  "nested goal",
		Steps: []StepDef{{ID: "s1", Agent: "worker", Task: "only step", Final: true}},
	}
	plan := NewPlan(
		WithPlanner(&staticPlanner{def: def}),
		WithAgent("worker", "worker", runner),
	)

	// Pre-stamp a grandparent RunID, as a Team would before delegating to Plan.
	ctx := hwcloud.WithRunInfo(context.Background(), hwcloud.RunInfo{
		RunID:       grandparentRunID,
		TurnID:      -1,
		ParentRunID: "", // the team is the top of this trajectory
	})

	result, err := plan.Execute(ctx, hwcloud.Session{ID: "nested-sess"}, def)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.PlanRunID == "" {
		t.Fatal("PlanResult.PlanRunID is empty")
	}
	if result.PlanRunID == grandparentRunID {
		t.Fatal("plan generated the grandparent's RunID instead of a fresh plan-level id")
	}

	// The child saw the PLAN's RunID (not the grandparent's) as its ctx
	// RunID — kernel.run reads that as ParentRunID, linking the child to
	// the plan, not directly to the team.
	ris := runner.snapshot()
	if len(ris) == 0 {
		t.Fatal("capturePlanRunner saw no RunInfo in ctx")
	}
	ri := ris[0]
	if ri.RunID != result.PlanRunID {
		t.Errorf("child ctx RunInfo.RunID = %q, want plan %q", ri.RunID, result.PlanRunID)
	}
	// The plan must preserve the grandparent RunID in its OWN ParentRunID by
	// reading prev BEFORE re-stamping (WithRunInfo shadows). The ctx the
	// child receives inherits that: ri.ParentRunID == grandparent. If the
	// plan had naively re-stamped without reading prev, ri.ParentRunID would
	// be empty and the trajectory would split at the plan boundary
	// (child → plan ✓, but plan → team ✗). The child's kernel.run then
	// re-stamps with ParentRunID = planRunID (its direct parent), so the
	// grandparent stays on the plan — not leaked into the child's events —
	// and a consumer walks the chain child → plan → team via ParentRunID.
	if ri.ParentRunID != grandparentRunID {
		t.Errorf("child ctx ParentRunID = %q, want grandparent %q (plan failed to preserve prev.ParentRunID across its re-stamp — trajectory splits at plan boundary)", ri.ParentRunID, grandparentRunID)
	}
}
