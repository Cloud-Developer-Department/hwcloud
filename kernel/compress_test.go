package kernel

import (
	"context"
	"sync"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// fakeCompressor is a minimal session.Compressor for CompressAll tests. Its
// Compact records the throughIndex it was asked to compress up to, then
// publishes a CompressedContext whose ThroughIndex reflects that cutoff (so
// the "summary advanced" branch fires). Compressed returns the stored cc.
type fakeCompressor struct {
	mu       sync.Mutex
	cc       *hwcloud.CompressedContext
	compactN int   // number of Compact calls
	cutoff   int   // last cutoff passed to Compact
}

func (f *fakeCompressor) Compact(_ context.Context, _ string, throughIndex int, _ []hwcloud.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compactN++
	f.cutoff = throughIndex
	// Summary advanced: ThroughIndex reflects the cutoff the caller passed.
	f.cc = &hwcloud.CompressedContext{
		Summary:      "compressed summary",
		ThroughIndex: throughIndex,
	}
	return nil
}

func (f *fakeCompressor) Compressed(_ context.Context, _ string) (*hwcloud.CompressedContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cc, nil
}

// fakeTokenizerModel is unused: CompressAll resolves the tokenizer via
// hwcloud.TokenizerModelID(model), and a nil model is safe there (the type
// assertion fails, falling back to "gpt-4"). We assert RunID/SessionID only,
// not freed-token counts, so no model is wired.

// TestCompressAll_StampedRunInfoAndSession is the #3 regression: CompressAll
// is invoked outside a kernel run (the ACP slash callback hands it a ctx that
// run() never stamped), so it must self-stamp a fresh RunInfo + WithSession
// at entry. Otherwise the 4 DecisionCompactionManual events it emits carry
// empty RunID/TurnID/SessionID, breaking the grouping invariant — a consumer
// cannot tell which /compact pass produced which event, nor join them to the
// session.
//
// The test asserts:
//   - every compaction event carries the SAME non-empty RunID (one /compact pass);
//   - every compaction event carries SessionID == the sessionID argument;
//   - the Attempted pre-call event fires, followed by a Freed event (the fake
//     compressor's Compact advances ThroughIndex, so the success branch runs);
//   - CompressStats.Compressed > 0 (the summary advanced).
func TestCompressAll_StampedRunInfoAndSession(t *testing.T) {
	store := &memStore{}
	// Seed enough messages that CompressAll has work to do (total > 0).
	for i := 0; i < 4; i++ {
		_ = store.Append(context.Background(), "compact-sess", hwcloud.UserMessage("msg"))
	}
	comp := &fakeCompressor{}
	cap := &captureBoth{}

	// No model wired: CompressAll's token math uses TokenizerModelID(nil) →
	// "gpt-4" fallback. We assert RunID/SessionID, not token counts.
	cfg := agent.New("test", agent.WithMaxTurns(1))
	deps := Deps{
		Observer:      cap,
		SessionStore:  store,
		Compressor:    comp,
	}
	rt := New(cfg, deps)

	// The ctx a slash callback would hand us — NO RunInfo, NO Session.
	ctx := context.Background()
	st, err := rt.CompressAll(ctx, "compact-sess")
	if err != nil {
		t.Fatalf("CompressAll: %v", err)
	}
	if st == nil {
		t.Fatal("CompressAll returned nil stats; compressor/store not wired")
	}
	if st.Compressed <= 0 {
		t.Fatalf("CompressStats.Compressed = %d, want >0 (summary did not advance)", st.Compressed)
	}

	_, decs := cap.snapshot()
	if len(decs) == 0 {
		t.Fatal("CompressAll emitted no DecisionEvents; observer wiring broken")
	}

	// Every event carries the same non-empty RunID (the fresh id CompressAll
	// stamped) and the sessionID argument — the #3 invariant.
	var runID string
	for i, d := range decs {
		if d.Layer != hwcloud.DecisionCompactionManual {
			t.Errorf("dec[%d] Layer = %q, want %q", i, d.Layer, hwcloud.DecisionCompactionManual)
		}
		if d.RunID == "" {
			t.Errorf("dec[%d] RunID empty (CompressAll did not self-stamp RunInfo)", i)
		}
		if d.SessionID != "compact-sess" {
			t.Errorf("dec[%d] SessionID = %q, want compact-sess (CompressAll did not WithSession)", i, d.SessionID)
		}
		if i == 0 {
			runID = d.RunID
		} else if d.RunID != runID {
			t.Errorf("dec[%d] RunID = %q, want %q (events from one /compact pass must share RunID)", i, d.RunID, runID)
		}
	}
	if runID == "" {
		t.Fatal("no RunID captured on any compaction event")
	}

	// The first event is the Attempted pre-call marker (compress.go L98-101).
	if decs[0].Outcome != hwcloud.OutcomeAttempted {
		t.Errorf("first event Outcome = %q, want %q (Attempted pre-call marker)", decs[0].Outcome, hwcloud.OutcomeAttempted)
	}

	// The Freed event must have fired (fake compressor advanced ThroughIndex)
	// and carry the 6-key detail shape (#5 vocabulary — verified fully in the
	// #5 test, here we just confirm the success branch ran).
	var freed *hwcloud.DecisionEvent
	for i := range decs {
		if decs[i].Outcome == hwcloud.OutcomeFreed {
			freed = &decs[i]
			break
		}
	}
	if freed == nil {
		t.Fatalf("no OutcomeFreed event; got outcomes: %v", outcomesOf(decs))
	}
}

// TestCompressAll_NilDepsNoPanic: with no store or compressor, CompressAll
// returns (nil, nil) — the documented nil contract. No event is emitted
// (the self-stamp happens AFTER the nil guards, so the helper never runs).
func TestCompressAll_NilDepsNoPanic(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("CompressAll panicked on nil deps: %v", rec)
		}
	}()
	cfg := agent.New("test", agent.WithMaxTurns(1))
	rt := New(cfg, Deps{Observer: &captureBoth{}})
	st, err := rt.CompressAll(context.Background(), "none")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if st != nil {
		t.Errorf("stats = %+v, want nil (no compressor → nil stats)", st)
	}
}

