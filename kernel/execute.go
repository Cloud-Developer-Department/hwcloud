package kernel

import (
	"context"
	"fmt"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/eventbus"
	"github.com/Cloud-Developer-Department/hwcloud/execution"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
)

// executeTools orchestrates the ⑥ policy/approval + ⑦ execution nodes.
// Approval happens sequentially first (the user clicks through dialogs
// quickly), then approved tools execute concurrently. Results preserve
// the call order.
func (rt *Runtime) executeTools(ctx context.Context, session hwcloud.Session, calls []hwcloud.ToolCall, ch chan<- hwcloud.StreamEvent) []hwcloud.Message {
	if len(calls) == 0 {
		return nil
	}

	results := make([]hwcloud.Message, len(calls))

	policy := rt.policy()
	// Custom (non-Engine) Policy impls can't be instrumented per-layer, so
	// wrap them with observingPolicy to capture ONE aggregate final-outcome
	// event. The default *governance.Engine already emits deep per-layer
	// events via its DecObserver field, so it is NOT wrapped (double-counting).
	if _, isEngine := policy.(*governance.Engine); !isEngine {
		if obs := rt.decisionObserver(); obs != nil {
			policy = observingPolicy{inner: policy, obs: obs}
		}
	}

	// Phase 1: evaluate all tools sequentially through the policy chain.
	approved := make([]bool, len(calls))
	for i, call := range calls {
		rc := rt.execution.Resolve(call.Function.Name)
		if rc == nil {
			results[i] = hwcloud.Message{
				Role:       hwcloud.RoleTool,
				ToolCallID: call.ID,
				Content:    fmt.Sprintf("tool %q not found", call.Function.Name),
			}
			continue
		}
		rt.logEvent(ctx, session.ID, eventbus.EventApprovalRequest, call.Function.Name, map[string]string{"call_id": call.ID})
		decision, err := policy.Evaluate(ctx, call, rc.Def, session)
		if err != nil {
			results[i] = hwcloud.Message{
				Role:       hwcloud.RoleTool,
				ToolCallID: call.ID,
				Content:    fmt.Sprintf("policy error: %v", err),
			}
			continue
		}
		rt.state.RecordApproval(call, call.Function.Name, decision.Action, decision.Reason)
		rt.logEvent(ctx, session.ID, eventbus.EventApprovalResult, decision, map[string]string{"call_id": call.ID})
		if decision.Action == governance.Deny {
			results[i] = hwcloud.Message{
				Role:       hwcloud.RoleTool,
				ToolCallID: call.ID,
				Content:    fmt.Sprintf("this call rejected, reason: %s", decision.Reason),
			}
			continue
		}
		if decision.ModifiedArgs != nil {
			// Human edited the arguments — execute with the edited args.
			// Mutate the slice element: `call` is a range copy, and Phase 2
			// reads calls[i] again.
			calls[i].Function.Arguments = string(decision.ModifiedArgs)
		}
		approved[i] = true
	}

	// Phase 2: execute approved tools concurrently as jobs, then collect
	// in call order (ordering preserved, execution parallel).
	handles := make([]execution.ExecutionHandle, len(calls))
	for i, call := range calls {
		if !approved[i] {
			continue
		}
		rt.state.RecordExecution(call.ID, call.Function.Name, "running", time.Now())
		rt.logEvent(ctx, session.ID, eventbus.EventToolCall, call.Function.Name, map[string]string{"call_id": call.ID})
		handles[i] = rt.execution.Start(ctx, session, call, ch)
	}
	for i, h := range handles {
		if h == nil {
			continue
		}
		if err := h.Wait(ctx); err != nil && ctx.Err() != nil {
			// Run cancelled mid-execution: cancel this job and the rest,
			// keep the (complete) results — Output below waits for the
			// job to actually finish, so the loop's cancel compensation
			// sees real tool results, not zero-value messages.
			h.Cancel()
			for _, rest := range handles[i+1:] {
				if rest != nil {
					rest.Cancel()
				}
			}
		}
		results[i] = h.Output()
		rt.state.RecordExecution(calls[i].ID, calls[i].Function.Name, "done", time.Time{})
		rt.logEvent(ctx, session.ID, eventbus.EventToolResult, results[i].Content, map[string]string{"call_id": calls[i].ID})
	}
	return results
}

// toolDef resolves a tool name to its FunctionDefinition (built-in or
// registered). Used by the loop's handoff check.
func (rt *Runtime) toolDef(name string) *hwcloud.FunctionDefinition {
	rc := rt.execution.Resolve(name)
	if rc == nil {
		return nil
	}
	d := rc.Def
	return &d
}

// toolDefinitions maps tools to their function definitions.
func toolDefinitions(tools []hwcloud.Tool) []hwcloud.FunctionDefinition {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]hwcloud.FunctionDefinition, len(tools))
	for i, t := range tools {
		defs[i] = t.Definition()
	}
	return defs
}
