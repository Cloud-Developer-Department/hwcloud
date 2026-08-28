package eventbus

import (
	"context"
	"time"
)

// EventType names the auditable lifecycle events the runtime logs.
type EventType string

const (
	EventUserInput        EventType = "user.input"
	EventAssistantMessage EventType = "assistant.message"
	EventToolCall         EventType = "tool.call"
	EventToolResult       EventType = "tool.result"
	EventApprovalRequest  EventType = "approval.request"
	EventApprovalResult   EventType = "approval.result"
)

// Event is an auditable runtime event. Events are NOT state — they are
// Audit / Observability / Replay aids; the conversation store is the
// source of truth. Payload is intentionally loose (event consumers
// type-switch), Metadata carries key/value context.
//
// RunID/TurnID are stamped from ctx RunInfo by the logEvent helper so a future
// consumer can join an eventbus Event to the DecisionEvent stream via the
// (session_id, run_id, turn_id) prefix — call_id stays in Metadata["call_id"].
// Today the eventbus is in-memory only with zero consumers (BusLogger keeps
// a bounded history for session-scoped replay and nothing subscribes); the
// fields are join-ready for when a durable sink is wired.
type Event struct {
	SessionID string            `json:"session_id"`
	Type      EventType         `json:"type"`
	Timestamp time.Time         `json:"timestamp"`
	Payload   any               `json:"payload,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	RunID     string            `json:"run_id,omitempty"`
	TurnID    int               `json:"turn_id,omitempty"`
}

// Logger appends audit events. Implementations may publish to an
// in-process bus, persist to disk, or forward to a remote sink.
type Logger interface {
	Append(ctx context.Context, evt Event) error
}

// BusLogger publishes events to a session-scoped [Bus] (in-process fan-out
// with history replay). Events are lost on process exit — persist to a
// durable sink for long-term audit.
type BusLogger struct {
	bus *Bus[Event]
}

// NewBusLogger creates a logger backed by the given bus.
func NewBusLogger(bus *Bus[Event]) *BusLogger {
	return &BusLogger{bus: bus}
}

// Append implements Logger.
func (l *BusLogger) Append(_ context.Context, evt Event) error {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}
	l.bus.Publish(evt.SessionID, evt)
	return nil
}