func outcomesOf(decs []hwcloud.DecisionEvent) []string {
	out := make([]string, len(decs))
	for i, d := range decs {
		out[i] = d.Outcome
	}
	return out
}

// ── #5: compactionOutcome vocabulary parity ──

// wantFreedKeys is the canonical 6-key detail shape for a Freed compaction
// event. Both the manual path (CompressAll) and the auto path (prepareMemory)
// route through compactionOutcome, so both MUST produce exactly this key set —
// a consumer reading "freed_tokens" should never have to branch on which path
// emitted the event.
var wantFreedKeys = map[string]bool{
	"compressed":     true,
	"freed_tokens":   true,
	"from":           true,
	"to":             true,
	"summary_tokens": true,
	"through_index":  true,
}

// detailKeys extracts the string keys of a map[string]any detail payload.
func detailKeys(d map[string]any) map[string]bool {
	out := make(map[string]bool, len(d))
	for k := range d {
		out[k] = true
	}
	return out
}

// TestCompactionOutcome_FreedDetailShape is the #5 regression: the Freed
// branch of compactionOutcome must emit all 6 keys (compressed/freed_tokens/
// from/to/summary_tokens/through_index), regardless of whether the auto path
// left summaryTokens/throughIndex at 0. Before the #5 fix, CompressAll hand-
// wrote its Freed detail with a divergent key set (summary_tokens +
// through_index, missing from/to) while the helper's auto path used from/to
// — a consumer couldn't parse both uniformly. Now both paths share this shape.
func TestCompactionOutcome_FreedDetailShape(t *testing.T) {
	// Manual-path shape: all fields populated (CompressAll sets every field
	// from CompressStats + cc.ThroughIndex).
	_, outcome, detail := compactionOutcome(compactionInfo{
		count:         4,
		freedTokens:   1200,
		from:          10,
		to:            14,
		summaryTokens: 80,
		throughIndex:  14,
	})
	if outcome != hwcloud.OutcomeFreed {
		t.Fatalf("outcome = %q, want %q", outcome, hwcloud.OutcomeFreed)
	}
	got := detailKeys(detail)
	if len(got) != len(wantFreedKeys) {
		t.Errorf("Freed detail has %d keys %v, want %d %v", len(got), got, len(wantFreedKeys), wantFreedKeys)
	}
	for k := range wantFreedKeys {
		if !got[k] {
			t.Errorf("Freed detail missing key %q (got %v)", k, got)
		}
	}
	// Value spot-check: the populated fields flow through unchanged.
	if detail["compressed"].(int) != 4 {
		t.Errorf("compressed = %v, want 4", detail["compressed"])
	}
	if detail["summary_tokens"].(int) != 80 {
		t.Errorf("summary_tokens = %v, want 80", detail["summary_tokens"])
	}
}

