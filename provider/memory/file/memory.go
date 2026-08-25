// Package file implements the durable knowledge provider
// (context.MemoryProvider) with a JSONL file on disk, zero external
// dependencies. Conversation storage lives in session/file — this package
// is the memory domain only.
package file

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
)

// Memory implements context.MemoryProvider backed by a knowledge.jsonl
// file in the given directory.
type Memory struct {
	dir string
	mu  sync.RWMutex
}

// New creates a knowledge store at dir. Directory is created if missing.
func New(dir string) (*Memory, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("file knowledge: %w", err)
	}
	return &Memory{dir: dir}, nil
}

// ── context.MemoryProvider ──

var _ ctxpkg.MemoryProvider = (*Memory)(nil)

// Recall returns durable knowledge items matching the query, scoped by
// user/project. Case-insensitive substring matching with prefix bonus.
func (m *Memory) Recall(ctx context.Context, scope ctxpkg.ContextScope, query string, limit int) ([]ctxpkg.MemoryEntry, error) {
	if limit <= 0 {
		limit = 5
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, err := os.ReadFile(m.knowledgePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("file knowledge read: %w", err)
	}
	needle := strings.ToLower(query)
	if needle == "" {
		// Empty query matches everything via strings.Contains — return
		// nothing instead (consistent with the sqlite provider).
		return nil, nil
	}
	var entries []ctxpkg.MemoryEntry
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			ScopeUser    string `json:"scope_user"`
			ScopeProject string `json:"scope_project"`
			ScopeSession string `json:"scope_session"`
			Kind         string `json:"kind"`
			Content      string `json:"content"`
			Topic        string `json:"topic"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			// A corrupt line is data loss — log instead of silently
			// skipping.
			slog.Warn("skipping corrupt knowledge line", "error", err)
			continue
		}
		if rec.ScopeUser != "" && rec.ScopeUser != scope.UserID {
			continue
		}
		if rec.ScopeProject != "" && rec.ScopeProject != scope.ProjectID {
			continue
		}
		// Session scope matches the sqlite provider: a non-empty scope
		// session sees its own entries plus shared ("") ones.
		if scope.SessionID != "" && rec.ScopeSession != "" && rec.ScopeSession != scope.SessionID {
			continue
		}
		if !strings.Contains(strings.ToLower(rec.Content), needle) {
			continue
		}
		score := 0.5
		if strings.HasPrefix(strings.ToLower(rec.Content), needle) {
			score = 1.0
		}
		entries = append(entries, ctxpkg.MemoryEntry{
			Kind:    ctxpkg.MemoryKind(rec.Kind),
			Content: rec.Content,
			Topic:   rec.Topic,
			Score:   score,
		})
		if len(entries) >= limit {
			break
		}
	}
	return entries, nil
}

// Store persists a durable knowledge item to the knowledge JSONL file.
// A non-empty Topic upserts: the record with the same (scope, topic) is
// replaced (the extractor's "update" operation); empty topic appends.
func (m *Memory) Store(ctx context.Context, scope ctxpkg.ContextScope, item ctxpkg.MemoryItem) error {
	record := struct {
		ScopeUser    string         `json:"scope_user"`
		ScopeProject string         `json:"scope_project"`
		ScopeSession string         `json:"scope_session"`
		Kind         string         `json:"kind"`
		Content      string         `json:"content"`
		Topic        string         `json:"topic"`
		Meta         map[string]any `json:"meta,omitempty"`
	}{scope.UserID, scope.ProjectID, scope.SessionID, item.Kind, item.Content, item.Topic, item.Meta}
	b, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("file knowledge marshal: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if item.Topic != "" {
		// Upsert: rewrite the file, replacing the record with the same
		// (scope_user, scope_project, scope_session, topic) — the session
		// is part of the key, matching the sqlite provider.
		data, err := os.ReadFile(m.knowledgePath())
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("file knowledge read: %w", err)
		}
		var out []string
		replaced := false
		for _, line := range strings.Split(string(data), "\n") {
			if line == "" {
				continue
			}
			var rec struct {
				ScopeUser    string `json:"scope_user"`
				ScopeProject string `json:"scope_project"`
				ScopeSession string `json:"scope_session"`
				Topic        string `json:"topic"`
			}
			if json.Unmarshal([]byte(line), &rec) == nil &&
				rec.ScopeUser == scope.UserID && rec.ScopeProject == scope.ProjectID &&
				rec.ScopeSession == scope.SessionID && rec.Topic == item.Topic {
				if !replaced {
					out = append(out, string(b))
					replaced = true
				}
				continue
			}
			out = append(out, line)
		}
		if !replaced {
			out = append(out, string(b))
		}
		return m.writeLines(out)
	}

	f, err := os.OpenFile(m.knowledgePath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("file knowledge open: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("file knowledge write: %w", err)
	}
	return nil
}

// writeLines rewrites the knowledge file with the given lines (atomic:
// tmp + rename so a crash mid-write cannot truncate the knowledge file).
func (m *Memory) writeLines(lines []string) error {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	path := m.knowledgePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0644); err != nil {
		return fmt.Errorf("file knowledge write: %w", err)
	}
	return os.Rename(tmp, path)
}

// knowledgePath returns the durable-knowledge file path.
func (m *Memory) knowledgePath() string {
	return filepath.Join(m.dir, "knowledge.jsonl")
}
