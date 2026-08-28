package context

import (
	"context"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/provider/resource"
	"github.com/Cloud-Developer-Department/hwcloud/provider/skill"
	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// Config wires the Context Runtime to its providers.
type Config struct {
	SessionStore     session.SessionStore
	Compressor       session.Compressor
	MemoryProvider   MemoryProvider
	SkillProvider    skill.Provider
	ResourceProvider resource.Provider
	Observer         hwcloud.RunObserver
}

// BuildRequest is the per-turn input to Build.
type BuildRequest struct {
	Session    hwcloud.Session
	Goal       string
	WorkingSet []hwcloud.Message // assembled working messages (kernel's compaction result)
}

// ContextRuntime assembles the [AgentContext] an agent sees: working
// messages plus durable knowledge recalled from the MemoryProvider,
// scoped by the session. It owns nothing the kernel doesn't hand it —
// compaction stays in the kernel; Build only layers knowledge on top and
// Commit persists messages.
type ContextRuntime struct {
	cfg Config
}

// NewContextRuntime creates a ContextRuntime.
func NewContextRuntime(cfg Config) *ContextRuntime {
	return &ContextRuntime{cfg: cfg}
}

// Build returns the assembled AgentContext: working set + recalled
// knowledge (preferences/facts/lessons) scoped to the session. Knowledge
// recall is best-effort — a provider failure degrades to no memories.
func (c *ContextRuntime) Build(ctx context.Context, req BuildRequest) (*AgentContext, error) {
	ac := &AgentContext{
		Messages: req.WorkingSet,
	}
	// Query from the goal when present, else the latest user input.
	query := req.Goal
	if query == "" && len(req.WorkingSet) > 0 {
		for i := len(req.WorkingSet) - 1; i >= 0; i-- {
			if req.WorkingSet[i].Role == hwcloud.RoleUser {
				query = req.WorkingSet[i].Content
				break
			}
		}
	}
	if query == "" {
		return ac, nil
	}

	// DecisionObserver + RunInfo are injected into ctx by the kernel at
	// run() entry (mirrors SessionFromContext). Leaf packages emit decision
	// events without holding a struct field; nil = silent (no observer, or
	// the RunObserver does not implement DecisionObserver).
	obs := hwcloud.DecisionObserverFromContext(ctx)
	ri := hwcloud.RunInfoFromContext(ctx)
	// The emit closure does not hold `session` (req.Session is Build-scoped,
	// not closure-scoped), so capture the ID once here for the SessionID
	// join key. CallID stays empty — context decisions are not tool calls.
	sessionID := req.Session.ID
	emit := func(layer, outcome, subject string, detail map[string]any) {
		if obs != nil {
			obs.ObserveDecision(ctx, hwcloud.DecisionEvent{
				Layer: layer, Outcome: outcome, Subject: subject,
				Detail: detail, RunID: ri.RunID, TurnID: ri.TurnID,
				ParentRunID: ri.ParentRunID, SessionID: sessionID,
			})
		}
	}

	// Knowledge recall (durable memory, user-level: knowledge is stored
	// and recalled across sessions, not scoped to the current one).
	if c.cfg.MemoryProvider != nil {
		scope := ContextScope{
			UserID: req.Session.UserID,
		}
		memories, err := c.cfg.MemoryProvider.Recall(ctx, scope, query, 5)
		if err != nil {
			// Best-effort layer: a provider failure degrades to no
			// memories, but must not fail the run. The DecisionEvent
			// (OutcomeFailed) reaches the slog Observer via the ctx's
			// DecisionObserver — the operator-facing log line.
			emit(hwcloud.DecisionContextRecall, hwcloud.OutcomeFailed, query, map[string]any{"error": err.Error()})
		} else {
			ac.Memories = memories
			outcome := hwcloud.OutcomeMiss
			if len(memories) > 0 {
				outcome = hwcloud.OutcomeHit
			}
			emit(hwcloud.DecisionContextRecall, outcome, query, map[string]any{"count": len(memories)})
		}
	}

	// Skills: the FULL catalog goes into context every turn (industry
	// pattern — Claude Code / OpenAI Agent Skills list every skill's
	// name+description and let the model decide). The intent query is NOT
	// used here: a continuation turn ("continue", "ok") carries no intent
	// and would wipe the catalog from the prompt. Provider.Match stays for
	// future scale (hundreds of skills) but is not the injection path.
	if c.cfg.SkillProvider != nil {
		skills, err := c.cfg.SkillProvider.Discover(ctx)
		if err != nil {
			emit(hwcloud.DecisionContextSkill, hwcloud.OutcomeFailed, "catalog", map[string]any{"error": err.Error()})
		} else {
			ac.Skills = skills
			outcome := hwcloud.OutcomeMiss
			if len(skills) > 0 {
				outcome = hwcloud.OutcomeHit
			}
			emit(hwcloud.DecisionContextSkill, outcome, "catalog", map[string]any{"count": len(skills)})
		}
	}

	// Resource search: external reference material relevant to the goal.
	if c.cfg.ResourceProvider != nil {
		resources, err := c.cfg.ResourceProvider.Search(ctx, query, 5)
		if err != nil {
			emit(hwcloud.DecisionContextResource, hwcloud.OutcomeFailed, query, map[string]any{"error": err.Error()})
		} else {
			ac.Resources = resources
			outcome := hwcloud.OutcomeMiss
			if len(resources) > 0 {
				outcome = hwcloud.OutcomeHit
			}
			emit(hwcloud.DecisionContextResource, outcome, query, map[string]any{"count": len(resources)})
		}
	}

	return ac, nil
}

// Runtime is the Context Runtime interface — the kernel consumes this,
// not the concrete implementation, so applications can substitute their
// own context assembly (e.g. a server-backed implementation).
type Runtime interface {
	Build(ctx context.Context, req BuildRequest) (*AgentContext, error)
}

// Compile-time assertion: the concrete runtime implements the interface.
var _ Runtime = (*ContextRuntime)(nil)
