// Package eventbus provides a generic session-scoped pub/sub event bus
// with history replay for late subscribers.
//
// Each session has its own topic. Multiple subscribers can independently
// receive the full event stream — unlike a Go channel which only delivers
// each value to a single receiver.
//
// Usage:
//
//	bus := eventbus.New[MyEvent](500) // max 500 history events per session
//
//	// Publisher:
//	bus.Publish(sessionID, evt)
//
//	// Subscriber (e.g. SSE connection):
//	sub := bus.Subscribe(sessionID)
//	defer bus.Unsubscribe(sessionID, sub)
//	for evt := range sub.C {
//	    // handle event
//	}
package eventbus

import (
	"sync"
	"sync/atomic"
)

// Bus is a session-scoped publish/subscribe event bus.
// T is the event type; typically a struct carrying type+payload.
type Bus[T any] struct {
	mu         sync.RWMutex
	sessions   map[string]*topic[T]
	maxHistory int
}

// Subscription represents a single subscriber's connection to a topic.
// Read events from C. When C is closed the subscription has been removed.
type Subscription[T any] struct {
	C      <-chan T
	ch     chan T
	t      *topic[T]
	closed atomic.Bool // ensure Close is called at most once
}

// subscriptionBuffer is the per-subscriber channel size. Publish is
// non-blocking (the producer must never stall on a slow consumer), so a
// larger buffer reduces drops under bursty traffic while keeping memory
// bounded. Drops are still possible for a consumer slower than the
// stream — acceptable for display events. Audit events go through the
// same in-process bus today: the current Logger (BusLogger) is
// memory-only and drops under load; a durable sink (file/DB) is not yet
// implemented, so audit history is NOT guaranteed durable.
const subscriptionBuffer = 256

// New creates a Bus with per-session history capped at maxHistory events.
// maxHistory controls the replay buffer size: when a new subscriber joins,
// it immediately receives up to maxHistory past events before new ones.
// Set to 0 to disable history replay.
func New[T any](maxHistory int) *Bus[T] {
	if maxHistory < 0 {
		maxHistory = 0
	}
	return &Bus[T]{
		sessions:   make(map[string]*topic[T]),
		maxHistory: maxHistory,
	}
}

// Subscribe returns a new subscription for the given session.
// If the session doesn't exist it is created.
//
// The subscriber immediately receives buffered history events (up to
// maxHistory) on its channel, then live events as they are published.
// Call [Bus.Unsubscribe] when done to release resources.
func (b *Bus[T]) Subscribe(sessionID string) *Subscription[T] {
	b.mu.Lock()
	t, ok := b.sessions[sessionID]
	if !ok {
		t = &topic[T]{maxHistory: b.maxHistory}
		b.sessions[sessionID] = t
	}
	b.mu.Unlock()

	return t.subscribe()
}

// SubscribeLive is like [Bus.Subscribe] but does NOT replay history.
// The subscriber only receives events published after the subscription is created.
// Use when another mechanism (e.g. persistent storage) handles history replay.
func (b *Bus[T]) SubscribeLive(sessionID string) *Subscription[T] {
	b.mu.Lock()
	t, ok := b.sessions[sessionID]
	if !ok {
		t = &topic[T]{maxHistory: b.maxHistory}
		b.sessions[sessionID] = t
	}
	b.mu.Unlock()

	return t.subscribeLive()
}

// RemoveTopic removes a session's topic entirely, closing all subscriber
// channels. Call when a session is deleted — no further events will be sent
// and existing SSE connections will receive channel closure (EOF).
// Safe to call on sessions that don't exist (no-op).
func (b *Bus[T]) RemoveTopic(sessionID string) {
	b.mu.Lock()
	t, ok := b.sessions[sessionID]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.sessions, sessionID)
	b.mu.Unlock()

	// Close all subscriber channels. Publish will no longer find this
	// topic, so no new sends can arrive.
	t.mu.Lock()
	for _, sub := range t.subs {
		if sub.closed.CompareAndSwap(false, true) {
			close(sub.ch)
		}
	}
	t.mu.Unlock()
}

