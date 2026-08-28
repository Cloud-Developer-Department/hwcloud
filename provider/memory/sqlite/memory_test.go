package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
)

// captureDec is a DecisionObserver that records every event it receives, so
// the regression test can assert the four-tuple join key (session_id, run_id,
// turn_id, parent_run_id) on events emitted by the sqlite Memory's leaf
// functions (indexEmbedding + knowledgeRecall).
type captureDec struct {
	mu      sync.Mutex
	events  []hwcloud.DecisionEvent
}

func (c *captureDec) ObserveDecision(_ context.Context, e hwcloud.DecisionEvent) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *captureDec) snapshot() []hwcloud.DecisionEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]hwcloud.DecisionEvent, len(c.events))
	copy(out, c.events)
	return out
}

// constEmbedder returns the same unit vector for every text, so cosine
// similarity between any stored item and any query is 1.0 → the vector-recall
// Hit branch fires deterministically (no flake from real embedding semantics).
// Dimensions() must satisfy the knowledge_vectors schema (a blob of N floats).
type constEmbedder struct{ dim int }

func (constEmbedder) Embed(_ context.Context, _ string) ([]float64, error) {
	v := make([]float64, 4)
	for i := range v {
		v[i] = 1.0
	}
	return v, nil
}
func (e constEmbedder) Dimensions() int { return e.dim }

// kernelCtx mimics what kernel.run() stamps onto ctx at entry: a fresh RunID,
// a TurnID, a ParentRunID (when the run is a child of a team/orchestrator),
// a Session (WithSession), and the DecisionObserver (WithDecisionObserver).
// The sqlite Memory's leaf functions have no session/run param — they read
// all four join keys back out of this ctx, so the test must stamp exactly what
// run() stamps or the assertion is meaningless.
func kernelCtx(t *testing.T, obs hwcloud.DecisionObserver) context.Context {
	t.Helper()
	ctx := hwcloud.WithRunInfo(context.Background(), hwcloud.RunInfo{
		RunID:       "leaf-run-abc",
		TurnID:      2,
		ParentRunID: "team-parent-run",
	})
	ctx = hwcloud.WithSession(ctx, hwcloud.Session{ID: "sess-leaf"})
	ctx = hwcloud.WithDecisionObserver(ctx, obs)
	return ctx
}

// assertJoinKey checks the four-tuple join key the leaf emit closures must
// stamp: SessionID (from ctx Session), RunID + TurnID + ParentRunID (from ctx
// RunInfo). Before the fix, SessionID was "" on every event here because the
// closures called obs.ObserveDecision directly (bypassing the free
// ObserveDecision helper) while their comments claimed the helper would fill it.
func assertJoinKey(t *testing.T, ev hwcloud.DecisionEvent, where string) {
	t.Helper()
	if ev.SessionID != "sess-leaf" {
		t.Errorf("%s SessionID = %q, want sess-leaf (leaf did not stamp from ctx Session)", where, ev.SessionID)
	}
	if ev.RunID != "leaf-run-abc" {
		t.Errorf("%s RunID = %q, want leaf-run-abc", where, ev.RunID)
	}
	if ev.TurnID != 2 {
		t.Errorf("%s TurnID = %d, want 2", where, ev.TurnID)
	}
	if ev.ParentRunID != "team-parent-run" {
		t.Errorf("%s ParentRunID = %q, want team-parent-run (parent link lost)", where, ev.ParentRunID)
	}
}

