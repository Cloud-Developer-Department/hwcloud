package slog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"log/slog"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// newCapturingObserver builds an Observer whose logger writes JSON to a buffer
// at the given minimum level. JSONHandler (not TextHandler) is used so the
// level is a parseable "level" field and slog.Group renders as a nested JSON
// object — both asserted by the tests below.
func newCapturingObserver(t *testing.T, level slog.Level) (*Observer, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})
	return NewObserver(slog.New(h)), &buf
}

// parseRecords splits the buffer into newline-delimited JSON records. Each
// Log call produces exactly one line, so the record count equals the number
// of events the Observer emitted (and that survived the handler's level
// filter).
func parseRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		records = append(records, rec)
	}
	return records
}

// TestObserveDecision_LevelByOutcome is the core classification contract:
// each of the 10 Outcome constants maps to exactly one slog level. A change
// to the switch in ObserveDecision must update this table or the test fails —
// keeping the level policy explicit and reviewed, not implicit.
func TestObserveDecision_LevelByOutcome(t *testing.T) {
	cases := []struct {
		outcome string
		want    string // JSONHandler level string
	}{
		{hwcloud.OutcomeFailed, "WARN"},
		{hwcloud.OutcomeDeny, "INFO"},
		{hwcloud.OutcomeAsk, "INFO"},
		{hwcloud.OutcomeAllow, "DEBUG"},
		{hwcloud.OutcomeHit, "DEBUG"},
		{hwcloud.OutcomeMiss, "DEBUG"},
		{hwcloud.OutcomeSkipped, "DEBUG"},
		{hwcloud.OutcomeStored, "DEBUG"},
		{hwcloud.OutcomeAttempted, "DEBUG"},
		{hwcloud.OutcomeFreed, "DEBUG"},
	}
	o, buf := newCapturingObserver(t, slog.LevelDebug)
	ctx := context.Background()
	for _, c := range cases {
		o.ObserveDecision(ctx, hwcloud.DecisionEvent{
			Layer:   "test",
			Outcome: c.outcome,
			Subject: "s",
		})
	}
	recs := parseRecords(t, buf)
	if len(recs) != len(cases) {
		t.Fatalf("got %d records, want %d (one per outcome)", len(recs), len(cases))
	}
	for i, c := range cases {
		got, _ := recs[i]["level"].(string)
		if got != c.want {
			t.Errorf("outcome %q: level = %q, want %q", c.outcome, got, c.want)
		}
	}
}

// TestObserveStage_ErrIsWarnNoDebugDup asserts the error branch: a non-nil
// Err produces exactly ONE Warn line carrying an "error" attr — NOT a Debug
// line plus a Warn line (the early-return in ObserveStage suppresses the
// Debug). This is the "one line per event, level by outcome" contract.
func TestObserveStage_ErrIsWarnNoDebugDup(t *testing.T) {
	o, buf := newCapturingObserver(t, slog.LevelDebug)
	stageErr := errors.New("model 429")
	o.ObserveStage(context.Background(), hwcloud.StageEvent{
		Name:     "model.call",
		Phase:    "leave",
		Duration: 12 * time.Millisecond,
		Err:      stageErr,
		RunID:    "run-1",
		TurnID:   0,
	})
	recs := parseRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (Debug suppressed on error); records: %v", len(recs), recs)
	}
	if recs[0]["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", recs[0]["level"])
	}
	if recs[0]["msg"] != "stage failed" {
		t.Errorf("msg = %v, want \"stage failed\"", recs[0]["msg"])
	}
	if recs[0]["error"] != stageErr.Error() {
		t.Errorf("error attr = %v, want %q", recs[0]["error"], stageErr.Error())
	}
}

// TestObserveStage_SuccessIsDebugWithJoinKeys asserts the success branch: a
// nil Err produces a Debug line carrying run_id/turn_id so stage logs join to
// the run trajectory — the old slogObserver dropped these, leaving stage logs
// unjoinable.
func TestObserveStage_SuccessIsDebugWithJoinKeys(t *testing.T) {
	o, buf := newCapturingObserver(t, slog.LevelDebug)
	o.ObserveStage(context.Background(), hwcloud.StageEvent{
		Name:     "memory.fetch",
		Phase:    "leave",
		Duration: 3 * time.Millisecond,
		RunID:    "run-join",
		TurnID:   2,
	})
	recs := parseRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0]["level"] != "DEBUG" {
		t.Errorf("level = %v, want DEBUG", recs[0]["level"])
	}
	if recs[0]["run_id"] != "run-join" {
		t.Errorf("run_id = %v, want run-join (stage log must carry join key)", recs[0]["run_id"])
	}
	// turn_id is a number in JSON; JSONHandler renders int as float64 in
	// map[string]any.
	if got, _ := recs[0]["turn_id"].(float64); got != 2 {
		t.Errorf("turn_id = %v, want 2", recs[0]["turn_id"])
	}
	// Duration renders as a human-readable string with unit, NOT a bare
	// nanosecond int (3ms → "3ms"). A regression to int rendering would
	// surface as a float64 here (3000000) instead of a string.
	if got, ok := recs[0]["duration"].(string); !ok || got != "3ms" {
		t.Errorf("duration = %v (type %T), want string \"3ms\" (must carry unit, not bare ns)", recs[0]["duration"], recs[0]["duration"])
	}
	// No error attr on success.
	if _, ok := recs[0]["error"]; ok {
		t.Errorf("success stage line carries an \"error\" attr: %v", recs[0]["error"])
	}
}