// Unsubscribe removes the subscription and closes its channel.
// It is safe to call multiple times on the same subscription (no-op after first).
func (b *Bus[T]) Unsubscribe(sessionID string, sub *Subscription[T]) {
	if sub == nil {
		return
	}
	if !sub.closed.CompareAndSwap(false, true) {
		return // already unsubscribed
	}

	sub.t.remove(sub)

	// Close the channel to signal EOF to the reader.
	// We must be certain no concurrent Publish is trying to send on it.
	// remove() already took the topic lock and removed sub from the list,
	// so Publish will no longer see this channel. Safe to close.
	close(sub.ch)
}

// HistoryLen returns the number of buffered history events for a session.
// Returns 0 if the session doesn't exist or has no history yet.
// Useful for detecting whether a subscriber is the first after a restart.
func (b *Bus[T]) HistoryLen(sessionID string) int {
	b.mu.RLock()
	t, ok := b.sessions[sessionID]
	b.mu.RUnlock()
	if !ok {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.history)
}

// Publish sends evt to all current subscribers of the session.
// The event is appended to the session's history buffer (capped at maxHistory).
// Creates the topic if it doesn't exist yet so events are never lost —
// late subscribers receive them via history replay.
// Slow subscribers may miss events (non-blocking send).
func (b *Bus[T]) Publish(sessionID string, evt T) {
	b.mu.RLock()
	t, ok := b.sessions[sessionID]
	b.mu.RUnlock()
	if ok {
		t.publish(evt)
		return
	}

	// Topic doesn't exist yet — create it so the event is stored in history
	// for when a subscriber connects.
	b.mu.Lock()
	t, ok = b.sessions[sessionID]
	if !ok {
		t = &topic[T]{maxHistory: b.maxHistory}
		b.sessions[sessionID] = t
	}
	b.mu.Unlock()
	t.publish(evt)
}

// ── topic ──

type topic[T any] struct {
	mu         sync.Mutex
	subs       []*Subscription[T]
	history    []T
	maxHistory int
}

func (t *topic[T]) subscribeLive() *Subscription[T] {
	t.mu.Lock()
	defer t.mu.Unlock()

	ch := make(chan T, subscriptionBuffer)
	sub := &Subscription[T]{
		C:  ch,
		ch: ch,
		t:  t,
	}
	// No history replay — subscriber only gets future events.
	t.subs = append(t.subs, sub)
	return sub
}

func (t *topic[T]) subscribe() *Subscription[T] {
	t.mu.Lock()
	defer t.mu.Unlock()

	// FULL history replay: size the channel to fit the entire buffered
	// history, then replay everything in order. A partial replay would
	// drop the NEWEST events (the channel fills from the oldest first),
	// handing late subscribers a stale view that can never be corrected.
	cap := subscriptionBuffer
	if len(t.history) > cap {
		cap = len(t.history)
	}
	ch := make(chan T, cap)
	sub := &Subscription[T]{
		C:  ch,
		ch: ch,
		t:  t,
	}

	// The channel is sized to hold the whole history — this send never
	// blocks (publish holds the topic lock, so no concurrent writers).
	for _, evt := range t.history {
		ch <- evt
	}

	t.subs = append(t.subs, sub)
	return sub
}

func (t *topic[T]) remove(sub *Subscription[T]) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for i, s := range t.subs {
		if s == sub {
			t.subs = append(t.subs[:i], t.subs[i+1:]...)
			break
		}
	}
}

func (t *topic[T]) publish(evt T) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Append to history, cap if needed.
	if t.maxHistory > 0 {
		t.history = append(t.history, evt)
		if len(t.history) > t.maxHistory {
			// Drop oldest to stay within limit.
			n := len(t.history) - t.maxHistory
			t.history = t.history[n:]
		}
	}

	// Fan out to all subscribers (non-blocking).
	for _, sub := range t.subs {
		select {
		case sub.ch <- evt:
		default:
			// Subscriber too slow — drop event for this subscriber.
		}
	}
}
