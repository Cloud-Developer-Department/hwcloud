package hwcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// RunHooks provides lifecycle callbacks in the Runner mainline.
// Naming follows OpenAI Agents SDK RunHooks conventions.
// nil RunHooks = no callbacks.
//
// OnAgentStart and OnToolStart return a context and an opaque value. The
// returned context replaces the caller's context for downstream operations
// (tool execution, observer events) — this is how OTel hooks thread span
// parent-child relationships through the run (the standard OTel pattern:
// tracer.Start returns an enriched ctx, the hook returns it, the kernel
// uses it so child spans attach to the parent). The opaque value is handed
// back to the corresponding End method. Implementations use this to carry
// state from start to finish: an OTel span, a start timestamp, a WASM
// guest handle — the Runner never inspects it.
//
// OnToolEnd receives result and err as pointers so that hooks can
// mutate them (redaction, truncation, metadata injection) before the
// result is stored in memory.
type RunHooks interface {
	// OnAgentStart is called once when agent.Run() begins, before the loop.
	// The returned context is used for the rest of the run (tool calls,
	// observer events) so hooks can inject trace context.
	OnAgentStart(ctx context.Context, req ChatCompletionRequest) (context.Context, any, error)
	// OnAgentEnd is called once when agent.Run() finishes (success, error, or cancel).
	OnAgentEnd(ctx context.Context, req ChatCompletionRequest, resp *ChatCompletionResponse, runErr error, startState any)
	// OnToolStart is called before each Tool.Execute. The returned context
	// is used for the tool execution so hooks can inject trace context.
	OnToolStart(ctx context.Context, tool FunctionDefinition, args json.RawMessage) (context.Context, any, error)
	// OnToolEnd is called after each Tool.Execute finishes. result is a
	// pointer — hooks may mutate it (redaction, truncation) before memory
	// storage. All failures are inside result.Error (single channel).
	OnToolEnd(ctx context.Context, tool FunctionDefinition, args json.RawMessage, result *ToolResult, startState any)
}

// MultiHooks combines multiple RunHooks into one. Each hook is called in
// order; one hook returning an error does not prevent subsequent hooks
// from running. Nil hooks are skipped.
//
// Start/End state pairing: OnAgentStart returns a []any, one entry per
// hook. OnAgentEnd receives the same slice and distributes each entry
// back to its hook. Same for OnToolStart/OnToolEnd.
func MultiHooks(hooks ...RunHooks) RunHooks {
	var filtered []RunHooks
	for _, h := range hooks {
		if h != nil {
			filtered = append(filtered, h)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return &multiHooks{list: filtered}
	}
}

type multiHooks struct {
	list []RunHooks
}

func (m *multiHooks) OnAgentStart(ctx context.Context, req ChatCompletionRequest) (context.Context, any, error) {
	states := make([]any, len(m.list))
	var firstErr error
	for i, h := range m.list {
		var s any
		var err error
		ctx, s, err = h.OnAgentStart(ctx, req)
		states[i] = s
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return ctx, states, firstErr
}

func (m *multiHooks) OnAgentEnd(ctx context.Context, req ChatCompletionRequest, resp *ChatCompletionResponse, runErr error, startState any) {
	states, ok := startState.([]any)
	if !ok || len(states) != len(m.list) {
		// State shape mismatch: the start value was not produced by
		// this multiHooks instance (e.g. a single hook was set via
		// WithRunHook and then wrapped, or a caller passed a foreign
		// startState). Distribute nil to every hook so none receives a
		// wrong sibling's state, and surface the mismatch loudly rather
		// than silently dropping per-hook state.
		slog.Warn("MultiHooks.OnAgentEnd startState shape mismatch",
			"got", fmt.Sprintf("%T len=%d", startState, 0),
			"want", fmt.Sprintf("[]any len=%d", len(m.list)),
		)
		states = nil
	}
	for i, h := range m.list {
		var s any
		if i < len(states) {
			s = states[i]
		}
		h.OnAgentEnd(ctx, req, resp, runErr, s)
	}
}

func (m *multiHooks) OnToolStart(ctx context.Context, tool FunctionDefinition, args json.RawMessage) (context.Context, any, error) {
	states := make([]any, len(m.list))
	var firstErr error
	for i, h := range m.list {
		var s any
		var err error
		ctx, s, err = h.OnToolStart(ctx, tool, args)
		states[i] = s
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return ctx, states, firstErr
}

func (m *multiHooks) OnToolEnd(ctx context.Context, tool FunctionDefinition, args json.RawMessage, result *ToolResult, startState any) {
	states, ok := startState.([]any)
	if !ok || len(states) != len(m.list) {
		// Same rationale as OnAgentEnd: never hand a hook a sibling's or
		// foreign state — distribute nil and warn. Silently zero-filling
		// (the old behavior) would mask a misconfigured hook pipeline
		// (e.g. nested MultiHooks) where every hook quietly loses state.
		slog.Warn("MultiHooks.OnToolEnd startState shape mismatch",
			"tool", tool.Name,
			"got", fmt.Sprintf("%T", startState),
			"want", fmt.Sprintf("[]any len=%d", len(m.list)),
		)
		states = nil
	}
	for i, h := range m.list {
		var s any
		if i < len(states) {
			s = states[i]
		}
		h.OnToolEnd(ctx, tool, args, result, s)
	}
}
