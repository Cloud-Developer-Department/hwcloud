// Package execution implements the Execution Runtime: running tool calls
// with hooks, streaming, result policy, and built-in tools. It is consumed
// by kernel.Runtime, which owns approval orchestration and the tools slice.
//
// P1: synchronous Execute. P3: Job-ization (Start/Wait/Cancel) for
// long-running tools, retry, and background execution.
package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
	"github.com/Cloud-Developer-Department/hwcloud/provider/skill"
)

// BuiltinHandler runs a built-in tool (load_skill, reload_skills, recall).
type BuiltinHandler func(ctx context.Context, session hwcloud.Session, call hwcloud.ToolCall, ch chan<- hwcloud.StreamEvent) hwcloud.Message

// Config wires the Execution Runtime to its dependencies.
type Config struct {
	// ToolSnapshot returns the current registered tool set (kernel
	// injects rt.SnapshotTools).
	ToolSnapshot func() []hwcloud.Tool
	// SkillProvider resolves skills for load_skill/reload_skills.
	SkillProvider skill.Provider
	// MemoryProvider feeds the recall built-in (durable knowledge).
	MemoryProvider ctxpkg.MemoryProvider
	// Hooks/Observer receive tool lifecycle events.
	Hooks    hwcloud.RunHooks
	Observer hwcloud.RunObserver
	// ResultPolicy truncates oversized results (after hooks).
	ResultPolicy hwcloud.ResultPolicy
	// SessionFromContext is optional; nil uses hwcloud.SessionFromContext.
	SessionFromContext func(ctx context.Context) (hwcloud.Session, bool)
}

// ExecutionRuntime executes tool calls.
type ExecutionRuntime struct {
	cfg Config

	// loadedSkills/loadedSkillsMu: skill bodies cached by load_skill,
	// refreshed by reload_skills (shared with the kernel prompt builder).
	loadedSkills   map[string]string
	loadedSkillsMu sync.RWMutex
}

// New creates an ExecutionRuntime.
func New(cfg Config) *ExecutionRuntime {
	return &ExecutionRuntime{
		cfg:          cfg,
		loadedSkills: make(map[string]string),
	}
}

// Resolve resolves a tool name to its definition and tool instance.
// builtin=true for built-in tools (executed by Execute's builtin path).
type ResolvedCall struct {
	Def     hwcloud.FunctionDefinition
	Tool    hwcloud.Tool
	Builtin bool
}

// Resolve returns nil if the tool is neither built-in nor registered.
func (e *ExecutionRuntime) Resolve(name string) *ResolvedCall {
	switch name {
	case "load_skill", "reload_skills", "recall":
		return &ResolvedCall{Def: e.builtinDef(name), Builtin: true}
	}
	if e.cfg.ToolSnapshot == nil {
		return nil
	}
	for _, t := range e.cfg.ToolSnapshot() {
		if t.Definition().Name == name {
			d := t.Definition()
			return &ResolvedCall{Def: d, Tool: t}
		}
	}
	return nil
}

// Start launches a tool call as a background job (P3 Job model). The
// kernel starts all approved calls concurrently, then waits in call order.
func (e *ExecutionRuntime) Start(ctx context.Context, session hwcloud.Session, call hwcloud.ToolCall, ch chan<- hwcloud.StreamEvent) ExecutionHandle {
	return e.startJob(ctx, session, call, ch)
}

// execute is the synchronous single-call pipeline. Retryable tool errors
// (ToolResult.Error.Retryable) are retried with backoff up to 2 times.
func (e *ExecutionRuntime) execute(ctx context.Context, session hwcloud.Session, call hwcloud.ToolCall, ch chan<- hwcloud.StreamEvent) hwcloud.Message {
	const maxRetries = 2
	var msg hwcloud.Message
	for attempt := 0; attempt <= maxRetries; attempt++ {
		msg = e.executeOnce(ctx, session, call, ch)
		if !isRetryable(msg) {
			return msg
		}
		if attempt < maxRetries {
			select {
			case <-time.After(time.Duration(1<<uint(attempt)) * 500 * time.Millisecond):
			case <-ctx.Done():
				return msg
			}
		}
	}
	return msg
}

