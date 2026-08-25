package hwcloud

import (
	"context"
	"time"
)

// ── Stage constants ──

const (
	StageMemoryFetch  = "memory.fetch"  // Recent() + Search(), turn 1 only
	StageGuardIn      = "guard.in"      // input guard, before loop
	StagePromptBuild  = "prompt.build"  // build messages for model
	StageModelCall    = "model.call"    // ChatCompletion + streaming
	StageGuardOut     = "guard.out"     // output guard (model + tool results)
	StageToolExecute  = "tool.execute"  // single tool execution
	StageMemoryAppend = "memory.append" // write message to storage

	// Team-level stages (Team observer, not Agent observer).
	StageTeamAgent = "team.agent" // agent enter/leave within a team run
	StageTeamRoute = "team.route" // router fallback event
)

// ── StageEvent ──

// StageEvent is emitted by the Runner at each stage boundary.
type StageEvent struct {
	Name        string         // stage constant
	Phase       string         // "enter" or "leave"
	Detail      map[string]any // optional metadata (tool name, model ID, etc.)
	Duration    time.Duration  // wall-clock time of the stage, set on "leave"
	Err         error          // non-nil if the stage failed
	RunID       string         // groups events into a run trajectory (empty for legacy/pre-instrumentation)
	TurnID      int            // turn index within the run (0-based; -1 = pre-loop)
	ParentRunID string         // #1: the enclosing run's RunID (team/orchestrator); empty for solo runs
}

// ── RunObserver ──

// RunObserver receives stage-level events from the Runner mainline loop.
// nil RunObserver = events are silently dropped.
//
// Unlike RunHooks which observes agent/tool lifecycles, RunObserver observes
// every stage inside the 8-node loop — memory fetch, prompt build, guard checks,
// model calls, tool execution, and memory append.
//
// Concurrency contract: implementations MUST be safe for concurrent use.
// StageToolExecute events are emitted from the tool job goroutines (parallel
// tools), everything else from the run goroutine — events from different
// stages can interleave arbitrarily. "enter"/"leave" pairs are guaranteed
// within a stage (even when the stage body panics), but leave may arrive
// after a later stage's enter when the body runs on a job goroutine.
type RunObserver interface {
	ObserveStage(ctx context.Context, event StageEvent)
}

// MultiObserver combines multiple RunObservers into one.
// Each observer is called in order; one observer failing does not
// prevent subsequent observers from running. Nil observers are skipped.
func MultiObserver(observers ...RunObserver) RunObserver {
	var filtered []RunObserver
	for _, o := range observers {
		if o != nil {
			filtered = append(filtered, o)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return &multiObserver{list: filtered}
	}
}

type multiObserver struct {
	list []RunObserver
}

func (m *multiObserver) ObserveStage(ctx context.Context, event StageEvent) {
	for _, o := range m.list {
		o.ObserveStage(ctx, event)
	}
}

// ObserveDecision fans the event out to the subset of the list that also
// implements DecisionObserver. Observers that only implement RunObserver
// are silently skipped — this is how old Go implementers keep working
// unchanged when combined into a MultiObserver alongside a DecisionObserver.
func (m *multiObserver) ObserveDecision(ctx context.Context, event DecisionEvent) {
	for _, o := range m.list {
		if d, ok := o.(DecisionObserver); ok {
			d.ObserveDecision(ctx, event)
		}
	}
}

// ── Run identity + context propagation ──

// RunInfo carries the run/turn identity that groups events into a trajectory.
// Subsystems extract it from ctx to stamp DecisionEvents (and StageEvents);
// the kernel injects it once at run() entry and updates TurnID per turn.
//
// ParentRunID links a child run to its enclosing run — a team run spawns one
// child run per agent, an orchestrator run spawns one per plan step. Each
// child's kernel.run() reads the parent's RunID from ctx BEFORE re-stamping
// its own RunID (WithRunInfo shadows, it does not merge — the run() entry
// must explicitly preserve ParentRunID). Empty for solo runs.
type RunInfo struct {
	RunID       string
	TurnID      int
	ParentRunID string
}

type runInfoCtxKey struct{}
type decObserverCtxKey struct{}

// WithRunInfo stamps the current run/turn identity into ctx so leaf packages
// can recover it when emitting events. The kernel calls this at run() entry
// and again at the top of each turn loop iteration.
func WithRunInfo(ctx context.Context, info RunInfo) context.Context {
	return context.WithValue(ctx, runInfoCtxKey{}, info)
}

// RunInfoFromContext returns the stamped RunInfo, or the zero value when the
// ctx was not prepared by the kernel (e.g. a standalone subsystem call).
func RunInfoFromContext(ctx context.Context) RunInfo {
	if v, ok := ctx.Value(runInfoCtxKey{}).(RunInfo); ok {
		return v
	}
	return RunInfo{}
}

// WithDecisionObserver injects a DecisionObserver into ctx so leaf packages
// (context/, provider/) can emit decision events without holding a struct
// field — mirrors the SessionFromContext pattern. nil is a no-op.
func WithDecisionObserver(ctx context.Context, obs DecisionObserver) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, decObserverCtxKey{}, obs)
}

// DecisionObserverFromContext returns the injected DecisionObserver, or nil
// when none was set (the caller MUST nil-check before emitting).
func DecisionObserverFromContext(ctx context.Context) DecisionObserver {
	if v, ok := ctx.Value(decObserverCtxKey{}).(DecisionObserver); ok {
		return v
	}
	return nil
}

