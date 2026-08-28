//go:build embed

// Package skills embeds the built-in skill directory tree into the binary
// when built with -tags embed. Without that tag, embed_stub.go provides a
// nil-returning BuiltinFS so the rest of the code compiles unchanged.
package skills

import (
	"embed"
	"io/fs"
)

//go:embed builtin
var builtinSkillsFS embed.FS

// BuiltinFS returns the embedded built-in skills as an fs.FS (each top-level
// directory under builtin/ is one skill). Returns nil when the binary was
// not built with -tags embed.
func BuiltinFS() fs.FS {
	sub, err := fs.Sub(builtinSkillsFS, "builtin")
	if err != nil {
		return nil
	}
	return sub
}
