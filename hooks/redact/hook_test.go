package redact

import (
	"context"
	"strings"
	"sync"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// withEnv sets env vars for a test; t.Setenv handles save/restore.
func withEnv(t *testing.T, kv ...string) {
	t.Helper()
	if len(kv)%2 != 0 {
		t.Fatal("withEnv: odd number of kv args")
	}
	for i := 0; i < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

func TestOnToolEnd_NoEnvNames_NoOp(t *testing.T) {
	h := NewHook(nil)
	out := "secret: abc"
	res := &hwcloud.ToolResult{Content: out}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if res.Content != out {
		t.Fatalf("nil envNames modified result: %q", res.Content)
	}

	h2 := NewHook([]string{})
	res2 := &hwcloud.ToolResult{Content: out}
	h2.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res2, nil)
	if res2.Content != out {
		t.Fatalf("empty envNames modified result: %q", res2.Content)
	}
}

func TestOnToolEnd_EmptyEnvValue_Skipped(t *testing.T) {
	// An unset/empty env var must be skipped — redacting "" would insert
	// "[REDACTED]" between every rune and corrupt all output.
	withEnv(t, "REDACT_EMPTY", "")
	h := NewHook([]string{"REDACT_EMPTY"})
	out := "nothing to see here"
	res := &hwcloud.ToolResult{Content: out}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if res.Content != out {
		t.Fatalf("empty-valued env var corrupted result: %q", res.Content)
	}
}

func TestOnToolEnd_EmptyResult_ShortCircuit(t *testing.T) {
	// Empty result must not enter the redaction loop.
	withEnv(t, "REDACT_ER", "xsecret1")
	h := NewHook([]string{"REDACT_ER"})
	res := &hwcloud.ToolResult{Content: ""}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if res.Content != "" {
		t.Fatalf("empty result modified: %q", res.Content)
	}
}

func TestOnToolEnd_SingleHit_RedactedWithHint(t *testing.T) {
	withEnv(t, "REDACT_TOKEN", "supersecret")
	h := NewHook([]string{"REDACT_TOKEN"})
	res := &hwcloud.ToolResult{Content: "the token is supersecret and that is bad"}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if strings.Contains(res.Content, "supersecret") {
		t.Fatalf("secret leaked in result: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[REDACTED]") {
		t.Fatalf("result missing [REDACTED]: %q", res.Content)
	}
	if !strings.Contains(res.Content, hint) {
		t.Fatalf("result missing hint: %q", res.Content)
	}
}

func TestOnToolEnd_MultipleOccurrences_AllReplaced(t *testing.T) {
	withEnv(t, "REDACT_TOK", "abcdef12")
	h := NewHook([]string{"REDACT_TOK"})
	res := &hwcloud.ToolResult{Content: "abcdef12 and abcdef12 and again abcdef12"}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	want := strings.ReplaceAll("abcdef12 and abcdef12 and again abcdef12", "abcdef12", "[REDACTED]") + hint
	if res.Content != want {
		t.Fatalf("got %q want %q", res.Content, want)
	}
}

func TestOnToolEnd_MultipleSecrets_HintAppendedOnce(t *testing.T) {
	withEnv(t, "REDACT_A", "AAAAsecret", "REDACT_B", "BBBBsecret")
	h := NewHook([]string{"REDACT_A", "REDACT_B"})
	res := &hwcloud.ToolResult{Content: "AAAAsecret here, BBBBsecret there, AAAAsecret again"}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if strings.Contains(res.Content, "AAAAsecret") || strings.Contains(res.Content, "BBBBsecret") {
		t.Fatalf("secret leaked: %q", res.Content)
	}
	if c := strings.Count(res.Content, hint); c != 1 {
		t.Fatalf("hint count = %d, want 1: %q", c, res.Content)
	}
}

func TestOnToolEnd_NoHit_Unchanged(t *testing.T) {
	withEnv(t, "REDACT_X", "xyzsecret")
	h := NewHook([]string{"REDACT_X"})
	out := "completely unrelated output"
	res := &hwcloud.ToolResult{Content: out}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if res.Content != out {
		t.Fatalf("no-hit result modified: %q", res.Content)
	}
}

func TestOnToolEnd_NilResult_NoPanic(t *testing.T) {
	withEnv(t, "REDACT_N", "nosecret")
	h := NewHook([]string{"REDACT_N"})
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, nil, nil)
}

func TestOnToolEnd_ErrorString_Redacted(t *testing.T) {
	withEnv(t, "REDACT_E", "leaked-in-error")
	h := NewHook([]string{"REDACT_E"})
	res := &hwcloud.ToolResult{Error: &hwcloud.ToolError{Message: "tool failed: leaked-in-error"}}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if res.Error == nil {
		t.Fatal("error became nil")
	}
	if strings.Contains(res.Error.Message, "leaked-in-error") {
		t.Fatalf("secret leaked in error: %q", res.Error.Message)
	}
	if !strings.Contains(res.Error.Message, "[REDACTED]") {
		t.Fatalf("error missing [REDACTED]: %q", res.Error.Message)
	}
}

func TestOnToolEnd_LazyResolution_EnvSetAfterConstruction(t *testing.T) {
	// Hook constructed before the env var exists; value set after must
	// still be honored at redaction time (lazy os.Getenv).
	t.Setenv("REDACT_LATE", "")
	h := NewHook([]string{"REDACT_LATE"})
	t.Setenv("REDACT_LATE", "late-value")
	res := &hwcloud.ToolResult{Content: "contains late-value here"}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if strings.Contains(res.Content, "late-value") {
		t.Fatalf("lazily-resolved secret leaked: %q", res.Content)
	}
}

func TestOnToolEnd_JSONResult_NoTrailingHint(t *testing.T) {
	// A JSON result must stay valid JSON after redaction — no trailing
	// hint appended.
	withEnv(t, "REDACT_J", "supersecret")
	h := NewHook([]string{"REDACT_J"})
	res := &hwcloud.ToolResult{Content: `{"token":"supersecret","ok":true}`}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if strings.Contains(res.Content, "supersecret") {
		t.Fatalf("secret leaked: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[REDACTED]") {
		t.Fatalf("missing [REDACTED]: %q", res.Content)
	}
	if strings.Contains(res.Content, hint) {
		t.Fatalf("hint appended to JSON result, breaking validity: %q", res.Content)
	}
}

func TestOnToolEnd_HintIdempotent_AlreadyHinted(t *testing.T) {
	// If the result already carries a hint (e.g. reprocessed), don't
	// stack a second one.
	withEnv(t, "REDACT_I", "secsecret")
	h := NewHook([]string{"REDACT_I"})
	res := &hwcloud.ToolResult{Content: "secsecret here" + hint}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if c := strings.Count(res.Content, hint); c != 1 {
		t.Fatalf("hint count = %d, want 1 (idempotent): %q", c, res.Content)
	}
}

func TestNewHook_DedupAndDropEmpty(t *testing.T) {
	// duplicate and empty names are collapsed.
	withEnv(t, "REDACT_D", "sensitive-dedup-token")
	h := NewHook([]string{"REDACT_D", "REDACT_D", "", "REDACT_D"})
	res := &hwcloud.ToolResult{Content: "sensitive-dedup-token"}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if strings.Contains(res.Content, "sensitive-dedup-token") {
		t.Fatalf("secret leaked despite dup/empty names: %q", res.Content)
	}
	// Functional check: envNames deduped to 1, no panic, no double hint.
	if c := strings.Count(res.Content, hint); c != 1 {
		t.Fatalf("hint count = %d, want 1: %q", c, res.Content)
	}
}

func TestOnToolEnd_ConcurrentSafe(t *testing.T) {
	// multiple goroutines using the same Hook must be safe.
	// Hook.envNames is read-only and os.Getenv is concurrency-safe; this
	// test pins the invariant so future mutable state (e.g. regex cache)
	// won't silently introduce a data race.
	withEnv(t, "REDACT_C", "concurrent-secret")
	h := NewHook([]string{"REDACT_C"})
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			res := &hwcloud.ToolResult{Content: "prefix concurrent-secret suffix"}
			h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
			if strings.Contains(res.Content, "concurrent-secret") {
				t.Errorf("secret leaked under concurrency")
			}
		}()
	}
	wg.Wait()
}

