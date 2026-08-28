package hwcloud

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/version"
)

// ToolResult is the structured outcome of a [Tool] execution.
//
// Content is the display text shown to the model (truncated when the raw
// result exceeds the runtime result policy threshold — see
// [ApplyResultPolicy]). JSON carries optional structured data for tools
// that produce it (e.g. parsed plan output). Metadata holds arbitrary
// key/value observations (exit code, duration, mime type, ...).
//
// When a result is too large for the model context, the runtime saves the
// raw output to disk, sets Truncated and FileRef, and replaces Content
// with a short pointer the model can read or grep on demand.
type ToolResult struct {
	Content   string          `json:"content,omitempty"`
	JSON      json.RawMessage `json:"json,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	FileRef   string          `json:"file_ref,omitempty"`
	Error     *ToolError      `json:"error,omitempty"`
}

// ToolError is a structured tool failure. Retryable marks errors that the
// runtime may retry with backoff (P3 wires the retry policy); Code is an
// optional machine-readable error code.
type ToolError struct {
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
	Code      string `json:"code,omitempty"`
}

// ErrorResult constructs a ToolResult carrying a structured error.
func ErrorResult(err error, retryable bool, code string) *ToolResult {
	return &ToolResult{Error: &ToolError{
		Message:   err.Error(),
		Retryable: retryable,
		Code:      code,
	}}
}

// AsError returns the tool error as an error value, or nil for a successful
// result. Handy for call sites that want the classic (result, error) shape.
func (r *ToolResult) AsError() error {
	if r == nil || r.Error == nil {
		return nil
	}
	return &toolErrorValue{err: r.Error}
}

// IsErr reports whether the result carries an error.
func (r *ToolResult) IsErr() bool { return r != nil && r.Error != nil }

// toolErrorValue adapts ToolError to the error interface.
type toolErrorValue struct{ err *ToolError }

func (e *toolErrorValue) Error() string { return e.err.Message }

// ── Runtime result policy ──

// artifactFraction is the percentage of the model's context window a single
// tool result may consume before the runtime saves it to disk. Mirrors the
// former hooks/artifact threshold so behavior stays consistent.
const artifactFraction = 5

// ArtifactRoot returns the platform-appropriate artifact directory:
// Linux/macOS /tmp/<version.Name> (default /tmp/hwcloud), Windows
// %TEMP%\<version.Name>. Runtime result truncation saves oversized output
// here. tool.ArtifactRoot delegates here so the two stay structurally
// identical — there is a single source of truth for the tmp root.
func ArtifactRoot() string {
	return filepath.Join(os.TempDir(), version.SafeName())
}

// ResultPolicy decides how raw tool output becomes the final [ToolResult]
// the model sees. nil ResultPolicy = no truncation.
//
// Implementations must be safe for concurrent use. The runner applies the
// policy after hooks have run (so redaction happens first) and before the
// result enters memory.
type ResultPolicy interface {
	// Apply truncates/saves result in place. Returns the same pointer.
	Apply(ctx context.Context, session Session, result *ToolResult) *ToolResult
}

// DefaultResultPolicy truncates oversized tool results by saving the raw
// output to disk under <ArtifactRoot()>/sess-<sessionID>/ and replacing
// Content with a short pointer. The threshold is token-based, measured
// with the same tokenizer the runner uses for context-window trimming, so
// the two lines agree on one ruler. A result that survives this policy
// (≤ threshold tokens) will not, by itself, push the next turn past the
// window.
//
// The session directory layout (sess-<sessionID>) mirrors the process
// manager's per-session output dir so all session-scoped ephemeral state
// can be cleaned together.
type DefaultResultPolicy struct {
	// ModelID is the tokenizer model id (default "gpt-4").
	ModelID string
	// Window is the context window in tokens; 0 = fall back to the
	// session model's ContextWindow, then to 128 KB.
	Window int
}

// Apply implements [ResultPolicy].
func (p *DefaultResultPolicy) Apply(ctx context.Context, session Session, result *ToolResult) *ToolResult {
	if result == nil || result.Content == "" || result.Error != nil {
		return result
	}

	cw := p.Window
	modelID := p.ModelID
	if modelID == "" {
		modelID = "gpt-4"
	}
	if cw <= 0 && session.Model != nil {
		if w := session.Model.ContextWindow(); w > 0 {
			cw = w
		}
		if tm, ok := session.Model.(TokenizerModeler); ok {
			if name := tm.TokenizerModel(); name != "" {
				modelID = name
			}
		}
	}
	if cw <= 0 {
		cw = 128 * 1024
	}

	threshold := cw * artifactFraction / 100
	if CountTokens(modelID, result.Content) <= threshold {
		return result
	}

	dir := filepath.Join(ArtifactRoot(), "sess-"+SanitizeName(session.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// Truncation failed: log instead of silently flooding the model
		// with the raw oversized output.
		slog.Warn("artifact dir create failed; oversized result passed through", "session", session.ID, "error", err)
		return result
	}
	path := filepath.Join(dir, "artifact-"+randHex(8)+".txt")
	raw := result.Content
	// Break overlong single lines (minified JSON, base64, logs without
	// newlines): read/grep cap a single line at 1MB (bufio.Scanner), so
	// an unwrapped megabyte line would make the artifact unreadable and
	// the model could never inspect what it was told to read.
	raw = wrapLongLines(raw, maxArtifactLine)
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		slog.Warn("artifact write failed; oversized result passed through", "session", session.ID, "error", err)
		return result
	}

	origBytes := len(result.Content)
	sizeKB := (len(raw) + 1023) / 1024
	lines := strings.Count(raw, "\n") + 1
	result.Content = "Output saved to " + path + " (" + strconv.Itoa(sizeKB) + " KB, " + strconv.Itoa(lines) + " lines). Use read or grep to search."
	result.Truncated = true
	result.FileRef = path
	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	result.Metadata["artifact_bytes"] = origBytes
	return result
}

// maxArtifactLine is the line length cap applied when spilling an
// oversized result to disk. tool/read and tool/grep scan line-by-line
// with a 1MB per-line cap; rune-counting keeps the byte size ≤ 4× the
// rune count, so 32K runes stay well inside the 1MB bound even for
// 4-byte (emoji) content. 32K runes (~32-128KB) is also a comfortable
// read unit for the model — a single 128K-rune line would still be a
// wall of text. A var (not const) so tests can shrink it.
var maxArtifactLine = 32 * 1024

// wrapMarker is appended at every artificial break so the model can tell
// a wrapped continuation from a newline that was actually in the original
// output — without it, a minified blob read from the artifact would look
// like real line breaks and could be misparsed (e.g. treated as separate
// log lines).
const wrapMarker = " [line wrapped; continues below]"

// wrapLongLines inserts '\n' every maxLine runes inside lines that run
// longer than maxLine, marking each artificial break with wrapMarker.
// Content-preserving for display purposes: minified JSON / base64 /
// newline-less logs read the same with a marked break every ~32K runes.
func wrapLongLines(s string, maxLine int) string {
	if maxLine <= 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + (len(s)/maxLine+1)*(len(wrapMarker)+1))
	count := 0
	for _, r := range s {
		b.WriteRune(r)
		// Both '\n' and '\r' terminate a line: '\r'-only line endings
		// (old Mac, some tool output) and Windows '\r\n' must not be
		// counted as content — otherwise a '\r'-separated blob reads as
		// one huge line and gets falsely wrapped (with a continuation
		// marker the model would misread as a single long line).
		if r == '\n' || r == '\r' {
			count = 0
			continue
		}
		count++
		if count >= maxLine {
			b.WriteString(wrapMarker)
			b.WriteByte('\n')
			count = 0
		}
	}
	return b.String()
}

// SanitizeName replaces path separators (and NUL) with '_' so a hostile
// or malformed session id cannot escape its session directory. Export for
// callers that build session-scoped paths (REST cleanup, artifacts).
func SanitizeName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == 0 {
			return '_'
		}
		return r
	}, name)
}

// artifactSeq disambiguates artifact names when crypto/rand fails (all-zero
// names would collide and overwrite each other).
var artifactSeq atomic.Uint64

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano(), 10))) + "-" + strconv.FormatUint(artifactSeq.Add(1), 10)
	}
	return hex.EncodeToString(b)
}
