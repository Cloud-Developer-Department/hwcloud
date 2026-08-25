package governance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// fakeStore is an in-memory session.Store for the persistence test.
type fakeStore struct {
	items map[string]session.SessionInfo
}

func (f *fakeStore) Save(_ context.Context, info session.SessionInfo) error {
	if f.items == nil {
		f.items = map[string]session.SessionInfo{}
	}
	f.items[info.ID] = info
	return nil
}
func (f *fakeStore) Get(_ context.Context, id string) (*session.SessionInfo, error) {
	if f.items == nil {
		return nil, nil
	}
	if info, ok := f.items[id]; ok {
		return &info, nil
	}
	return nil, nil
}
func (f *fakeStore) List(context.Context) ([]session.SessionInfo, error) { return nil, nil }
func (f *fakeStore) Delete(_ context.Context, id string) error {
	delete(f.items, id)
	return nil
}
func (f *fakeStore) Close() error { return nil }

// TestPersistentApprovalMemory_CrossInstance verifies decisions survive
// across memory instances (i.e. server restarts) when backed by the store.
func TestPersistentApprovalMemory_CrossInstance(t *testing.T) {
	store := &fakeStore{}
	ctx := context.Background()
	sessID := "s1"

	// Instance 1 (first process): remember an always decision.
	m1 := NewPersistentApprovalMemory(session.NewRuntime(store, nil))
	if err := m1.Remember(ctx, sessID, "shell", Decision{Action: Allow, Reason: "always"}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	// Instance 2 (restarted process): the decision must be recallable.
	m2 := NewPersistentApprovalMemory(session.NewRuntime(store, nil))
	d, ok := m2.Recall(ctx, sessID, "shell")
	if !ok {
		t.Fatal("decision lost across instances — persistence broken")
	}
	if d.Action != Allow {
		t.Fatalf("action = %v, want Allow", d.Action)
	}

	// Store content check: decisions live under session meta.
	info, _ := store.Get(ctx, sessID)
	approvals, ok := session.GetMeta[map[string]Decision](*info, approvalsKey)
	if !ok || len(approvals) != 1 {
		t.Fatalf("store approvals = %v", approvals)
	}

	// Unknown session/key: no decision.
	if _, ok := m2.Recall(ctx, "s-unknown", "shell"); ok {
		t.Fatal("unexpected decision for unknown session")
	}
}

// jsonRoundTripStore simulates the REAL store behavior (sqlite/file
// persist session meta as JSON): every Save/Get round-trips Meta through
// JSON, so nested values decode as map[string]any instead of their Go
// types. This is the path the old GetMeta assertion silently failed on.
type jsonRoundTripStore struct {
	inner *fakeStore
}

func (s *jsonRoundTripStore) Save(ctx context.Context, info session.SessionInfo) error {
	info.Meta = roundTripJSON(info.Meta)
	return s.inner.Save(ctx, info)
}
func (s *jsonRoundTripStore) Get(ctx context.Context, id string) (*session.SessionInfo, error) {
	info, err := s.inner.Get(ctx, id)
	if err != nil || info == nil {
		return info, err
	}
	info.Meta = roundTripJSON(info.Meta)
	return info, nil
}
func (s *jsonRoundTripStore) List(ctx context.Context) ([]session.SessionInfo, error) { return nil, nil }
func (s *jsonRoundTripStore) Delete(ctx context.Context, id string) error             { return s.inner.Delete(ctx, id) }
func (s *jsonRoundTripStore) Close() error                                            { return nil }

func roundTripJSON(v map[string]any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// TestPersistentApprovalMemory_SurvivesJSONRoundTrip covers the real
// sqlite/file persistence path: after the store round-trips Meta through
// JSON, the decision must still be recallable by a fresh instance.
func TestPersistentApprovalMemory_SurvivesJSONRoundTrip(t *testing.T) {
	store := &jsonRoundTripStore{inner: &fakeStore{}}
	ctx := context.Background()
	sessID := "s1"

	m1 := NewPersistentApprovalMemory(session.NewRuntime(store, nil))
	if err := m1.Remember(ctx, sessID, "shell", Decision{Action: Allow, Reason: "always"}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	// Fresh instance (restart) reading through the JSON round-trip store.
	m2 := NewPersistentApprovalMemory(session.NewRuntime(store, nil))
	d, ok := m2.Recall(ctx, sessID, "shell")
	if !ok {
		t.Fatal("decision lost across JSON round-trip — persistence broken on real stores")
	}
	if d.Action != Allow {
		t.Fatalf("action = %v, want Allow", d.Action)
	}
}

// TestPersistentApprovalMemory_NilStoreFallsBack: without a store the
// memory degrades to in-process (decisions within one instance).
func TestPersistentApprovalMemory_NilStoreFallsBack(t *testing.T) {
	m := NewPersistentApprovalMemory(nil)
	ctx := context.Background()
	if err := m.Remember(ctx, "s1", "read", Decision{Action: Allow}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Recall(ctx, "s1", "read"); !ok {
		t.Fatal("in-process fallback lost decision")
	}
}
