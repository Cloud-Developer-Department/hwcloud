package governance

import (
	"encoding/json"
	"testing"
)

// The same semantic args hash identically regardless of key order or
// whitespace — model output is not byte-stable.
func TestApprovalKeyCanonicalizesArgs(t *testing.T) {
	a := ApprovalKey("shell", json.RawMessage(`{"command":"ls -la","timeout":10}`))
	b := ApprovalKey("shell", json.RawMessage(`{"timeout":10,"command":"ls -la"}`))
	c := ApprovalKey("shell", json.RawMessage(`{ "command" : "ls -la" , "timeout" : 10 }`))
	if a != b || b != c {
		t.Fatalf("canonical keys differ: %s / %s / %s", a, b, c)
	}
	// A different tool or a changed argument is a different key.
	if a == ApprovalKey("read", json.RawMessage(`{"command":"ls -la","timeout":10}`)) {
		t.Fatal("tool must be part of the key")
	}
	if a == ApprovalKey("shell", json.RawMessage(`{"command":"ls -la","timeout":11}`)) {
		t.Fatal("changed args must produce a different key")
	}
	// Empty args are stable too.
	if ApprovalKey("ls", nil) != ApprovalKey("ls", json.RawMessage(`{}`)) {
		t.Fatal("empty and {} args must hash identically")
	}
	// Unparseable args fall back to raw bytes deterministically.
	if ApprovalKey("t", json.RawMessage(`{invalid`)) != ApprovalKey("t", json.RawMessage(`{invalid`)) {
		t.Fatal("fallback must be deterministic")
	}
}
