// Package slog provides hwcloud's built-in log/slog implementations of the
// two observation axes. Both are zero-external-dependency (standard library
// only) and share a single *slog.Logger so one handler config governs the
// whole run's logging:
//
//   - Hooks: hwcloud.RunHooks — the lifecycle axis. Logs agent start/end
//     (model, token usage, duration, errors) and tool start/end. Combine with
//     hooks/redact (redact FIRST) to keep secrets out of logs.
//   - Observer: hwcloud.RunObserver + hwcloud.DecisionObserver — the
//     observation axis. Logs stage boundaries (enter/leave, with run/turn
//     join keys) and decision events (layer + outcome, level by outcome).
//
// The two types are independent: wire one or both via kernel.Deps. Hooks
// fires on agent/tool lifecycle transitions; Observer fires on stage
// boundaries and per-decision events — neither subsumes the other.
//
// Usage (wire both via kernel.Deps):
//
//	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
//	deps := kernel.Deps{
//	    Hooks:    hwcloud.MultiHooks(redacthook.NewHook(envNames), sloghooks.New(logger)),
//	    Observer: sloghooks.NewObserver(logger),
//	    ...
//	}
//	rt := kernel.New(cfg, deps)
//
// In ACP/HTTP/run CLI modes the observer is backed by slog.Default() — a
// rotated file or io.Discard, NEVER stderr (stderr is the ACP control pipe).
package slog

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// Hooks implements hwcloud.RunHooks via log/slog.
type Hooks struct {
	logger *slog.Logger
}

// New creates a Hooks that logs to the given slog.Logger.
func New(logger *slog.Logger) *Hooks {
	return &Hooks{logger: logger}
}

func (h *Hooks) OnAgentStart(ctx context.Context, req hwcloud.ChatCompletionRequest) (context.Context, any, error) {
	h.logger.InfoContext(ctx, "agent start",
		"model", req.Model,
		"messages", len(req.Messages),
		"tools", len(req.Tools),
	)
	return ctx, time.Now(), nil
}

func (h *Hooks) OnToolStart(ctx context.Context, tool hwcloud.FunctionDefinition, args json.RawMessage) (context.Context, any, error) {
	h.logger.DebugContext(ctx, "tool start",
		"tool", tool.Name,
		"args", string(args), // full arguments — debug level is for investigation
	)
	return ctx, time.Now(), nil
}

func (h *Hooks) OnAgentEnd(ctx context.Context, req hwcloud.ChatCompletionRequest, resp *hwcloud.ChatCompletionResponse, runErr error, startState any) {
	t0, _ := startState.(time.Time)
	elapsed := time.Since(t0)

	attrs := []slog.Attr{
		slog.String("model", req.Model),
		slog.Duration("elapsed", elapsed),
	}
	if resp != nil {
		attrs = append(attrs,
			slog.Int("prompt_tokens", resp.Usage.PromptTokens),
			slog.Int("completion_tokens", resp.Usage.CompletionTokens),
			slog.Int("total_tokens", resp.Usage.TotalTokens),
		)
	}
	if runErr != nil {
		attrs = append(attrs, slog.String("error", runErr.Error()))
		h.logger.LogAttrs(ctx, slog.LevelError, "agent end", attrs...)
	} else {
		h.logger.LogAttrs(ctx, slog.LevelInfo, "agent end", attrs...)
	}
}

func (h *Hooks) OnToolEnd(ctx context.Context, tool hwcloud.FunctionDefinition, args json.RawMessage, result *hwcloud.ToolResult, startState any) {
	t0, _ := startState.(time.Time)
	elapsed := time.Since(t0)

	attrs := []slog.Attr{
		slog.String("tool", tool.Name),
		slog.Duration("elapsed", elapsed),
	}
	if result != nil && result.Error != nil {
		attrs = append(attrs, slog.String("error", result.Error.Message))
		h.logger.LogAttrs(ctx, slog.LevelError, "tool end", attrs...)
	} else if result != nil {
		attrs = append(attrs, slog.Int("result_len", len(result.Content)))
		if result.Truncated {
			attrs = append(attrs, slog.String("file_ref", result.FileRef))
		}
		h.logger.LogAttrs(ctx, slog.LevelDebug, "tool end", attrs...)
	}
}

var _ hwcloud.RunHooks = (*Hooks)(nil)
