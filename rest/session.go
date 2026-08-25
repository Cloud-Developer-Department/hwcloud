package rest

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/eventbus"
	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// sessionEntry is implemented by every session state type.
type sessionEntry interface {
	sessionInfo() *session.SessionInfo
	isActive() bool
	// Approval slot: the chat and orchestrate sessions share the same
	// single pending-approval mechanism (one dialog at a time).
	takePending() *pendingApproval
	setPending(p *pendingApproval)
}

// sessionHooks parameterises sessionManager by entry type E.
type sessionHooks[E sessionEntry] struct {
	kind       string
	newEntry   func(ctx context.Context, info session.SessionInfo) E
	fillDetail func(e E, detail *SessionDetail)
	onDelete   func(e E)
	cleanupDir func(sessionID string)
}

// ── sessionManager ──

// sessionManager handles session CRUD, store-backed restores,
// message listing, and bus subscriptions for a single mode.
//
// Lifecycle operations (create/save/delete/checkpoint) go through the
// session.Runtime API — one explicit entry instead of juggling the
// metadata Store and message SessionStore separately. The message store
// stays reachable for listing (the messages endpoint needs full
// messages, not the lightweight Restore refs).
type sessionManager[E sessionEntry] struct {
	memory  session.SessionStore
	runtime session.Runtime // nil until SetStore
	bus     *eventbus.Bus[SSEEvent]
	hooks   sessionHooks[E]

	mu         sync.RWMutex
	entries    map[string]E
	lastAccess map[string]time.Time
}

func newSessionManager[E sessionEntry](
	memory session.SessionStore,
	bus *eventbus.Bus[SSEEvent],
	hooks sessionHooks[E],
) *sessionManager[E] {
	return &sessionManager[E]{
		memory:     memory,
		bus:        bus,
		hooks:      hooks,
		entries:    make(map[string]E),
		lastAccess: make(map[string]time.Time),
	}
}

func (sm *sessionManager[E]) SetStore(s session.Store) {
	sm.runtime = session.NewRuntime(s, sm.memory)
}
func (sm *sessionManager[E]) SetCleanupDir(fn func(string)) { sm.hooks.cleanupDir = fn }
func (sm *sessionManager[E]) Bus() *eventbus.Bus[SSEEvent]  { return sm.bus }
func (sm *sessionManager[E]) Memory() session.SessionStore  { return sm.memory }
func (sm *sessionManager[E]) Runtime() session.Runtime      { return sm.runtime }

// Checkpoint marks the current message count as a recoverable restore
// point (crash recovery). Called after a completed run.
func (sm *sessionManager[E]) Checkpoint(ctx context.Context, id string) {
	if sm.runtime == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sm.runtime.Checkpoint(ctx, id); err != nil {
		slog.Warn("session checkpoint failed", "session", id, "error", err)
	}
}

func (sm *sessionManager[E]) Exists(id string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	_, ok := sm.entries[id]
	return ok
}

func (sm *sessionManager[E]) withMeta(id string, fn func(*session.SessionInfo)) (*session.SessionInfo, bool) {
	sm.mu.RLock()
	e, ok := sm.entries[id]
	if !ok {
		sm.mu.RUnlock()
		return nil, false
	}
	fn(e.sessionInfo())
	out := *e.sessionInfo()
	sm.mu.RUnlock()
	sm.touch(id)
	return &out, true
}

func (sm *sessionManager[E]) touch(id string) {
	sm.mu.Lock()
	sm.lastAccess[id] = time.Now()
	sm.mu.Unlock()
}

func (sm *sessionManager[E]) syncMeta(inf *session.SessionInfo) {
	if sm.runtime == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sm.runtime.Save(ctx, *inf); err != nil {
			slog.Error("failed to persist session meta", "session", inf.ID, "error", err)
		}
	}()
}

// ── HTTP handlers ──

func (sm *sessionManager[E]) create(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid session request"}`, http.StatusBadRequest)
			return
		}
	}

	id := generateID()
	now := time.Now()
	info := session.SessionInfo{
		ID:        id,
		Title:     req.Title,
		CreatedAt: now,
		UpdatedAt: now,
	}
	info.SetMeta("kind", sm.hooks.kind)
	info.SetMeta("modelId", req.ModelID)
	info.SetMeta("provider", req.Provider)

	sm.mu.Lock()
	sm.entries[id] = sm.hooks.newEntry(r.Context(), info)
	sm.mu.Unlock()

	sm.syncMeta(&info)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(info)
}

