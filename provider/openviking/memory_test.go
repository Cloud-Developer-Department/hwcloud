package openviking

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
)

// TestRecall_EndpointAndParsing: the Memory provider calls
// /api/v1/search/recall (not /search) with the configured quotas, and
// parses entries into MemoryEntry. Content falls back to summary →
// abstract → uri when the server returns mode="uri" (no full content).
func TestRecall_EndpointAndParsing(t *testing.T) {
	var gotPath string
	var gotBody recallRequest

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search/recall", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		resp := map[string]any{
			"status": "ok",
			"result": map[string]any{
				"entries": []map[string]any{
					{
						"uri":     "viking://user/default/memories/preferences/deploy.md",
						"score":   0.92,
						"type":    "preferences",
						"mode":    "full",
						"content": "I prefer terraform for infrastructure.",
					},
					{
						"uri":     "viking://user/default/memories/events/chat-2024.md",
						"score":   0.71,
						"type":    "events",
						"mode":    "summary",
						"summary": "Discussed kubectl rollout restart.",
						"abstract": "kubectl rollout event",
					},
					{
						"uri":     "viking://user/default/memories/entities/port.md",
						"score":   0.55,
						"type":    "entities",
						"mode":    "uri",
						"abstract": "port 5432",
					},
				},
				"rendered": "",
				"stats":    map[string]any{},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemoryWithRecall(client, RecallConfig{
		Quotas:   map[string]int{"events": 8, "entities": 8, "preferences": 2, "experiences": 0},
		MaxChars: 3000,
		MinScore: 0.2,
	})

	entries, err := mem.Recall(context.Background(), ctxpkg.ContextScope{}, "terraform deploy", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	if gotPath != "/api/v1/search/recall" {
		t.Errorf("requested %q, want /api/v1/search/recall", gotPath)
	}
	if gotBody.Query != "terraform deploy" {
		t.Errorf("query = %q, want %q", gotBody.Query, "terraform deploy")
	}
	if gotBody.MaxChars != 3000 {
		t.Errorf("max_chars = %d, want 3000", gotBody.MaxChars)
	}
	if gotBody.MinScore != 0.2 {
		t.Errorf("min_score = %v, want 0.2", gotBody.MinScore)
	}
	if gotBody.Render != false {
		t.Errorf("render = %v, want false (hwcloud renders itself)", gotBody.Render)
	}
	if gotBody.Quotas["preferences"] != 2 {
		t.Errorf("preferences quota = %d, want 2", gotBody.Quotas["preferences"])
	}
	if gotBody.Quotas["events"] != 8 {
		t.Errorf("events quota = %d, want 8", gotBody.Quotas["events"])
	}

	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}

	cases := []struct {
		content string
		score   float64
		kind    string
	}{
		{"I prefer terraform for infrastructure.", 0.92, "preferences"},
		{"Discussed kubectl rollout restart.", 0.71, "events"},
		{"port 5432", 0.55, "entities"},
	}
	for i, c := range cases {
		if entries[i].Content != c.content {
			t.Errorf("entry[%d] content = %q, want %q", i, entries[i].Content, c.content)
		}
		if entries[i].Score != c.score {
			t.Errorf("entry[%d] score = %v, want %v", i, entries[i].Score, c.score)
		}
		if string(entries[i].Kind) != c.kind {
			t.Errorf("entry[%d] kind = %q, want %q", i, entries[i].Kind, c.kind)
		}
	}
}

// TestRecall_DefaultQuotas: NewMemory (no explicit config) uses the
// server-default quotas so a bare endpoint produces non-empty results.
func TestRecall_DefaultQuotas(t *testing.T) {
	var gotBody recallRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search/recall", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"result": map[string]any{"entries": []any{}, "rendered": "", "stats": map[string]any{}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemory(client)
	_, _ = mem.Recall(context.Background(), ctxpkg.ContextScope{}, "test", 5)

	if gotBody.Quotas["events"] != 10 {
		t.Errorf("default events quota = %d, want 10", gotBody.Quotas["events"])
	}
	if gotBody.Quotas["entities"] != 10 {
		t.Errorf("default entities quota = %d, want 10", gotBody.Quotas["entities"])
	}
	if gotBody.Quotas["preferences"] != 3 {
		t.Errorf("default preferences quota = %d, want 3", gotBody.Quotas["preferences"])
	}
	if gotBody.Quotas["experiences"] != 0 {
		t.Errorf("default experiences quota = %d, want 0", gotBody.Quotas["experiences"])
	}
	if gotBody.MaxChars != 6500 {
		t.Errorf("default max_chars = %d, want 6500", gotBody.MaxChars)
	}
	if gotBody.MinScore != 0.1 {
		t.Errorf("default min_score = %v, want 0.1", gotBody.MinScore)
	}
}

// TestRecall_EmptyEntries: an empty recall result is not an error — the
// caller (context runtime) treats nil entries as "no memories".
func TestRecall_EmptyEntries(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search/recall", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"result": map[string]any{"entries": []any{}, "rendered": "", "stats": map[string]any{}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemory(client)
	entries, err := mem.Recall(context.Background(), ctxpkg.ContextScope{}, "nothing matches", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

// TestRecall_URIFallback: when the server returns mode="uri" with no
// content/summary/abstract, the entry's content falls back to the URI so
// the caller always gets useful text.
func TestRecall_URIFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search/recall", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"result": map[string]any{
				"entries": []map[string]any{
					{
						"uri":  "viking://user/default/memories/entities/redis.md",
						"score": 0.4,
						"type": "entities",
						"mode": "uri",
					},
				},
				"rendered": "",
				"stats":    map[string]any{},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemory(client)
	entries, err := mem.Recall(context.Background(), ctxpkg.ContextScope{}, "redis", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Content != "viking://user/default/memories/entities/redis.md" {
		t.Errorf("content = %q, want URI fallback", entries[0].Content)
	}
}
