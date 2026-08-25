package context

import (
	"context"
)

// MemoryProvider stores and retrieves durable knowledge (preferences,
// project facts, lessons). It is the long-term layer of the context
// architecture — distinct from short-term session storage
// (session.SessionStore) and compression (session.Compressor).
//
// OpenViking or any enterprise knowledge base plugs in by implementing
// this interface. A nil provider means no long-term memory: recall tools
// are not injected and no knowledge is stored.
type MemoryProvider interface {
	// Recall searches durable knowledge relevant to query within the
	// given scope. Results are best-effort ranked (higher Score = more
	// relevant). When scope.SessionID is set, implementations may narrow
	// the search to that session's archive.
	Recall(ctx context.Context, scope ContextScope, query string, limit int) ([]MemoryEntry, error)

	// Store persists a knowledge item under the given scope.
	Store(ctx context.Context, scope ContextScope, item MemoryItem) error
}
