package kernel

import (
	"context"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
)

// observingPolicy wraps a custom Policy implementation (one that is NOT
// *governance.Engine) and emits a single aggregate DecisionEvent carrying
// the final outcome. This is shallow by design: we cannot instrument the
// internals of a user-supplied Policy. The default Engine, by contrast,
// emits deep per-layer events (rule/safety/memory/human) via its
// DecObserver field, so it is never wrapped — see executeTools.
//
// The wrapper is only applied when the kernel's RunObserver also
// implements DecisionObserver; otherwise observingPolicy is never
// constructed and the custom Policy runs exactly as before.
type observingPolicy struct {
	inner governance.Policy
	obs   hwcloud.DecisionObserver
}

// Evaluate delegates to the inner Policy and emits one DecisionPolicyCustom
// event with the resulting outcome (allow/deny/ask, or failed/skipped on
// error). RunInfo is recovered from ctx so the event joins the correct
// trajectory.
func (o observingPolicy) Evaluate(ctx context.Context, call hwcloud.ToolCall, def hwcloud.FunctionDefinition, session hwcloud.Session) (governance.Decision, error) {
	dec, err := o.inner.Evaluate(ctx, call, def, session)
	ri := hwcloud.RunInfoFromContext(ctx)
	outcome := hwcloud.OutcomeSkipped
	detail := map[string]any{}
	if err != nil {
		outcome = hwcloud.OutcomeFailed
		detail["error"] = err.Error()
	} else {
		// governance.ApprovalAction is a string-typed alias, so its
		// underlying value (allow/deny/ask) maps directly to an outcome.
		outcome = string(dec.Action)
		detail["reason"] = dec.Reason
	}
	o.obs.ObserveDecision(ctx, hwcloud.DecisionEvent{
		Layer:       hwcloud.DecisionPolicyCustom,
		Outcome:     outcome,
		Subject:     call.Function.Name,
		Detail:      detail,
		RunID:       ri.RunID,
		TurnID:      ri.TurnID,
		ParentRunID: ri.ParentRunID,
		SessionID:   session.ID,
		CallID:      call.ID,
	})
	return dec, err
}