// TestObserveDecision_DetailNestsUnderGroup asserts Detail renders as a
// nested "detail" JSON object, NOT flattened into the top level. This guards
// against key collisions (Detail["error"] vs a top-level "error") and keeps
// the fixed identity fields filterable at the top level for dashboards.
func TestObserveDecision_DetailNestsUnderGroup(t *testing.T) {
	o, buf := newCapturingObserver(t, slog.LevelDebug)
	o.ObserveDecision(context.Background(), hwcloud.DecisionEvent{
		Layer:   "compaction.auto",
		Outcome: hwcloud.OutcomeFreed,
		Subject: "session-1",
		Detail:  map[string]any{"freed_tokens": 1200, "from": 10, "to": 14},
		RunID:   "run-x",
	})
	recs := parseRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	detail, ok := recs[0]["detail"].(map[string]any)
	if !ok {
		t.Fatalf("no nested \"detail\" object; got %v", recs[0]["detail"])
	}
	if detail["freed_tokens"].(float64) != 1200 {
		t.Errorf("detail.freed_tokens = %v, want 1200", detail["freed_tokens"])
	}
	// The detail keys must NOT leak to the top level.
	for _, leak := range []string{"freed_tokens", "from", "to"} {
		if _, ok := recs[0][leak]; ok {
			t.Errorf("detail key %q leaked to top level (should be nested under \"detail\")", leak)
		}
	}
	// Fixed identity fields stay top-level.
	if recs[0]["layer"] != "compaction.auto" {
		t.Errorf("top-level layer = %v, want compaction.auto", recs[0]["layer"])
	}
	if recs[0]["outcome"] != "freed" {
		t.Errorf("top-level outcome = %v, want freed", recs[0]["outcome"])
	}
}

// TestNewObserver_NilLoggerNoPanic asserts the nil-guard: NewObserver(nil)
// falls back to slog.Default() rather than panicking. The constructor is the
// only place logger is dereferenced, so this proves the whole type is
// nil-safe at construction.
func TestNewObserver_NilLoggerNoPanic(t *testing.T) {
	var got *Observer
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewObserver(nil) panicked: %v", r)
		}
		if got == nil {
			t.Fatal("NewObserver(nil) returned nil")
		}
	}()
	got = NewObserver(nil)
}

// TestObserveDecision_LowLevelSuppressed asserts the level classification is
// real — not just a label. With the handler's minimum level set to WARN, a
// Debug-classified event (OutcomeAllow) is dropped by the handler: the buffer
// stays empty. This proves routine outcomes don't spam an operator-facing
// log (which typically runs at Info or above), while Deny/Ask/Failed still
// surface.
func TestObserveDecision_LowLevelSuppressed(t *testing.T) {
	// Handler at WARN: drops Info and Debug.
	o, buf := newCapturingObserver(t, slog.LevelWarn)
	ctx := context.Background()
	// Routine → Debug → dropped.
	o.ObserveDecision(ctx, hwcloud.DecisionEvent{Layer: "policy.rule", Outcome: hwcloud.OutcomeAllow, Subject: "tool"})
	// Actionable → Info → dropped (below WARN).
	o.ObserveDecision(ctx, hwcloud.DecisionEvent{Layer: "policy.rule", Outcome: hwcloud.OutcomeDeny, Subject: "tool"})
	// Failure → Warn → kept.
	o.ObserveDecision(ctx, hwcloud.DecisionEvent{Layer: "context.recall", Outcome: hwcloud.OutcomeFailed, Subject: "q"})
	recs := parseRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records at WARN threshold, want 1 (only Failed survives); records: %v", len(recs), recs)
	}
	if recs[0]["outcome"] != "failed" {
		t.Errorf("surviving record outcome = %v, want failed", recs[0]["outcome"])
	}
}
