package rest

import (
	"context"
	"log/slog"
	"sort"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// teamAgentMemory splits persistence across agent-private and team-shared
// memory so each agent's internal work (tool calls, tool results) stays
// private while user messages, handoffs, and text output are shared.
//
// Private: keyed by sessionID + "::" + agentName
// Shared:  keyed by sessionID (the team session)
//
// All methods are safe for concurrent use — the underlying stores handle
// that. P2: implements session.SessionStore + session.Compressor +
// provider/memory.MemoryProvider (compressor/provider operate on shared).
type teamAgentMemory struct {
	agentName string
	shared    session.SessionStore
	private   session.SessionStore // same underlying store, different key prefix

	compressor session.Compressor // optional (asserted from shared)
	provider   ctxpkg.MemoryProvider
}

func newTeamAgentMemory(agentName string, shared session.SessionStore) *teamAgentMemory {
	t := &teamAgentMemory{
		agentName: agentName,
		shared:    shared,
		private:   shared, // same store; private ops use prefixed sessionID
	}
	if c, ok := shared.(session.Compressor); ok {
		t.compressor = c
	}
	if p, ok := shared.(ctxpkg.MemoryProvider); ok {
		t.provider = p
	}
	return t
}

// privateKey returns the agent-scoped session key.
func (m *teamAgentMemory) privateKey(sessionID string) string {
	return sessionID + "::" + m.agentName
}

// ── session.SessionStore ──

func (m *teamAgentMemory) Append(ctx context.Context, sessionID string, msg hwcloud.Message) error {
	// Tool results and assistant messages that carry tool_calls are
	// internal agent work — store in private memory.
	if msg.Role == hwcloud.RoleTool {
		return m.private.Append(ctx, m.privateKey(sessionID), msg)
	}
	if msg.Role == hwcloud.RoleAssistant && len(msg.ToolCalls) > 0 {
		return m.private.Append(ctx, m.privateKey(sessionID), msg)
	}
	// Everything else — user messages, text-only assistant output,
	// system messages, handoffs — goes to shared memory.
	return m.shared.Append(ctx, sessionID, msg)
}

func (m *teamAgentMemory) Recent(ctx context.Context, sessionID string, n int, offset int) ([]hwcloud.Message, error) {
	shared, err := m.shared.Recent(ctx, sessionID, n, offset)
	if err != nil {
		slog.Warn("shared memory read failed", "session", sessionID, "error", err)
	}
	priv, err := m.private.Recent(ctx, m.privateKey(sessionID), n, offset)
	if err != nil {
		slog.Warn("private memory read failed", "session", sessionID, "error", err)
	}

	// Concatenate: shared (narrative) first, then private (own work).
	// This gives the agent: "here's the conversation so far, and here's
	// what you did last time." The runner's prefix (tr.runMessages)
	// supplies the rest of the conversation.
	out := make([]hwcloud.Message, 0, len(shared)+len(priv))
	out = append(out, shared...)
	out = append(out, priv...)
	return out, nil
}

// PrivateRecent returns only the agent-private messages.
func (m *teamAgentMemory) PrivateRecent(ctx context.Context, sessionID string, n int, offset int) ([]hwcloud.Message, error) {
	return m.private.Recent(ctx, m.privateKey(sessionID), n, offset)
}

func (m *teamAgentMemory) Count(ctx context.Context, sessionID string) (int, error) {
	sharedN, _ := m.shared.Count(ctx, sessionID)
	privN, _ := m.private.Count(ctx, m.privateKey(sessionID))
	return sharedN + privN, nil
}

// RecentAfter returns the post-summary increment. The summary only ever
// covers the SHARED partition (team Compact routes to the shared key), so
// the private partition starts from 0 — its history is never summarized.
// Like Recent, the two partitions are concatenated with an approximation
// of the merged index.
func (m *teamAgentMemory) RecentAfter(ctx context.Context, sessionID string, throughIndex, n int) ([]hwcloud.Message, error) {
	shared, err := m.shared.RecentAfter(ctx, sessionID, throughIndex, n)
	if err != nil {
		slog.Warn("shared memory read failed", "session", sessionID, "error", err)
	}
	priv, err := m.private.RecentAfter(ctx, m.privateKey(sessionID), 0, n)
	if err != nil {
		slog.Warn("private memory read failed", "session", sessionID, "error", err)
	}
	out := make([]hwcloud.Message, 0, len(shared)+len(priv))
	out = append(out, shared...)
	out = append(out, priv...)
	return out, nil
}

func (m *teamAgentMemory) DeleteSession(ctx context.Context, sessionID string) error {
	return m.private.DeleteSession(ctx, m.privateKey(sessionID))
}

// ── session.Compressor (shared only) ──

func (m *teamAgentMemory) Compact(ctx context.Context, sessionID string, throughIndex int, messages []hwcloud.Message) error {
	if m.compressor == nil {
		return nil
	}
	return m.compressor.Compact(ctx, sessionID, throughIndex, messages)
}

func (m *teamAgentMemory) Compressed(ctx context.Context, sessionID string) (*hwcloud.CompressedContext, error) {
	if m.compressor == nil {
		return nil, nil
	}
	return m.compressor.Compressed(ctx, sessionID)
}

// ── provider/memory.MemoryProvider (shared knowledge) ──

func (m *teamAgentMemory) Recall(ctx context.Context, scope ctxpkg.ContextScope, query string, limit int) ([]ctxpkg.MemoryEntry, error) {
	if m.provider == nil {
		return nil, nil
	}
	// Merge shared + private conversation recall (both keyed scopes).
	sharedScope := scope
	sharedScope.SessionID = scope.SessionID
	entries, err := m.provider.Recall(ctx, sharedScope, query, limit)
	if err != nil {
		return nil, err
	}
	privScope := scope
	privScope.SessionID = m.privateKey(scope.SessionID)
	priv, err := m.provider.Recall(ctx, privScope, query, limit)
	if err != nil {
		// Partial results: shared entries only, but the failure must not
		// stay silent.
		slog.Warn("private knowledge recall failed", "session", scope.SessionID, "error", err)
		return entries, nil
	}
	entries = append(entries, priv...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

func (m *teamAgentMemory) Store(ctx context.Context, scope ctxpkg.ContextScope, item ctxpkg.MemoryItem) error {
	if m.provider == nil {
		return nil
	}
	return m.provider.Store(ctx, scope, item)
}
