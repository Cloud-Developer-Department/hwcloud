// Package redact provides a RunHooks implementation that masks sensitive
// environment-variable values in tool results. Each configured env-var name
// is resolved at redaction time via os.Getenv; if its value appears in the
// tool result, every occurrence is replaced with "[REDACTED]" and a hint is
// appended telling the model the value was sensitive and must not be
// reconstructed.
//
// The hook stores env-var *names* only — secret values are never held in
// struct state and are resolved lazily so construction order (e.g. env set
// after agent creation) does not matter.
//
// # Coverage scope — read carefully
//
// This hook ONLY redacts the *result* (output) and *error* of a tool call.
// It does NOT redact:
//   - tool call *arguments* (args) — the LLM may place secrets in args
//     (e.g. shell command, http headers, DB DSN); those are stored in
//     memory and sent to the client unredacted.
//   - secrets that were not pre-registered as env-var names. Only the exact
//     value of a named env var is matched (plain substring, no regex).
//     PII, credit cards, AWS keys, private keys, JWTs, etc. are NOT
//     detected unless their value happens to be an env var listed here.
//   - env-var values shorter than 8 characters. Short values are skipped to
//     avoid mis-masking normal output (e.g. "x", "true", "us-east"). A real
//     secret shorter than 8 chars will silently pass through — do not rely
//     on this hook for very short secrets.
//   - secrets whose value contains JSON special characters (", \, control
//     chars) when the result is JSON. The env-var holds the raw (unescaped)
//     value, but inside a JSON string it is escaped (e.g. `"` → `\"`), so
//     the byte-level substring match silently fails and the secret passes
//     through. This is a known limitation of text-level matching; a
//     JSON-aware redactor would be needed to cover it.
//
// For a hook that redacts args or matches regex patterns, extend this
// package or compose another hook — that is out of scope for this one.
//
// # Ordering with other hooks
//
// redact must run FIRST (before slog, artifact) so downstream observers
// never see raw secrets in logs or on disk. This ordering is established by
// the caller in buildOpts, not by this hook.
//
// # Failure mode
//
// redaction never fails: it uses only os.Getenv + strings.ReplaceAll, which
// cannot error. If this hook is ever extended with regex or JSON parsing,
// the degradation policy is: on any internal failure, replace the whole
// result with a safe placeholder rather than return the original (leaking)
// data. Never fall back to the unredacted input.
//
// Usage:
//
//	agent := hwcloud.NewAgent("bot",
//	    hwcloud.WithRunHooks(redact.NewHook([]string{"AWS_SECRET_ACCESS_KEY"})),
//	    ...
//	)
package redact

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// redacted is the placeholder substituted for every sensitive value.
const redacted = "[REDACTED]"

// hint is appended once to the result when any redaction occurred.
const hint = "\n\n[REDACTED] values were present in this result and have been masked. Do not attempt to reconstruct them."

// Hook masks sensitive env-var values in tool results. Implements
// hwcloud.RunHooks.
type Hook struct {
	envNames []string
}

// NewHook creates a Hook that redacts the values of the given env-var names.
// A nil or empty slice yields a hook that never modifies results.
func NewHook(envNames []string) *Hook {
	// Dedup and drop empty names so OnToolEnd does no redundant work and
	// os.Getenv("") is never called.
	seen := make(map[string]struct{}, len(envNames))
	clean := make([]string, 0, len(envNames))
	for _, n := range envNames {
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		clean = append(clean, n)
	}
	return &Hook{envNames: clean}
}

// OnAgentStart is a no-op.
func (h *Hook) OnAgentStart(ctx context.Context, req hwcloud.ChatCompletionRequest) (context.Context, any, error) {
	return ctx, nil, nil
}

// OnAgentEnd is a no-op.
func (h *Hook) OnAgentEnd(ctx context.Context, req hwcloud.ChatCompletionRequest, resp *hwcloud.ChatCompletionResponse, runErr error, startState any) {
}

// OnToolStart is a no-op. Tool args are not redacted by this hook (see
// package doc — Coverage scope).
func (h *Hook) OnToolStart(ctx context.Context, tool hwcloud.FunctionDefinition, args json.RawMessage) (context.Context, any, error) {
	return ctx, nil, nil
}

// OnToolEnd replaces every occurrence of each sensitive env-var value in
// *result with "[REDACTED]". If any replacement happened, a hint is appended
// once (idempotently). The error string is redacted likewise when *err != nil.
//
// Values are resolved lazily via os.Getenv so secrets never live in hook
// state and envvars set after construction are still honored. Empty values
// (unset or blank env vars) are skipped — redacting the empty string would
// corrupt every result.
//
// If the result is valid JSON, the hint is NOT appended as a trailing string
// (that would break JSON validity); the redaction still happens in-place
// and the result stays parseable.
func (h *Hook) OnToolEnd(ctx context.Context, tool hwcloud.FunctionDefinition, args json.RawMessage, result *hwcloud.ToolResult, startState any) {
	if len(h.envNames) == 0 {
		return
	}
	if result != nil && result.Content != "" {
		if r, ok := redactString(result.Content, h.envNames); ok {
			result.Content = applyHint(r, result.Content)
		}
	}
	if result != nil && result.Error != nil {
		if s, ok := redactString(result.Error.Message, h.envNames); ok {
			result.Error.Message = applyHint(s, result.Error.Message)
		}
	}
}

// applyHint appends the redaction hint to s unless s already contains it
// (idempotent) or s is valid JSON (appending would break JSON validity).
// orig is the pre-redaction string, used only to avoid double-hinting when
// the original already carried a hint from a prior redaction.
func applyHint(s, orig string) string {
	if strings.Contains(orig, hint) {
		// Already hinted in a prior pass — don't stack.
		return s
	}
	if json.Valid([]byte(s)) {
		// Result is (still) valid JSON: don't append trailing text.
		return s
	}
	return s + hint
}

// minSecretLen is the minimum length a secret value must have to be
// redacted. Shorter values are skipped: a 1–7 char env-var value (e.g. a
// region code, a mode flag) is almost never a real secret, and redacting
// it would mis-mask large swaths of normal output (every occurrence of "x",
// "true", "us-east"…). Real secrets (API keys, tokens) are far longer.
const minSecretLen = 8

// redactString replaces every sensitive value in s with "[REDACTED]" and
// reports whether any replacement occurred.
//
// Values are resolved lazily via os.Getenv. Empty values and values shorter
// than minSecretLen are skipped. Surviving values are replaced longest-first
// so a shorter secret that is a substring of a longer one cannot partially
// consume it and block the longer match — e.g. with TOKEN_A="abcdef" and
// TOKEN_B="abc", replacing "abc" first would turn "abcdef" into
// "[REDACTED]def" and leak "def". Longest-first guarantees the longer
// value matches cleanly before the shorter one gets a chance.
func redactString(s string, envNames []string) (string, bool) {
	// Resolve and filter values up front.
	values := make([]string, 0, len(envNames))
	for _, name := range envNames {
		v := os.Getenv(name)
		if len(v) < minSecretLen {
			continue
		}
		values = append(values, v)
	}
	// Longest-first so substring secrets don't clobber longer matches.
	sort.SliceStable(values, func(i, j int) bool {
		return len(values[i]) > len(values[j])
	})
	changed := false
	for _, v := range values {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, redacted)
			changed = true
		}
	}
	return s, changed
}