// TestMemory_StoreIndexEmbedding_StampsJoinKey is the #2 regression for the
// Store path: indexEmbedding (called by Store after the row is persisted)
// emits a DecisionExtractor event (Stored when an embedder is configured). The
// event must carry the full four-tuple join key — specifically SessionID, which
// the buggy emit closure left empty.
func TestMemory_StoreIndexEmbedding_StampsJoinKey(t *testing.T) {
	m, err := New(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	m.WithEmbedder(constEmbedder{dim: 4})

	cap := &captureDec{}
	ctx := kernelCtx(t, cap)

	if err := m.Store(ctx, ctxpkg.ContextScope{UserID: "u1"}, ctxpkg.MemoryItem{
		Kind: "fact", Content: "the sky is blue", Topic: "sky",
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	decs := cap.snapshot()
	if len(decs) == 0 {
		t.Fatal("Store emitted no DecisionEvent; indexEmbedding did not fire")
	}
	// The Store path emits exactly one DecisionExtractor event (Stored branch —
	// embedder is configured + id > 0, so the Skipped branch is skipped).
	var stored *hwcloud.DecisionEvent
	for i := range decs {
		if decs[i].Layer == hwcloud.DecisionExtractor && decs[i].Outcome == hwcloud.OutcomeStored {
			stored = &decs[i]
			break
		}
	}
	if stored == nil {
		t.Fatalf("no DecisionExtractor/Stored event; got %+v", decs)
	}
	assertJoinKey(t, *stored, "indexEmbedding Stored event")
}

// TestMemory_Recall_VectorHit_StampsJoinKey is the #2 regression for the
// Recall path: knowledgeRecall emits DecisionVectorRecall events (Hit when the
// vector search returns entries). The constEmbedder makes every stored item
// cosine-similar to the query (sim = 1.0), so a prior Store guarantees a Hit.
func TestMemory_Recall_VectorHit_StampsJoinKey(t *testing.T) {
	m, err := New(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	m.WithEmbedder(constEmbedder{dim: 4})

	// Seed one knowledge item so recall has a candidate to hit on. Store uses
	// a background ctx with no observer — the seed's own Stored event is
	// irrelevant; only the recall event is asserted.
	seedCtx := hwcloud.WithRunInfo(context.Background(), hwcloud.RunInfo{RunID: "seed"})
	seedCtx = hwcloud.WithSession(seedCtx, hwcloud.Session{ID: "sess-leaf"})
	if err := m.Store(seedCtx, ctxpkg.ContextScope{UserID: "u1"}, ctxpkg.MemoryItem{
		Kind: "fact", Content: "the sky is blue", Topic: "sky",
	}); err != nil {
		t.Fatalf("seed Store: %v", err)
	}

	cap := &captureDec{}
	ctx := kernelCtx(t, cap)

	entries, err := m.Recall(ctx, ctxpkg.ContextScope{UserID: "u1"}, "anything", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("Recall returned no entries; seed did not take — vector Hit branch won't fire")
	}

	decs := cap.snapshot()
	var hit *hwcloud.DecisionEvent
	for i := range decs {
		if decs[i].Layer == hwcloud.DecisionVectorRecall && decs[i].Outcome == hwcloud.OutcomeHit {
			hit = &decs[i]
			break
		}
	}
	if hit == nil {
		t.Fatalf("no DecisionVectorRecall/Hit event; got %+v", decs)
	}
	assertJoinKey(t, *hit, "knowledgeRecall vector Hit event")
}

// TestMemory_Recall_KeywordMiss_StampsJoinKey covers the keyword-fallback
// branch: with NO embedder, knowledgeRecall skips the vector path entirely
// and emits only a keyword-recall event (Miss when no LIKE match). This
// proves the fix holds on both emit closures' branches, not just the vector
// path. The join key must still be complete.
func TestMemory_Recall_KeywordMiss_StampsJoinKey(t *testing.T) {
	m, err := New(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	// No embedder → vector path skipped, keyword fallback used.

	cap := &captureDec{}
	ctx := kernelCtx(t, cap)

	// Query that won't match any keyword (no rows stored) → keyword Miss.
	if _, err := m.Recall(ctx, ctxpkg.ContextScope{UserID: "u1"}, "nomatch", 5); err != nil {
		t.Fatalf("Recall: %v", err)
	}

	decs := cap.snapshot()
	if len(decs) == 0 {
		t.Fatal("keyword recall emitted no DecisionEvent")
	}
	// Every event from this call is a keyword-recall event; all must carry the
	// join key. (The keyword path emits Hit/Miss/Failed — assert on all of
	// them, since the closure is shared across the three outcomes.)
	for _, d := range decs {
		assertJoinKey(t, d, "knowledgeRecall keyword event")
	}
}

// TestMemory_NoObserverInCtx_NoPanic guards the nil path: a ctx that was not
// prepared by a kernel run (no DecisionObserver injected) must not panic when
// Store/Recall emit. The leaf closures guard on obs != nil, so this is a
// belt-and-suspenders regression for the fix touching those closures.
func TestMemory_NoObserverInCtx_NoPanic(t *testing.T) {
	m, err := New(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	m.WithEmbedder(constEmbedder{dim: 4})

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Store/Recall panicked on ctx without observer: %v", rec)
		}
	}()
	ctx := hwcloud.WithRunInfo(context.Background(), hwcloud.RunInfo{RunID: "r"})
	ctx = hwcloud.WithSession(ctx, hwcloud.Session{ID: "s"})
	_ = m.Store(ctx, ctxpkg.ContextScope{UserID: "u1"}, ctxpkg.MemoryItem{
		Kind: "fact", Content: "x", Topic: "t",
	})
	_, _ = m.Recall(ctx, ctxpkg.ContextScope{UserID: "u1"}, "x", 5)
}
