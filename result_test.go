package hwcloud

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeWindowModel is a Model with a fixed, small context window so the
// result policy threshold (cw * artifactFraction / 100) can be made
// trivially small in tests without writing megabytes of "result".
type fakeWindowModel struct{ cw int }

func (m *fakeWindowModel) ChatCompletion(context.Context, ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, nil
}
func (m *fakeWindowModel) ChatCompletionStream(context.Context, ChatCompletionRequest) (StreamReader, error) {
	return nil, nil
}
func (m *fakeWindowModel) ContextWindow() int { return m.cw }

// TestDefaultResultPolicy_UsesSessionModelContextWindow asserts the built-in
// result policy reads the context window from session.Model, truncating
// oversized tool output by saving it to disk and replacing Content with a
// pointer.
func TestDefaultResultPolicy_UsesSessionModelContextWindow(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	root := ArtifactRoot()
	if !strings.HasPrefix(root, scratch) {
		t.Fatalf("ArtifactRoot=%q not under TMPDIR scratch %q", root, scratch)
	}

	// Window = 10_000 tokens → threshold = 10_000 * 5 / 100 = 500 tokens.
	// 5000 ASCII bytes ≈ 625 tokens > 500 → saved; 1000 bytes ≈ 250
	// tokens < 500 → untouched.
	const big = 5000
	const small = 1000

	sess := Session{
		ID:    "s-test",
		Model: &fakeWindowModel{cw: 10_000},
	}
	ctx := context.Background()

	// Big result → truncated, saved to disk, FileRef set.
	bigRes := strings.Repeat("x", big)
	policy := &DefaultResultPolicy{}
	res := policy.Apply(ctx, sess, &ToolResult{Content: bigRes})
	if !res.Truncated {
		t.Fatal("big result not flagged Truncated")
	}
	if res.FileRef == "" {
		t.Fatal("big result missing FileRef")
	}
	if strings.Contains(res.Content, strings.Repeat("x", 100)) {
		t.Fatal("Content still carries the raw big output")
	}
	// Layout: <ArtifactRoot()>/sess-<sessionID>/artifact-<8hex>.txt
	sessDir := filepath.Join(root, "sess-"+sess.ID)
	matches, err := filepath.Glob(filepath.Join(sessDir, "artifact-*.txt"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 saved artifact, got %d (%v)", len(matches), matches)
	}
	if name := filepath.Base(matches[0]); len(name) <= len("artifact-.txt") {
		t.Fatalf("artifact filename %q has no random suffix", name)
	}
	got, _ := os.ReadFile(matches[0])
	if string(got) != bigRes {
		t.Fatalf("artifact content mismatch: got len=%d want len=%d", len(got), len(bigRes))
	}

	// Small result → NOT truncated, unchanged.
	smallRes := strings.Repeat("y", small)
	res2 := policy.Apply(ctx, sess, &ToolResult{Content: smallRes})
	if res2.Truncated || res2.FileRef != "" || res2.Content != smallRes {
		t.Fatalf("small result was truncated: %+v", res2)
	}
	matches2, _ := filepath.Glob(filepath.Join(sessDir, "*.txt"))
	if len(matches2) != 1 {
		t.Fatalf("small result should not have been saved; file count = %d", len(matches2))
	}
}

// TestDefaultResultPolicy_NilAndErrorResultsPassthrough asserts error results
// and nil results are never truncated.
func TestDefaultResultPolicy_NilAndErrorResultsPassthrough(t *testing.T) {
	policy := &DefaultResultPolicy{}
	sess := Session{ID: "s-nil", Model: &fakeWindowModel{cw: 100}}

	if got := policy.Apply(context.Background(), sess, nil); got != nil {
		t.Fatalf("nil result became non-nil: %+v", got)
	}

	errRes := &ToolResult{Error: &ToolError{Message: "boom"}}
	got := policy.Apply(context.Background(), sess, errRes)
	if got.Truncated || got.FileRef != "" {
		t.Fatalf("error result was truncated: %+v", got)
	}
}

// TestDefaultResultPolicy_WrapsOverlongSingleLine: a huge single-line
// result (minified JSON / base64 / newline-less logs) must be written to
// disk with line breaks every maxArtifactLine runes — read/grep cap a
// single line at 1MB, so an unwrapped megabyte line would make the
// artifact unreadable ("bufio.Scanner: token too long").
//
// maxArtifactLine is shrunk for the test: tokenizer.Count is the slow
// part of Apply (linear, ~0.4ms/byte), so the input stays small while
// still exceeding both the (shrunk) wrap threshold and the token
// threshold.
func TestDefaultResultPolicy_WrapsOverlongSingleLine(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	_ = ArtifactRoot()

	old := maxArtifactLine
	maxArtifactLine = 1024
	t.Cleanup(func() { maxArtifactLine = old })

	// ~6KB single line, no newlines: past the 500-token threshold
	// (window 10k × 5%) and past the shrunk wrap threshold.
	big := strings.Repeat("z", maxArtifactLine*6)
	sess := Session{ID: "s-wrap", Model: &fakeWindowModel{cw: 10_000}}
	policy := &DefaultResultPolicy{}
	res := policy.Apply(context.Background(), sess, &ToolResult{Content: big})
	if !res.Truncated || res.FileRef == "" {
		t.Fatalf("expected truncation: %+v", res)
	}

	got, err := os.ReadFile(res.FileRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < len(big) {
		t.Fatalf("wrapped artifact smaller than original: %d < %d", len(got), len(big))
	}
	// Every line must stay within the wrap cap (the production cap of
	// 32K runes stays inside the read/grep 1MB single-line limit).
	// Wrapped lines carry the wrapMarker suffix — the CONTENT part stays
	// within the cap.
	breaks := 0
	for _, ln := range strings.Split(string(got), "\n") {
		content := strings.TrimSuffix(ln, wrapMarker)
		if n := len([]rune(content)); n > maxArtifactLine {
			t.Fatalf("line content of %d runes exceeds maxArtifactLine %d", n, maxArtifactLine)
		}
		if strings.HasSuffix(ln, wrapMarker) {
			breaks++
		}
	}
	if breaks == 0 {
		t.Fatalf("no wrap markers found in artifact (want %d breaks)", len(big)/maxArtifactLine)
	}
	if res.Metadata["artifact_bytes"] != len(big) {
		t.Fatalf("artifact_bytes = %v, want original size %d", res.Metadata["artifact_bytes"], len(big))
	}

	// Short single lines (no newlines) pass through unwrapped.
	small := strings.Repeat("q", 1000)
	res2 := policy.Apply(context.Background(), sess, &ToolResult{Content: small})
	if res2.Truncated {
		t.Fatalf("small result truncated: %+v", res2)
	}
}

// wrapLongLines must treat '\r' as a line terminator too: '\r'-only
// endings and Windows '\r\n' must never be counted as content (which
// would falsely wrap short "\r"-separated lines and mislead the model
// with a continuation marker).
func TestWrapLongLinesTreatsCRAsLineEnding(t *testing.T) {
	// CR-only separated content: each "line" is 1 rune — far below the
	// (shrunk) cap — so no artificial breaks may appear.
	s := strings.Repeat("a\r", 3000)
	got := wrapLongLines(s, 1024)
	if strings.Contains(got, wrapMarker) {
		t.Fatalf("CR-separated content was falsely wrapped: %d markers", strings.Count(got, wrapMarker))
	}

	// Windows CRLF: same, no breaks.
	got = wrapLongLines(strings.Repeat("b\r\n", 3000), 1024)
	if strings.Contains(got, wrapMarker) {
		t.Fatalf("CRLF content was falsely wrapped: %d markers", strings.Count(got, wrapMarker))
	}

	// A genuine long line WITH trailing CRLF still wraps at the cap.
	long := strings.Repeat("c", 4000) + "\r\n"
	got = wrapLongLines(long, 1024)
	if !strings.Contains(got, wrapMarker) {
		t.Fatal("long CRLF-terminated line was not wrapped")
	}
}
