package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// loadApprovals reads the approvals map from session meta, tolerating
// both shapes a store can return: the in-process map[string]Decision
// (set directly on an in-memory SessionInfo) and the JSON round-trip
// shape map[string]any (sqlite/file persist meta as JSON, so after any
// Get the values decode as map[string]any and the direct type assertion
// would silently fail — Allow-Always must survive restarts, not just
// in-process use).
func loadApprovals(info *session.SessionInfo) map[string]Decision {
	approvals := map[string]Decision{}
	if info == nil || info.Meta == nil {
		return approvals
	}
	v, ok := info.Meta[approvalsKey]
	if !ok {
		return approvals
	}
	switch t := v.(type) {
	case map[string]Decision:
		return t
	case map[string]any:
		b, err := json.Marshal(t)
		if err != nil {
			return approvals
		}
		if err := json.Unmarshal(b, &approvals); err != nil {
			return approvals
		}
		return approvals
	}
	return approvals
}

// PersistentApprovalMemory persists session-scoped approval decisions
// ("allow always") in the session metadata store, so decisions survive
// server restarts — the Allow-Always contract holds across process
// lifetimes, not just within one run.
//
// Decisions are stored under the session's "_meta.approvals" map, keyed
// by tool name. The in-process fallback (used when no store is wired)
// keeps the same semantics within the process.
type PersistentApprovalMemory struct {
	store    session.Runtime
	fallback ApprovalMemory
	mu       sync.Mutex // serializes read-modify-write on session meta
}

// NewPersistentApprovalMemory creates a store-backed approval memory.
// When store is nil it degrades to an in-process memory (decisions last
// for the process lifetime).
func NewPersistentApprovalMemory(store session.Runtime) *PersistentApprovalMemory {
	return &PersistentApprovalMemory{
		store:    store,
		fallback: NewSessionApprovalMemory(),
	}
}

// approvalsKey is the session metadata key holding the approvals map.
const approvalsKey = "approvals"

// Remember implements ApprovalMemory.
func (m *PersistentApprovalMemory) Remember(ctx context.Context, sessionID, key string, decision Decision) error {
	if m.store == nil {
		return m.fallback.Remember(ctx, sessionID, key, decision)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	info, err := m.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("approval memory: get session: %w", err)
	}
	if info == nil {
		// Session not yet persisted — create a minimal record so the
		// decision has somewhere to live.
		info = &session.SessionInfo{ID: sessionID, Meta: map[string]any{}}
	}

	approvals := loadApprovals(info)
	approvals[key] = decision
	info.SetMeta(approvalsKey, approvals)

	if err := m.store.Save(ctx, *info); err != nil {
		return fmt.Errorf("approval memory: save session: %w", err)
	}
	return nil
}

// Recall implements ApprovalMemory.
func (m *PersistentApprovalMemory) Recall(ctx context.Context, sessionID, key string) (Decision, bool) {
	if m.store == nil {
		return m.fallback.Recall(ctx, sessionID, key)
	}

	info, err := m.store.Get(ctx, sessionID)
	if err != nil || info == nil {
		return Decision{}, false
	}
	approvals := loadApprovals(info)
	d, ok := approvals[key]
	return d, ok
}
