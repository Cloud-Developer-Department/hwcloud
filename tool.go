package hwcloud

import (
	"context"
	"encoding/json"
)

// FunctionDefinition is the definition of a tool function: name,
// description, and provider-neutral [Parameters]. The Parameters model's
// JSON form IS a JSON Schema, so serializing the FunctionDefinition
// directly yields the provider's schema (OpenAI "parameters", Anthropic
// "input_schema", MCP "inputSchema") with no per-provider conversion.
type FunctionDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  *Parameters `json:"parameters"`

	// EndTurn, when true, tells the runner to end the agent turn loop
	// immediately after executing this tool. Used by handoff tools
	// (transfer_to_*) — aligning with OpenAI Agents SDK's NextStepHandoff.
	EndTurn bool `json:"-"`
}

// Tool represents a callable tool. Both local tools and MCP-imported tools
// implement this interface — the Runner does not distinguish between them.
//
// Execute returns a structured [ToolResult]. Failures are carried in
// Result.Error (structured, with Retryable marking errors the runtime may
// retry and Code for audit) — there is no separate error return; a nil
// result.Error means success. The runner applies the configured
// [ResultPolicy] (truncation) after hooks and before the result enters
// memory. Framework-level problems (tool not found, panic) are handled by
// the runtime and never appear in tool signatures.
type Tool interface {
	Definition() FunctionDefinition
	Execute(ctx context.Context, args json.RawMessage) *ToolResult
}

// ToolStreamChunk is a single chunk of streaming output from a tool that
// implements [StreamExecutor]. Chunks are concatenated to form the final
// tool result; they are also emitted as [StreamToolProgress] events for
// real-time display.
type ToolStreamChunk struct {
	Content string `json:"content"`
	Error   error  `json:"-"`
}

// StreamExecutor is an optional interface for tools that produce streaming
// output during execution. The Runner checks for this interface before
// calling [Tool.Execute]:
//
//	if se, ok := tool.(StreamExecutor); ok {
//	    // streaming path — chunks emitted as StreamToolProgress events
//	} else {
//	    // blocking path — Tool.Execute, no intermediate events
//	}
//
// The chunks returned by ExecuteStream are concatenated to produce the
// final tool result. Tools that implement this interface should close
// the channel when execution is complete.
type StreamExecutor interface {
	ExecuteStream(ctx context.Context, args json.RawMessage) <-chan ToolStreamChunk
}
