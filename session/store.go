package session

import "context"

// Store persists session metadata (SessionInfo) across server restarts.
// Implementations must be safe for concurrent use.
type Store interface {
	// Save upserts session metadata.
	Save(ctx context.Context, info SessionInfo) error

	// Get returns the session metadata, or (nil, nil) if not found.
	Get(ctx context.Context, id string) (*SessionInfo, error)

	// List returns all sessions. The caller may filter by Meta fields.
	List(ctx context.Context) ([]SessionInfo, error)

	// Delete removes session metadata. No-op if the session doesn't exist.
	Delete(ctx context.Context, id string) error

	// Close releases resources held by the store.
	Close() error
}