func TestOnToolEnd_ShortValueSkipped(t *testing.T) {
	// Values shorter than minSecretLen (8) are not real secrets and are
	// skipped — redacting "x" or "true" would corrupt vast swaths of output.
	withEnv(t, "REDACT_SHORT", "abc")
	h := NewHook([]string{"REDACT_SHORT"})
	out := "the value abc appears here"
	res := &hwcloud.ToolResult{Content: out}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	if res.Content != out {
		t.Fatalf("short value was redacted (should be skipped): %q", res.Content)
	}
}

func TestOnToolEnd_LongestFirst_NestedSecrets(t *testing.T) {
	// When one secret value is a substring of another, the longer one must
	// be replaced first. Otherwise the shorter one would partially consume
	// the longer match and leak the remainder.
	withEnv(t, "REDACT_LONG", "abcdef1234", "REDACT_SHORT", "abcdef12")
	h := NewHook([]string{"REDACT_SHORT", "REDACT_LONG"}) // short listed first on purpose
	res := &hwcloud.ToolResult{Content: "token=abcdef1234 here"}
	h.OnToolEnd(context.Background(), hwcloud.FunctionDefinition{}, nil, res, nil)
	// The long value must be fully redacted; "34" must not leak as a
	// leftover from a partial short-match.
	if strings.Contains(res.Content, "abcdef1234") || strings.Contains(res.Content, "abcdef12") {
		t.Fatalf("secret leaked: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[REDACTED]") {
		t.Fatalf("missing [REDACTED]: %q", res.Content)
	}
}
