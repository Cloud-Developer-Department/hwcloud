package kernel

import (
	"context"
	"log/slog"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// cancelCompensation persists unresolved tool results when the run is
// cancelled mid-execution, so a restarted session sees what was in flight.
// It uses context.Background() deliberately — for BOTH the store write and
// the event sends: the run context is cancelled, and if the events were
// sent with it, chSend's select would randomly drop them (ctx.Done() is
// already closed and races the buffered send), so the client would never
// see the compensation or the terminal aborted event.
func (rt *Runtime) cancelCompensation(ctx context.Context, session hwcloud.Session, workingMessages []hwcloud.Message, ch chan<- hwcloud.StreamEvent) {
	// Event sends carry a bounded deadline instead of an unbounded
	// background send: the consumer goroutine is NOT guaranteed to be
	// alive — REST returns its drain loop on r.Context().Done() (client
	// disconnect) before the run finishes cancelling, and a full channel
	// with a dead consumer would block forever. The store write below is
	// the durable contract; the events are best-effort on top.
	sendComp := func(ev hwcloud.StreamEvent) {
		if ch == nil {
			return
		}
		select {
		case ch <- ev:
		case <-time.After(5 * time.Second):
		}
	}
	if rt.deps.SessionStore == nil {
		sendComp(hwcloud.StreamEvent{Type: hwcloud.StreamAborted, Error: ctx.Err()})
		return
	}
	// Find assistant tool_calls in the working set whose results are missing
	// (RoleTool with a matching ToolCallID) — those were interrupted.
	covered := make(map[string]bool)
	for _, m := range workingMessages {
		if m.Role == hwcloud.RoleTool {
			covered[m.ToolCallID] = true
		}
	}
	for _, m := range workingMessages {
		if m.Role != hwcloud.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if covered[tc.ID] {
				continue
			}
			msg := hwcloud.Message{
				Role:       hwcloud.RoleTool,
				ToolCallID: tc.ID,
				Content:    "cancelled by user",
			}
			// commit() stamps CreatedAt for normal appends; this path
			// bypasses commit (it calls Append directly with a background
			// ctx), so stamp here too — cancelled tool results deserve a
			// wall-clock for history display just like committed ones.
			if msg.CreatedAt == nil {
				now := time.Now().UTC()
				msg.CreatedAt = &now
			}
			start := time.Now()
			rt.observe(context.Background(), hwcloud.StageMemoryAppend, "enter", nil, time.Time{}, nil)
			err := rt.deps.SessionStore.Append(context.Background(), session.ID, msg)
			rt.observe(context.Background(), hwcloud.StageMemoryAppend, "leave", map[string]any{
				"role":  string(msg.Role),
				"chars": len([]rune(msg.Content)),
			}, start, err)
			if err != nil {
				slog.Error("cancel compensation append failed", "error", err)
			}
			sendComp(hwcloud.StreamEvent{Type: hwcloud.StreamToolResult, Message: msg})
			covered[tc.ID] = true
		}
	}
	sendComp(hwcloud.StreamEvent{Type: hwcloud.StreamAborted, Error: ctx.Err()})
}
