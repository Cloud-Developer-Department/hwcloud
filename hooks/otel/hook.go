// Package otel is an hwcloud built-in RunHooks + RunObserver implementation
// that traces the agent lifecycle with OpenTelemetry.
//
// RunHooks axis (hook.go):
//   - Each agent run becomes an "agent.run" span; each tool call a
//     "tool.<name>" child span with args, result length, truncation file
//     refs, and error status.
//
// RunObserver axis (observer.go):
//   - Each of the 8 loop stages (memory.fetch, guard.in, prompt.build,
//     model.call, guard.out, tool.execute, memory.append) becomes a child
//     span under "agent.run", so the trace shows the full per-turn trajectory.
//
// Attributes follow the OTel GenAI semantic conventions (gen_ai.*) where they
// apply, so AI-native backends (Langfuse, Phoenix, Arize) auto-render token
// usage and model panels. Agent-specific attrs (agent.turns, tool.args) use
// the agent.* / tool.* namespace.
//
// Usage (wire via kernel.Deps):
//
//	tracer := otel.GetTracerProvider().Tracer("hwcloud")
//	deps := kernel.Deps{
//	    Hooks:    otelhooks.New(tracer),
//	    Observer: otelhooks.NewObserver(tracer),
//	    ...
//	}
//
// Combine with hooks/slog for logs and hooks/redact for secret masking
// (redact FIRST in the MultiHooks chain).
package otel

import (
	"context"
	"encoding/json"
	"fmt"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/version"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Hooks implements hwcloud.RunHooks via OpenTelemetry spans.
type Hooks struct {
	tracer trace.Tracer
}

// New creates a Hooks that creates spans with the given tracer.
func New(tracer trace.Tracer) *Hooks {
	return &Hooks{tracer: tracer}
}

func (h *Hooks) OnAgentStart(ctx context.Context, req hwcloud.ChatCompletionRequest) (context.Context, any, error) {
	ctx, span := h.tracer.Start(ctx, "agent.run",
		trace.WithAttributes(
			// OTel GenAI semantic conventions (gen_ai.*).
			attribute.String("gen_ai.system", version.Name),
			attribute.String("gen_ai.request.model", req.Model),
			// Agent-specific attributes.
			attribute.String("agent.model", req.Model),
			attribute.Int("agent.messages", len(req.Messages)),
			attribute.Int("agent.tools", len(req.Tools)),
			// Binary identity for filtering traces by build.
			attribute.String("service.name", version.Name),
			attribute.String("service.version", version.Version),
		),
	)
	// Return the enriched ctx so the kernel threads it to OnToolStart —
	// child spans then attach to this agent.run span (standard OTel
	// context propagation).
	return ctx, span, nil
}

func (h *Hooks) OnAgentEnd(ctx context.Context, req hwcloud.ChatCompletionRequest, resp *hwcloud.ChatCompletionResponse, runErr error, startState any) {
	span, _ := startState.(trace.Span)
	if span == nil {
		return
	}
	defer span.End()

	if resp != nil {
		// GenAI usage attributes — recognized by Langfuse/Phoenix/Arize.
		span.SetAttributes(
			attribute.Int("gen_ai.usage.input_tokens", resp.Usage.PromptTokens),
			attribute.Int("gen_ai.usage.output_tokens", resp.Usage.CompletionTokens),
			attribute.Int("gen_ai.usage.total_tokens", resp.Usage.TotalTokens),
			// Agent-specific aliases.
			attribute.Int("agent.prompt_tokens", resp.Usage.PromptTokens),
			attribute.Int("agent.completion_tokens", resp.Usage.CompletionTokens),
			attribute.Int("agent.total_tokens", resp.Usage.TotalTokens),
		)
	}
	if runErr != nil {
		span.SetStatus(codes.Error, runErr.Error())
		span.RecordError(runErr)
	}
}

func (h *Hooks) OnToolStart(ctx context.Context, tool hwcloud.FunctionDefinition, args json.RawMessage) (context.Context, any, error) {
	ctx, span := h.tracer.Start(ctx, fmt.Sprintf("tool.%s", tool.Name),
		trace.WithAttributes(
			attribute.String("tool.name", tool.Name),
			attribute.String("tool.args", string(args)),
		),
	)
	// Return the enriched ctx so the tool execution and stage observer
	// spans attach to this tool span.
	return ctx, span, nil
}

func (h *Hooks) OnToolEnd(ctx context.Context, tool hwcloud.FunctionDefinition, args json.RawMessage, result *hwcloud.ToolResult, startState any) {
	span, _ := startState.(trace.Span)
	if span == nil {
		return
	}
	defer span.End()

	if result != nil && result.Error != nil {
		span.SetStatus(codes.Error, result.Error.Message)
		span.RecordError(result.AsError())
	}
	if result != nil {
		span.SetAttributes(attribute.Int("tool.result_len", len(result.Content)))
		if result.Truncated {
			span.SetAttributes(attribute.String("tool.file_ref", result.FileRef))
		}
	}
}

var _ hwcloud.RunHooks = (*Hooks)(nil)