// TestCompactionOutcome_AutoPathZeroFieldsKept: the auto path (prepareMemory)
// leaves summaryTokens/throughIndex at 0 (it doesn't measure summary cost the
// way CompressAll does). The helper must STILL emit those keys — with value 0
// — so the detail shape is identical to the manual path. A consumer that
// branches on key presence would break if the auto path omitted them.
func TestCompactionOutcome_AutoPathZeroFieldsKept(t *testing.T) {
	// Auto-path shape: summaryTokens + throughIndex left 0 (the auto path
	// sets from/to but not these two).
	_, outcome, detail := compactionOutcome(compactionInfo{
		count:         3,
		freedTokens:   900,
		from:          5,
		to:            8,
		summaryTokens: 0, // auto path doesn't measure this
		throughIndex:  0, // auto path relies on from/to
	})
	if outcome != hwcloud.OutcomeFreed {
		t.Fatalf("outcome = %q, want %q", outcome, hwcloud.OutcomeFreed)
	}
	got := detailKeys(detail)
	for k := range wantFreedKeys {
		if !got[k] {
			t.Errorf("auto-path Freed detail missing key %q (zero-valued fields must still be present)", k)
		}
	}
	// The zero-valued keys are present AND zero — a consumer reads 0 and
	// knows summary cost wasn't measured for this pass.
	if detail["summary_tokens"].(int) != 0 {
		t.Errorf("auto-path summary_tokens = %v, want 0", detail["summary_tokens"])
	}
	if detail["through_index"].(int) != 0 {
		t.Errorf("auto-path through_index = %v, want 0", detail["through_index"])
	}
}

// TestCompactionOutcome_FailedAndSkippedVocabulary: the non-Freed branches
// must use the unified vocabulary — Failed carries an "error" detail,
// Skipped carries a "reason" detail. The Skipped reason is the helper's
// canonical "no new messages compressed" (the #5 fix unified the manual
// path's divergent "no new messages to compress" onto this string).
func TestCompactionOutcome_FailedAndSkippedVocabulary(t *testing.T) {
	// Failed.
	_, outcome, detail := compactionOutcome(compactionInfo{err: errBoom})
	if outcome != hwcloud.OutcomeFailed {
		t.Fatalf("outcome = %q, want %q", outcome, hwcloud.OutcomeFailed)
	}
	if detail["error"].(string) != "boom" {
		t.Errorf("Failed detail.error = %v, want boom", detail["error"])
	}

	// Skipped (count == 0, no error).
	_, outcome, detail = compactionOutcome(compactionInfo{})
	if outcome != hwcloud.OutcomeSkipped {
		t.Fatalf("outcome = %q, want %q", outcome, hwcloud.OutcomeSkipped)
	}
	if detail["reason"].(string) != "no new messages compressed" {
		t.Errorf("Skipped detail.reason = %v, want 'no new messages compressed'", detail["reason"])
	}
}

// errBoom is a sentinel for the Failed-branch test.
var errBoom = errBoomSentinel{}

type errBoomSentinel struct{}

func (errBoomSentinel) Error() string { return "boom" }

// TestCompressAll_FreedEventCarriesSixKeys is the manual-path integration
// half of #5: the Freed DecisionEvent CompressAll emits (compress.go L177-180)
// must carry all 6 keys in its Detail — proving the manual path routes through
// compactionOutcome rather than hand-writing a divergent detail. This pairs
// with the unit tests above to prove manual + auto parity end-to-end.
func TestCompressAll_FreedEventCarriesSixKeys(t *testing.T) {
	store := &memStore{}
	for i := 0; i < 4; i++ {
		_ = store.Append(context.Background(), "sixkey-sess", hwcloud.UserMessage("msg"))
	}
	comp := &fakeCompressor{}
	cap := &captureBoth{}
	cfg := agent.New("test", agent.WithMaxTurns(1))
	rt := New(cfg, Deps{Observer: cap, SessionStore: store, Compressor: comp})

	if _, err := rt.CompressAll(context.Background(), "sixkey-sess"); err != nil {
		t.Fatalf("CompressAll: %v", err)
	}
	_, decs := cap.snapshot()
	var freed *hwcloud.DecisionEvent
	for i := range decs {
		if decs[i].Outcome == hwcloud.OutcomeFreed {
			freed = &decs[i]
			break
		}
	}
	if freed == nil {
		t.Fatalf("no Freed event; got outcomes: %v", outcomesOf(decs))
	}
	got := detailKeys(freed.Detail)
	for k := range wantFreedKeys {
		if !got[k] {
			t.Errorf("CompressAll Freed detail missing key %q (manual path diverged from helper shape)", k)
		}
	}
}

// Compile-time: fakeCompressor satisfies session.Compressor.
var _ session.Compressor = (*fakeCompressor)(nil)