// executeOnce runs a single attempt of the call pipeline.
func (e *ExecutionRuntime) executeOnce(ctx context.Context, session hwcloud.Session, call hwcloud.ToolCall, ch chan<- hwcloud.StreamEvent) hwcloud.Message {
	rc := e.Resolve(call.Function.Name)
	if rc == nil {
		return hwcloud.Message{
			Role:       hwcloud.RoleTool,
			ToolCallID: call.ID,
			Content:    fmt.Sprintf("tool %q not found", call.Function.Name),
		}
	}

	args := json.RawMessage(call.Function.Arguments)
	toolCtx := withSession(ctx, session)

	// ── Built-in tools ──
	if rc.Builtin {
		if h := e.builtinHandler(call.Function.Name); h != nil {
			tc := e.fireToolHooks(toolCtx, rc.Def, args)
			var result *hwcloud.ToolResult
			// Pair the leave + OnToolEnd even if the builtin handler
			// panics (job goroutine recovers above) — see executeOnce.
			defer func() {
				e.fireToolHooksEnd(toolCtx, rc.Def, args, result, tc)
			}()
			// Use tc.ctx (enriched by OnToolStart) so the builtin handler
			// and any spans it creates attach to the tool span.
			msg := h(tc.ctx, session, call, ch)
			result = &hwcloud.ToolResult{Content: msg.Content}
			msg.Content = result.Content
			msg.Result = result
			return msg
		}
		return hwcloud.Message{
			Role:       hwcloud.RoleTool,
			ToolCallID: call.ID,
			Content:    fmt.Sprintf("tool %q not found", call.Function.Name),
		}
	}

	var toolHookState any
	if e.cfg.Hooks != nil {
		var err error
		toolCtx, toolHookState, err = e.cfg.Hooks.OnToolStart(toolCtx, rc.Def, args)
		if err != nil {
			slog.Warn("OnToolStart hook failed", "tool", call.Function.Name, "error", err)
		}
	}

	teStart := time.Now()
	e.observe(toolCtx, hwcloud.StageToolExecute, "enter", map[string]any{"tool": call.Function.Name}, time.Time{}, nil)
	var result *hwcloud.ToolResult
	// Pair the leave + OnToolEnd even when the tool panics (recovered by
	// the job goroutine above executeOnce) — a dangling enter reads as a
	// stuck tool to observers, and an un-Ended OTel tool span is never
	// exported (the trace silently loses the tool call). result.AsError()
	// is nil-safe. toolHookState may be nil if OnToolStart failed; the
	// otel/slog hooks handle nil gracefully.
	//
	// Order matters: OnToolEnd must run BEFORE ResultPolicy (redaction
	// happens inside OnToolEnd; truncation happens in ResultPolicy). So
	// OnToolEnd is called here directly on the normal path, and in the
	// defer only as a panic fallback (guarding with a flag so it never
	// runs twice).
	toolEndDone := false
	callToolEnd := func() {
		if toolEndDone {
			return
		}
		toolEndDone = true
		if e.cfg.Hooks != nil {
			e.cfg.Hooks.OnToolEnd(toolCtx, rc.Def, args, result, toolHookState)
		}
	}
	defer func() {
		callToolEnd() // panic-safe: runs only if not already called
		e.observe(ctx, hwcloud.StageToolExecute, "leave", toolResultDetail(call.Function.Name, result), teStart, result.AsError())
	}()

	// ── Streaming path (optional interface) ──
	// Streaming tools run only when the parent run is itself streaming
	// (ch != nil): under a synchronous run their progress chunks would be
	// concatenated with the final output, duplicating content in the tool
	// result (e.g. the sub-agent streams deltas then the final answer).
	if se, ok := rc.Tool.(hwcloud.StreamExecutor); ok && ch != nil {
		toolCh := se.ExecuteStream(toolCtx, args)
		if toolCh == nil {
			// Broken tool contract: a nil channel is never ready in a
			// select, so the loop below would spin on the ticker forever
			// — the run hangs silently. Fail explicitly instead.
			result = hwcloud.ErrorResult(fmt.Errorf("tool %q: ExecuteStream returned a nil channel", call.Function.Name), false, "")
		} else {
			var buf strings.Builder
			// Rate-limit progress events (1/sec) to avoid flooding the channel.
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			var pending string
			flush := func() {
				if pending != "" && ch != nil {
					select {
					case ch <- hwcloud.StreamEvent{Type: hwcloud.StreamToolProgress, Text: pending, ToolCallID: call.ID}:
					case <-toolCtx.Done():
					}
					pending = ""
				}
			}
			done := false
			cancelled := false
			for !done {
				select {
				case chunk, ok := <-toolCh:
					if !ok {
						done = true
						// A ctx-respecting streaming tool closes its channel
						// early on cancellation (the common pattern) — the
						// channel close, not toolCtx.Done(), is what the loop
						// sees. toolCtx.Err() is only non-nil on real
						// cancellation (job.Cancel / parent ctx): the job
						// does NOT cancel toolCtx on normal completion.
						if toolCtx.Err() != nil {
							cancelled = true
						}
					} else if chunk.Error != nil {
						result = hwcloud.ErrorResult(chunk.Error, false, "")
						done = true
					} else {
						buf.WriteString(chunk.Content)
						pending += chunk.Content
					}
				case <-ticker.C:
					flush()
				case <-toolCtx.Done():
					flush()
					done = true
					cancelled = true
				}
			}
			flush()
			if result == nil {
				if cancelled {
					// Cancelled mid-stream (run cancel / job cancel): the
					// partial content is already on the wire as progress
					// events, but the RESULT must not read as a successful
					// completion — the kernel persists it (cancelled runs
					// commit with a background ctx) and the model would
					// otherwise see a truncated "success" next turn. Same
					// semantics as the blocking path, where a ctx-aware tool
					// returns a cancellation error.
					result = &hwcloud.ToolResult{
						Content: buf.String(),
						Error: &hwcloud.ToolError{
							Message: "tool execution cancelled",
							Code:    "cancelled",
						},
					}
				} else {
					result = &hwcloud.ToolResult{Content: buf.String()}
				}
			}
		}
	} else {
		// ── Blocking path (default) ──
		result = rc.Tool.Execute(toolCtx, args)
	}

	// OnToolEnd runs BEFORE ResultPolicy so redaction (inside OnToolEnd)
	// sees the raw result, and truncation (ResultPolicy) sees the redacted
	// result. callToolEnd is idempotent — the defer calls it only if the
	// normal path didn't (i.e. a panic skipped this line).
	callToolEnd()

	// Result policy (truncation) — after hooks so redaction happens first.
	if e.cfg.ResultPolicy != nil && result != nil {
		result = e.cfg.ResultPolicy.Apply(toolCtx, session, result)
	}

	return toolResultMessage(call, result)
}

