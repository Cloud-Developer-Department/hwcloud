package context

import (
	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// CompressorOf returns store as a session.Compressor when it implements
// one (sqlite/file message stores do). Nil otherwise. Centralizes the
// optional-capability assertion so callers don't each re-implement it.
func CompressorOf(store session.SessionStore) session.Compressor {
	if c, ok := store.(session.Compressor); ok {
		return c
	}
	return nil
}
