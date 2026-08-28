package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ApprovalKey derives the memory key for a tool call: the tool name plus
// a hash of its normalized arguments. Same tool + same args = same key —
// that is the unit of an allow/always decision (Claude Code semantics:
// a changed argument is a different operation and asks again). The args
// are hashed, never stored in plaintext in memory keys.
func ApprovalKey(tool string, args json.RawMessage) string {
	return tool + ":" + hex.EncodeToString(sha256Sum(normalizeArgs(args)))
}

// normalizeArgs canonicalizes raw JSON args so semantically identical
// calls hash identically regardless of key order or whitespace (model
// output is not guaranteed to be byte-stable). A parse failure falls
// back to the raw bytes — the call still gets a deterministic key.
func normalizeArgs(args json.RawMessage) []byte {
	if len(args) == 0 {
		return []byte("{}")
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return args
	}
	// Marshal of a decoded value sorts map keys (encoding/json) — the
	// canonical form for hashing.
	canon, err := json.Marshal(v)
	if err != nil {
		return args
	}
	return canon
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