// ── DecisionEvent ──

// DecisionEvent is emitted at a single decision point inside a subsystem —
// the granularity RunObserver/StageEvent cannot reach: which policy layer
// fired, whether recall hit the vector index or the keyword fallback, whether
// compaction actually freed tokens. Events fire immediately at each point;
// consumers reassemble trajectories via the four-tuple join key
// (session_id, run_id, turn_id, call_id).
//
// Concurrency contract mirrors RunObserver: implementations MUST be safe for
// concurrent use. Events from different turns/tools can interleave.
type DecisionEvent struct {
	Layer       string         // decision-layer constant (DecisionPolicyRule, DecisionVectorRecall, ...)
	Outcome     string         // decision-value constant (OutcomeAllow, OutcomeHit, ...)
	Subject     string         // what was decided on (tool name, query snippet, sessionID)
	Detail      map[string]any // optional structured payload (scores, rule reason, freed tokens)
	RunID       string         // trajectory grouping key (empty when ctx had no RunInfo)
	TurnID      int            // turn index within the run (0-based; -1 = pre-loop)
	ParentRunID string         // #1: enclosing run's RunID (team/orchestrator); empty for solo runs
	SessionID   string         // #2: session the decision belongs to (auto-stamped from ctx when blank)
	CallID      string         // #4: the tool call this decision governs (empty for non-tool decisions)
}

// Decision-layer constants — the "where" of a decision. Each names a single
// instrumented decision point in a subsystem.
const (
	// governance 4-layer chain (governance/policy.go Evaluate).
	DecisionPolicyRule   = "policy.rule"   // rules layer (first match wins)
	DecisionPolicySafety = "policy.safety" // safety classifier (ReadOnly → Allow)
	DecisionPolicyMemory = "policy.memory" // approval memory (multi-key ALL / single Recall)
	DecisionPolicyHuman  = "policy.human"  // human approver (askHuman)
	// governance custom Policy (kernel/decision.go observingPolicy wrapper).
	// One aggregate final-outcome event — custom impls can't be per-layer
	// instrumented, so the wrapper emits the verdict only.
	DecisionPolicyCustom = "policy.custom"
	// context 3 points (context/runtime.go Build).
	DecisionContextRecall   = "context.recall"    // knowledge recall
	DecisionContextSkill    = "context.skill"     // skill discover
	DecisionContextResource = "context.resource"  // resource search
	// provider retrieval (provider/memory/sqlite/memory.go).
	DecisionVectorRecall  = "provider.vector"    // vector cosine recall
	DecisionKeywordRecall = "provider.keyword"   // keyword LIKE fallback
	DecisionExtractor     = "provider.extractor" // knowledge extraction/store
	// session compaction (kernel/compress.go + kernel/prepare.go).
	DecisionCompactionAuto   = "compaction.auto"   // turn-0 overflow-triggered
	DecisionCompactionManual = "compaction.manual" // /compact slash command
)

// Decision-value constants — the "what" of a decision. Shared across layers
// so a consumer can tally outcomes without parsing per-layer vocabularies.
const (
	OutcomeAllow     = "allow"     // governance: tool allowed
	OutcomeDeny      = "deny"      // governance: blocked
	OutcomeAsk       = "ask"       // governance: escalated to human
	OutcomeHit       = "hit"       // retrieval: returned results
	OutcomeMiss      = "miss"      // retrieval: no results
	OutcomeSkipped   = "skipped"   // layer short-circuited (no work done)
	OutcomeStored    = "stored"    // extractor: knowledge persisted
	OutcomeAttempted = "attempted" // compaction: Compact called
	OutcomeFreed     = "freed"     // compaction: summary advanced, tokens freed
	OutcomeFailed    = "failed"    // any layer: error
)

// ── DecisionObserver ──

// DecisionObserver receives per-decision-point events. OPTIONAL: a RunObserver
// need not implement it. Old Go implementers compile unchanged and silently
// skip decision events — the type-assertion dance in ObserveDecision (and in
// multiObserver.ObserveDecision) routes events only to observers that opted in.
// This is the io.ReadSeeker pattern: a separate interface checked via type
// assertion, never a requirement of RunObserver.
type DecisionObserver interface {
	ObserveDecision(ctx context.Context, event DecisionEvent)
}

// ObserveDecision emits to obs if it implements DecisionObserver, else no-op.
// Callers pass their RunObserver directly; the helper does the type-assertion
// dance and stamps the trajectory join keys from ctx when the caller left them
// blank (the common case for leaf packages that build the event from local
// data). RunID/TurnID/ParentRunID come from RunInfo; SessionID comes from the
// session injected into ctx (kernel run() calls WithSession so leaf packages
// that have no session in scope still get one). CallID is caller-set — the
// helper cannot infer it from ctx, so non-tool decisions correctly stay empty.
// nil-safe.
func ObserveDecision(ctx context.Context, obs RunObserver, event DecisionEvent) {
	if obs == nil {
		return
	}
	d, ok := obs.(DecisionObserver)
	if !ok {
		return
	}
	ri := RunInfoFromContext(ctx) // extract once — every blank key reads the same value
	if event.RunID == "" {
		event.RunID = ri.RunID
		event.TurnID = ri.TurnID
	}
	if event.ParentRunID == "" {
		event.ParentRunID = ri.ParentRunID
	}
	if event.SessionID == "" {
		if s, ok := SessionFromContext(ctx); ok {
			event.SessionID = s.ID
		}
	}
	d.ObserveDecision(ctx, event)
}
