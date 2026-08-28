package hwcloud

// ── Run result ──

// RunResult is the output of an Agent.Run call.
type RunResult struct {
	Messages      []Message // all messages from this run
	FinalOutput   string    // last assistant message content
	TurnCount     int
	Usage         Usage  // total tokens used
	ContextWindow int    // model's context window size (0 if unknown)
	StopReason    string // "end_turn", "refusal", "cancelled", etc. (ACP agents)
	RunID         string // trajectory grouping key — joins this result with its StageEvent/DecisionEvent stream
	ParentRunID   string // #1: the enclosing run's RunID (team/orchestrator); empty for solo runs
}

// ── Stream events ──

// StreamEventType categorizes events emitted by RunStream.
type StreamEventType string

const (
	StreamThought      StreamEventType = "thought" // reasoning content (o1, deepseek-r1)
	StreamTextDelta    StreamEventType = "text_delta"
	StreamToolCall     StreamEventType = "tool_call"
	StreamToolProgress StreamEventType = "tool_progress" // streaming tool output chunk
	StreamToolResult   StreamEventType = "tool_result"
	StreamRetrying     StreamEventType = "retrying"
	StreamDone         StreamEventType = "done"
	StreamError        StreamEventType = "error"
	StreamAborted      StreamEventType = "aborted" // context cancelled, deadline exceeded
)

// StreamEvent is emitted by RunStream for real-time rendering.
type StreamEvent struct {
	Type       StreamEventType
	Text       string     // text_delta, tool_progress
	Message    Message    // tool_call, tool_result
	Result     *RunResult // done
	Error      error      // error, retrying
	ToolCallID string     // tool_progress — disambiguates concurrent streaming tools
}
