// Package file implements session.Store with a JSON file on disk.
package file

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// Store persists session metadata in a sessions.json file.
type Store struct {
	mu       sync.RWMutex
	path     string
	sessions map[string]session.SessionInfo
}

// New creates a Store backed by dir/sessions.json.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("file sessionstore: %w", err)
	}

	s := &Store{
		path:     filepath.Join(dir, "sessions.json"),
		sessions: make(map[string]session.SessionInfo),
	}

	data, err := os.ReadFile(s.path)
	if err == nil {
		json.Unmarshal(data, &s.sessions)
	}

	return s, nil
}

// ── session.Store ──

func (s *Store) Save(ctx context.Context, info session.SessionInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[info.ID] = info
	return s.flush()
}

func (s *Store) Get(ctx context.Context, id string) (*session.SessionInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info, ok := s.sessions[id]
	if !ok {
		return nil, nil
	}
	return &info, nil
}

func (s *Store) List(ctx context.Context) ([]session.SessionInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]session.SessionInfo, 0, len(s.sessions))
	for _, info := range s.sessions {
		list = append(list, info)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].UpdatedAt.After(list[j].UpdatedAt)
	})
	return list, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return s.flush()
}

func (s *Store) Close() error { return nil }

func (s *Store) flush() error {
	data, err := json.Marshal(s.sessions)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
