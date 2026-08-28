package session

import (
	"context"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// fakeStore is an in-memory metadata Store.
type fakeStore struct {
	items map[string]SessionInfo
}

func (f *fakeStore) Save(_ context.Context, info SessionInfo) error {
	if f.items == nil {
		f.items = map[string]SessionInfo{}
	}
	f.items[info.ID] = info
	return nil
}
func (f *fakeStore) Get(_ context.Context, id string) (*SessionInfo, error) {
	if f.items == nil {
		return nil, nil
	}
	if info, ok := f.items[id]; ok {
		return &info, nil
	}
	return nil, nil
}
func (f *fakeStore) List(context.Context) ([]SessionInfo, error) { return nil, nil }
func (f *fakeStore) Delete(_ context.Context, id string) error {
	delete(f.items, id)
	return nil
}
func (f *fakeStore) Close() error { return nil }

// fakeMessages is an in-memory SessionStore.
type fakeMessages struct {
	bySession map[string][]hwcloud.Message
}

func (f *fakeMessages) Append(_ context.Context, sid string, m hwcloud.Message) error {
	if f.bySession == nil {
		f.bySession = map[string][]hwcloud.Message{}
	}
	m.Index = int64(len(f.bySession[sid]))
	f.bySession[sid] = append(f.bySession[sid], m)
	return nil
}
func (f *fakeMessages) Recent(_ context.Context, sid string, n, offset int) ([]hwcloud.Message, error) {
	msgs := f.bySession[sid]
	if offset >= len(msgs) {
		return nil, nil
	}
	start := len(msgs) - n - offset
	if start < 0 {
		start = 0
	}
	return msgs[start:], nil
}
func (f *fakeMessages) RecentAfter(_ context.Context, sid string, throughIndex, n int) ([]hwcloud.Message, error) {
	msgs := f.bySession[sid]
	if throughIndex >= len(msgs) || n <= 0 {
		return nil, nil
	}
	end := throughIndex + n
	if end > len(msgs) {
		end = len(msgs)
	}
	return msgs[throughIndex:end], nil
}
func (f *fakeMessages) Count(_ context.Context, sid string) (int, error) {
	return len(f.bySession[sid]), nil
}
func (f *fakeMessages) DeleteSession(_ context.Context, sid string) error {
	delete(f.bySession, sid)
	return nil
}

// TestSessionRuntime_Lifecycle verifies Create → Save → Checkpoint →
// Restore → Delete.
func TestSessionRuntime_Lifecycle(t *testing.T) {
	meta := &fakeStore{}
	msgs := &fakeMessages{}
	rt := NewRuntime(meta, msgs)
	ctx := context.Background()

	info, err := rt.Create(ctx, "u1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.ID == "" {
		t.Fatal("Create returned empty ID")
	}

	// Add messages + checkpoint.
	_ = msgs.Append(ctx, info.ID, hwcloud.UserMessage("hello"))
	_ = msgs.Append(ctx, info.ID, hwcloud.UserMessage("world"))
	if err := rt.Checkpoint(ctx, info.ID); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got, ok := meta.items[info.ID].Meta["checkpoint_msgs"]; !ok || got.(int64) != 2 {
		t.Fatalf("checkpoint_msgs = %v, want 2", got)
	}

	// Restore.
	restored, refs, ok, err := rt.Restore(ctx, info.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !ok {
		t.Fatal("Restore reported missing session")
	}
	if restored.ID != info.ID {
		t.Fatalf("restored ID = %q", restored.ID)
	}
	if len(refs) != 2 || refs[0].Content != "hello" {
		t.Fatalf("restored refs = %+v", refs)
	}

	// Delete cleans both stores.
	if err := rt.Delete(ctx, info.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := meta.items[info.ID]; ok {
		t.Fatal("metadata not deleted")
	}
	if _, ok := msgs.bySession[info.ID]; ok {
		t.Fatal("messages not deleted")
	}
}

// TestSessionRuntime_RestoreMissingSession verifies ok=false.
func TestSessionRuntime_RestoreMissingSession(t *testing.T) {
	rt := NewRuntime(&fakeStore{}, &fakeMessages{})
	_, _, ok, err := rt.Restore(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for missing session")
	}
}
