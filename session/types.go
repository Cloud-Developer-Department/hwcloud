// Package session provides session metadata types and persistent storage.
//
// SessionInfo follows the standard session definition: an ID, a working
// directory, a title, timestamps, optional additional directories, and
// an extensible metadata map.
//
// Store is the abstract interface for session metadata persistence.
// Implementations exist for SQLite (session/sqlite) and file (session/file).
package session

import "time"

// SessionInfo is the public representation of a conversation session.
type SessionInfo struct {
	ID                    string         `json:"sessionId"`
	Cwd                   string         `json:"cwd"`
	Title                 string         `json:"title,omitempty"`
	CreatedAt             time.Time      `json:"createdAt"`
	UpdatedAt             time.Time      `json:"updatedAt"`
	AdditionalDirectories []string       `json:"additionalDirectories,omitempty"`
	Meta                  map[string]any `json:"_meta,omitempty"`
}

// GetMeta retrieves a value from Meta. Returns the value and true if the
// key exists and matches type T.
func GetMeta[T any](s SessionInfo, key string) (T, bool) {
	if s.Meta == nil {
		var zero T
		return zero, false
	}
	v, ok := s.Meta[key].(T)
	return v, ok
}

// SetMeta sets a key-value pair in Meta. If Meta is nil it is initialised.
func (s *SessionInfo) SetMeta(key string, value any) {
	if s.Meta == nil {
		s.Meta = make(map[string]any)
	}
	s.Meta[key] = value
}