func (sm *sessionManager[E]) list(w http.ResponseWriter, r *http.Request) {
	if sm.runtime != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		list, err := sm.runtime.List(ctx)
		if err != nil {
			http.Error(w, `{"error":"failed to list sessions"}`, http.StatusInternalServerError)
			return
		}
		filtered := make([]session.SessionInfo, 0, len(list))
		for _, s := range list {
			k, _ := session.GetMeta[string](s, "kind")
			if k == sm.hooks.kind {
				filtered = append(filtered, s)
			}
		}
		if filtered == nil {
			filtered = []session.SessionInfo{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(filtered)
		return
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	list := make([]session.SessionInfo, 0, len(sm.entries))
	for _, e := range sm.entries {
		list = append(list, *e.sessionInfo())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (sm *sessionManager[E]) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	sm.mu.RLock()
	e, ok := sm.entries[id]
	sm.mu.RUnlock()

	if ok {
		detail := SessionDetail{SessionInfo: *e.sessionInfo()}
		if sm.hooks.fillDetail != nil {
			sm.hooks.fillDetail(e, &detail)
		}
		if sm.memory != nil {
			if n, _ := sm.memory.Count(r.Context(), id); n > 0 {
				detail.MessageCount = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(detail)
		return
	}

	if sm.runtime != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		stored, err := sm.runtime.Get(ctx, id)
		if err != nil {
			http.Error(w, `{"error":"failed to get session"}`, http.StatusInternalServerError)
			return
		}
		if stored != nil {
			detail := SessionDetail{SessionInfo: *stored}
			if sm.memory != nil {
				if n, _ := sm.memory.Count(r.Context(), id); n > 0 {
					detail.MessageCount = n
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(detail)
			return
		}
	}

	http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
}

func (sm *sessionManager[E]) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}

	if sm.runtime != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if stored, err := sm.runtime.Get(ctx, id); err == nil && stored != nil {
			sm.getOrCreate(r.Context(), id)
		}
	}

	inf, ok := sm.withMeta(id, func(inf *session.SessionInfo) {
		inf.Title = body.Title
		if body.Title != "" {
			inf.UpdatedAt = time.Now()
		}
	})
	if !ok {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}
	sm.syncMeta(inf)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(*inf)
}

func (sm *sessionManager[E]) messages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if sm.memory == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]hwcloud.Message{})
		return
	}

	limit := 50
	if l, err := parseIntParam(r, "limit", 1, 200); err == nil {
		limit = l
	}
	before := 0
	if b, err := parseIntParam(r, "before", 0, 100000); err == nil {
		before = b
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	msgs, err := sm.memory.Recent(ctx, id, limit, before)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch messages"}`, http.StatusInternalServerError)
		return
	}
	if msgs == nil {
		msgs = []hwcloud.Message{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msgs)
}

func (sm *sessionManager[E]) del(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Runtime.Delete removes metadata and messages together.
	if sm.runtime != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := sm.runtime.Delete(ctx, id); err != nil {
			slog.Error("failed to delete session", "session", id, "error", err)
		}
	}

	sm.mu.Lock()
	e, ok := sm.entries[id]
	if ok && sm.hooks.onDelete != nil {
		sm.hooks.onDelete(e)
	}
	delete(sm.entries, id)
	delete(sm.lastAccess, id)
	sm.mu.Unlock()

	sm.bus.RemoveTopic(id)

	if sm.hooks.cleanupDir != nil {
		sm.hooks.cleanupDir(id)
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Idle eviction ──

func (sm *sessionManager[E]) StartJanitor(ctx context.Context, interval, maxIdle time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sm.evictIdle(maxIdle)
			}
		}
	}()
}

func (sm *sessionManager[E]) evictIdle(maxIdle time.Duration) {
	now := time.Now()
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for id, e := range sm.entries {
		last, ok := sm.lastAccess[id]
		if !ok || now.Sub(last) < maxIdle {
			continue
		}
		if e.isActive() {
			continue
		}

		if sm.runtime != nil {
			info := *e.sessionInfo()
			info.UpdatedAt = now
			go func(inf session.SessionInfo) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := sm.runtime.Save(ctx, inf); err != nil {
					slog.Error("failed to persist session before eviction", "session", id, "error", err)
				}
			}(info)
		}
		if sm.hooks.onDelete != nil {
			sm.hooks.onDelete(e)
		}

		delete(sm.entries, id)
		delete(sm.lastAccess, id)
	}
}

// getOrCreate returns the existing entry or creates a new one.
func (sm *sessionManager[E]) getOrCreate(ctx context.Context, id string) E {
	sm.mu.Lock()
	if e, ok := sm.entries[id]; ok {
		sm.lastAccess[id] = time.Now()
		sm.mu.Unlock()
		return e
	}
	sm.mu.Unlock()

	var info session.SessionInfo
	restored := false
	if sm.runtime != nil {
		// Bound the restore by the caller's context (the request) plus a
		// cap — a slow store must not hold the request forever.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if stored, err := sm.runtime.Get(ctx, id); err == nil && stored != nil {
			info = *stored
			restored = true
		}
	}

	if !restored {
		now := time.Now()
		info = session.SessionInfo{
			ID:        id,
			CreatedAt: now,
			UpdatedAt: now,
		}
		info.SetMeta("kind", sm.hooks.kind)
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	if e, ok := sm.entries[id]; ok {
		return e
	}

	e := sm.hooks.newEntry(ctx, info)
	sm.entries[id] = e
	sm.lastAccess[id] = time.Now()

	if !restored && sm.runtime != nil {
		sm.syncMeta(&info)
	}

	return e
}
