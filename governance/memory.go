package governance

import (
	"context"
	"sync"
)

// SessionApprovalMemory is an in-process, session-scoped approval memory:
// once a decision is remembered for a tool name within a session, the
// policy chain short-circuits to that decision. This fixes the legacy
// "Allow Always does not persist" bug at the session level (P2 persists
// it to the session store).
//
// Safe for concurrent use.
type SessionApprovalMemory struct {
	mu       sync.Mutex
	sessions map[string]map[string]Decision
}

// NewSessionApprovalMemory creates an empty memory.
func NewSessionApprovalMemory() *SessionApprovalMemory {
	return &SessionApprovalMemory{sessions: make(map[string]map[string]Decision)}
}

// Remember stores a decision for a tool key within a session.
func (m *SessionApprovalMemory) Remember(_ context.Context, sessionID, key string, decision Decision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	byKey := m.sessions[sessionID]
	if byKey == nil {
		byKey = make(map[string]Decision)
		m.sessions[sessionID] = byKey
	}
	byKey[key] = decision
	return nil
}

// Recall returns the remembered decision for a tool key, if any.
func (m *SessionApprovalMemory) Recall(_ context.Context, sessionID, key string) (Decision, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byKey, ok := m.sessions[sessionID]
	if !ok {
		return Decision{}, false
	}
	d, ok := byKey[key]
	return d, ok
}

// ForgetSession drops all remembered decisions for a session.
func (m *SessionApprovalMemory) ForgetSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionID)
}
