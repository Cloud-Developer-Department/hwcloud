package context

import (
	"context"
	"strings"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// fakeProvider is an in-memory MemoryProvider test double.
type fakeProvider struct {
	items []MemoryItem
}

func (f *fakeProvider) Recall(_ context.Context, scope ContextScope, query string, limit int) ([]MemoryEntry, error) {
	var out []MemoryEntry
	for _, it := range f.items {
		if query == "" || matchesQuery(it.Content, query) {
			out = append(out, MemoryEntry{Kind: MemoryKind(it.Kind), Content: it.Content, Score: 1.0})
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// matchesQuery reports whether any query word appears in content.
func matchesQuery(content, query string) bool {
	for _, w := range strings.Fields(query) {
		if strings.Contains(content, w) {
			return true
		}
	}
	return false
}

func (f *fakeProvider) Store(_ context.Context, _ ContextScope, item MemoryItem) error {
	f.items = append(f.items, item)
	return nil
}

// TestKnowledgeLoop_RecallBuild verifies the recall half of the
// self-evolution chain: stored knowledge → next session's Build recalls
// and injects it into the AgentContext. (The extract half is the LLM
// extractor — covered end-to-end by the smoke test.)
func TestKnowledgeLoop_RecallBuild(t *testing.T) {
	prov := &fakeProvider{}
	prov.items = append(prov.items, MemoryItem{
		Kind:    "preference",
		Content: "I prefer terraform for infrastructure.",
	})

	rt := NewContextRuntime(Config{MemoryProvider: prov})
	ac, err := rt.Build(context.Background(), BuildRequest{
		Session:    hwcloud.Session{ID: "s2", UserID: "u1"},
		Goal:       "deploy with terraform",
		WorkingSet: []hwcloud.Message{hwcloud.UserMessage("deploy with terraform")},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(ac.Memories) == 0 {
		t.Fatal("expected recalled knowledge in AgentContext")
	}
	found := false
	for _, m := range ac.Memories {
		if strings.Contains(m.Content, "terraform") {
			found = true
		}
	}
	if !found {
		t.Fatalf("recalled memories missing the terraform preference: %+v", ac.Memories)
	}
}

// TestParseExtractionItems verifies the LLM output parser tolerates
// markdown fences and prose-wrapped JSON.
func TestParseExtractionItems(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{`[{"op":"add","kind":"preference","content":"prefers terraform","topic":"deployment"}]`, 1},
		{"```json\n[{\"op\":\"add\",\"kind\":\"fact\",\"content\":\"uses huawei cloud\",\"topic\":\"cloud\"}]\n```", 1},
		{`Here are the memories: [{"op":"skip","kind":"fact","content":"x","topic":"y"}]`, 1},
		{`not json`, 0},
	}
	for _, c := range cases {
		items, err := parseExtractionItems(c.raw)
		if c.want == 0 {
			if err == nil {
				t.Fatalf("expected error for %q", c.raw)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parse %q: %v", c.raw, err)
		}
		if len(items) != c.want {
			t.Fatalf("parse %q: got %d items, want %d", c.raw, len(items), c.want)
		}
	}
}

// TestExtractor_DisabledNoOp verifies nil extractors are no-ops.
func TestExtractor_DisabledNoOp(t *testing.T) {
	prov := &fakeProvider{}
	ext := NewLLMExtractor(nil, prov) // nil model → disabled
	ext.Extract(context.Background(), ContextScope{}, []hwcloud.Message{
		hwcloud.UserMessage("I prefer nginx."),
	})
	if len(prov.items) != 0 {
		t.Fatal("disabled extractor wrote to provider")
	}
}

// fakeStore is a SessionStore test double.
type fakeStore struct {
	onAppend func(sid string, m hwcloud.Message)
}

func (f *fakeStore) Append(_ context.Context, sid string, m hwcloud.Message) error {
	if f.onAppend != nil {
		f.onAppend(sid, m)
	}
	return nil
}
func (f *fakeStore) Recent(context.Context, string, int, int) ([]hwcloud.Message, error) {
	return nil, nil
}
func (f *fakeStore) Count(context.Context, string) (int, error)  { return 0, nil }
func (f *fakeStore) DeleteSession(context.Context, string) error { return nil }
