//go:build !embed

// Package skills provides BuiltinFS. Without -tags embed this stub returns
// nil so no embedded skills are available — the agent behaves exactly as it
// would without this package (skills loaded from disk only).
package skills

import "io/fs"

// BuiltinFS returns nil when the binary was not built with -tags embed.
func BuiltinFS() fs.FS { return nil }
