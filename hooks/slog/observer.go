package slog

import (
	"context"
	"log/slog"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// Observer implements hwcloud.RunObserver + hwcloud.DecisionObserver via
// log/slog. It is the observation-axis counterpart to Hooks (the lifecycle
// axis): Hooks logs agent/tool start/end; Observer logs stage boundaries and
// decision events. The two are independent types — wire one or both via
// kernel.Deps. Both share a single *slog.Logger so one handler config
// (rotated file / discard / never stderr) governs the whole run's logging.
type Observer struct {
	logger *slog.Logger
}

// NewObserver creates an Observer. nil logger falls back to slog.Default()
// (Hooks.New does NOT nil-guard — the asymmetry is intentional: Observer is
// the more "always-on" of the two and tolerates a bare NewObserver(nil),
// e.g. when a caller has not yet built a handler but still wants stage/
// decision events on the default logger).
func NewObserver(logger *slog.Logger) *Observer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Observer{logger: logger}
}

// ObserveStage logs stage boundaries. enter/leave on success → Debug; a
// non-nil Err (leave only — the kernel passes nil err on enter) → Warn and
// suppresses the Debug line (one line per event, level by outcome). Stage
// failures are often retried (model 429 → next turn continues), so Warn not
// Error — contrast Hooks.OnAgentEnd which uses Error for terminal run
// failures. run_id/turn_id are stamped so stage logs join to the run
// trajectory the same way DecisionEvents do.
func (o *Observer) ObserveStage(ctx context.Context, ev hwcloud.StageEvent) {
	attrs := []any{
		"name", ev.Name,
		"phase", ev.Phase,
		// Duration.String() renders a human-readable unit (e.g.
		// "713.928679ms", "143.288µs") instead of a bare nanosecond int,
		// which is unreadable in a log line. String form still sorts
		// correctly for equal-unit durations and filters by prefix.
		"duration", ev.Duration.String(),
		"run_id", ev.RunID,
		"turn_id", ev.TurnID,
	}
	if ev.ParentRunID != "" {
		attrs = append(attrs, "parent_run_id", ev.ParentRunID)
	}
	if ev.Err != nil {
		attrs = append(attrs, "error", ev.Err)
		o.logger.WarnContext(ctx, "stage failed", attrs...)
		return
	}
	o.logger.DebugContext(ctx, "stage", attrs...)
}

// ObserveDecision logs decision events at a level chosen by Outcome (not
// Layer — Layer needs Outcome to disambiguate, e.g. CompactionAuto/Freed is
// routine while VectorRecall/Failed is a degraded path; level reflects
// importance to an operator, which Outcome captures directly):
//
//	Failed                       → Warn   (degraded but recoverable; not Error)
//	Deny, Ask                    → Info   (actionable: security block / human escalation)
//	Allow, Hit, Miss, Skipped,
//	Stored, Attempted, Freed     → Debug  (routine)
//
// Detail nests under slog.Group("detail",...) — a JSON sub-object — so its
// arbitrary keys (count/error/id/dim/freed_tokens/…) never collide with the
// fixed identity fields below (Detail["error"] vs a top-level "error", for
// instance). Fixed identity fields stay top-level so a dashboard can filter
// directly on layer + outcome + run_id + turn_id + session_id without
// descending into the detail blob.
func (o *Observer) ObserveDecision(ctx context.Context, ev hwcloud.DecisionEvent) {
	level := slog.LevelDebug
	switch ev.Outcome {
	case hwcloud.OutcomeFailed:
		level = slog.LevelWarn
	case hwcloud.OutcomeDeny, hwcloud.OutcomeAsk:
		level = slog.LevelInfo
	}
	attrs := []any{
		"layer", ev.Layer,
		"outcome", ev.Outcome,
		"subject", ev.Subject,
		"run_id", ev.RunID,
		"turn_id", ev.TurnID,
	}
	if ev.ParentRunID != "" {
		attrs = append(attrs, "parent_run_id", ev.ParentRunID)
	}
	if ev.SessionID != "" {
		attrs = append(attrs, "session_id", ev.SessionID)
	}
	if ev.CallID != "" {
		attrs = append(attrs, "call_id", ev.CallID)
	}
	if len(ev.Detail) > 0 {
		detailAttrs := make([]any, 0, len(ev.Detail)*2)
		for k, v := range ev.Detail {
			detailAttrs = append(detailAttrs, k, v)
		}
		attrs = append(attrs, slog.Group("detail", detailAttrs...))
	}
	o.logger.Log(ctx, level, "decision", attrs...)
}

var _ hwcloud.RunObserver = (*Observer)(nil)
var _ hwcloud.DecisionObserver = (*Observer)(nil)