// toolResultMessage assembles the RoleTool message from a structured
// result. Single error channel: failures live in result.Error and render
// as "error: <message>" for the model.
func toolResultMessage(call hwcloud.ToolCall, result *hwcloud.ToolResult) hwcloud.Message {
	content := ""
	if result != nil {
		if result.Error != nil {
			content = "error: " + result.Error.Message
		} else {
			content = result.Content
		}
	}
	return hwcloud.Message{
		Role:       hwcloud.RoleTool,
		ToolCallID: call.ID,
		Content:    content,
		Result:     result,
	}
}

// ── hooks/observer plumbing ──

type toolHookCtx struct {
	start     time.Time
	hookState any
	ctx       context.Context // enriched by OnToolStart (carries tool span)
}

func (e *ExecutionRuntime) fireToolHooks(ctx context.Context, def hwcloud.FunctionDefinition, args json.RawMessage) toolHookCtx {
	tc := toolHookCtx{start: time.Now(), ctx: ctx}
	if e.cfg.Hooks != nil {
		var err error
		tc.ctx, tc.hookState, err = e.cfg.Hooks.OnToolStart(ctx, def, args)
		if err != nil {
			slog.Warn("OnToolStart hook failed", "tool", def.Name, "error", err)
		}
	}
	e.observe(tc.ctx, hwcloud.StageToolExecute, "enter", map[string]any{"tool": def.Name}, time.Time{}, nil)
	return tc
}

