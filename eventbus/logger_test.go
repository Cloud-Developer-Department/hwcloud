package eventbus

import (
	"context"
	"testing"
	"time"
)

// TestBusLogger_AppendPublishesToSession verifies BusLogger events reach
// session-scoped subscribers and carry a timestamp.
func TestBusLogger_AppendPublishesToSession(t *testing.T) {
	bus := New[Event](100)
	logger := NewBusLogger(bus)

	sub := bus.Subscribe("sess-1")
	defer bus.Unsubscribe("sess-1", sub)

	if err := logger.Append(context.Background(), Event{
		SessionID: "sess-1",
		Type:      EventToolCall,
		Payload:   "shell",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	select {
	case evt := <-sub.C:
		if evt.Type != EventToolCall {
			t.Fatalf("type = %q, want tool.call", evt.Type)
		}
		if evt.Payload != "shell" {
			t.Fatalf("payload = %v, want shell", evt.Payload)
		}
		if evt.Timestamp.IsZero() {
			t.Fatal("timestamp not set")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive the event")
	}
}

// TestBusLogger_SessionIsolation verifies events are scoped per session.
func TestBusLogger_SessionIsolation(t *testing.T) {
	bus := New[Event](100)
	logger := NewBusLogger(bus)

	sub := bus.Subscribe("sess-a")
	defer bus.Unsubscribe("sess-a", sub)

	_ = logger.Append(context.Background(), Event{SessionID: "sess-b", Type: EventUserInput})
	select {
	case evt := <-sub.C:
		t.Fatalf("session-b event leaked to session-a: %+v", evt)
	case <-time.After(100 * time.Millisecond):
		// expected: no cross-session delivery
	}
}