func (e *ExecutionRuntime) fireToolHooksEnd(ctx context.Context, def hwcloud.FunctionDefinition, args json.RawMessage, result *hwcloud.ToolResult, tc toolHookCtx) {
	e.observe(tc.ctx, hwcloud.StageToolExecute, "leave", toolResultDetail(def.Name, result), tc.start, result.AsError())
	if e.cfg.Hooks != nil {
		e.cfg.Hooks.OnToolEnd(tc.ctx, def, args, result, tc.hookState)
	}
}

// toolResultDetail builds the tool.execute leave detail: metadata only
// (success/error code, size, truncation, artifact) — never the content.
func toolResultDetail(name string, result *hwcloud.ToolResult) map[string]any {
	detail := map[string]any{"tool": name}
	if result == nil {
		detail["ok"] = false
		return detail // panic or nil result — no payload to summarize
	}
	detail["ok"] = result.Error == nil
	if result.Error != nil {
		detail["error_code"] = result.Error.Code
		detail["retryable"] = result.Error.Retryable
	}
	detail["chars"] = len([]rune(result.Content))
	detail["truncated"] = result.Truncated
	if result.FileRef != "" {
		detail["file_ref"] = result.FileRef
	}
	return detail
}

func (e *ExecutionRuntime) observe(ctx context.Context, stage, phase string, detail map[string]any, start time.Time, err error) {
	if e.cfg.Observer == nil {
		return
	}
	// "enter" events pass a zero start (callers mark the start separately
	// and pass it to the matching "leave"). time.Since(zero) is ~1.7e9
	// years — a nonsense duration that observers must ignore today; make
	// it explicit zero instead so nothing has to special-case enter.
	// Mirrors kernel.Runtime.observe (run.go).
	d := time.Duration(0)
	if !start.IsZero() {
		d = time.Since(start)
	}
	// Stamp the trajectory grouping keys from ctx, mirroring kernel.Runtime.observe.
	// tool.execute events fire from tool-job goroutines; without this they carry
	// empty RunID/TurnID and break the per-run grouping invariant consumers rely
	// on to reassemble trajectories.
	ri := hwcloud.RunInfoFromContext(ctx)
	e.cfg.Observer.ObserveStage(ctx, hwcloud.StageEvent{
		Name: stage, Phase: phase, Detail: detail, Duration: d, Err: err,
		RunID: ri.RunID, TurnID: ri.TurnID, ParentRunID: ri.ParentRunID,
	})
}

// withSession injects the session into the tool context so tools and
// hooks can retrieve it via SessionFromContext.
func withSession(ctx context.Context, session hwcloud.Session) context.Context {
	return hwcloud.WithSession(ctx, session)
}

// Runtime is the Execution Runtime interface — the kernel consumes this,
// not the concrete implementation, so applications can substitute their
// own execution layer (e.g. a remote executor).
type Runtime interface {
	Start(ctx context.Context, session hwcloud.Session, call hwcloud.ToolCall, ch chan<- hwcloud.StreamEvent) ExecutionHandle
	Resolve(name string) *ResolvedCall
}

// Compile-time assertion: the concrete runtime implements the interface.
var _ Runtime = (*ExecutionRuntime)(nil)
