package rest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
	"github.com/Cloud-Developer-Department/hwcloud/eventbus"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/model/openai"
	wasm "github.com/Cloud-Developer-Department/hwcloud/plugin/agent/wasm"
	"github.com/Cloud-Developer-Department/hwcloud/plugin/wasmhost"
	"github.com/Cloud-Developer-Department/hwcloud/process"
	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// ── Handler ──

// Handler serves a REST API for an hwcloud Agent.
//
// Create with [NewHandler], then register on an [http.ServeMux]:
//
//	handler := rest.NewHandler(agent)
//	mux := http.NewServeMux()
//	handler.Register(mux)
//	http.ListenAndServe(":8080", mux)
//
// ModelInfo describes a registered model for the frontend.
type ModelInfo struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
}

type Handler struct {
	defaultModel hwcloud.Model
	models       map[string]hwcloud.Model // "provider/modelID" → model instance
	modelConfigs map[string]ModelConfig     // "provider/modelID" → original apiKey/baseURL, for SetModel fallback
	modelList    []ModelInfo                // ordered list for /models endpoint
	modelsMu     sync.RWMutex

	cfg       *agent.Agent  // template configuration (cloned per session)
	deps      kernel.Deps   // template runtime deps (copied per session)
	pluginMgr *wasm.Manager // nil = plugins disabled, set by WithPluginManager

	// approverEnabled gates the per-session WithApprover. Default false
	// (no approval step — tools run unapproved); the CLI enables it via
	// --approver=on / settings. Set via WithApproverEnabled.
	approverEnabled bool

	// processBaseDir is the root directory for per-session process output.
	// When set, shell commands write stdout/stderr to files here so the
	// model can monitor long-running processes across turns.
	processBaseDir string

	sm *sessionManager[*sessionState] // session CRUD, store, bus
}

// NewHandler creates a Handler from an agent config and runtime deps.
func NewHandler(cfg *agent.Agent, deps kernel.Deps) *Handler {
	h := &Handler{
		defaultModel:    cfg.Model,
		models:          make(map[string]hwcloud.Model),
		modelConfigs:    make(map[string]ModelConfig),
		modelList:       nil,
		cfg:             cfg.Clone(),
		deps:            deps,
		approverEnabled: true,
	}
	// One shared background extractor per server (never per run).
	if h.deps.Extractor == nil && h.deps.MemoryProvider != nil && h.cfg.Model != nil {
		h.deps.Extractor = ctxpkg.NewAsyncExtractor(ctxpkg.NewLLMExtractor(h.cfg.Model, h.deps.MemoryProvider))
	}

	bus := eventbus.New[SSEEvent](500)
	h.sm = newSessionManager[*sessionState](deps.SessionStore, bus, sessionHooks[*sessionState]{
		kind:       "single",
		newEntry:   h.newEntry,
		fillDetail: h.fillDetail,
		onDelete: func(e *sessionState) {
			if e.processMgr != nil {
				e.processMgr.Cleanup()
			}
		},
	})

	return h
}

// fillDetail enriches the SessionDetail with per-handler runtime fields
// (ContextWindow from the agent's model).
func (h *Handler) fillDetail(e *sessionState, detail *SessionDetail) {
	if e.cfg != nil && e.cfg.Model != nil {
		detail.ContextWindow = e.cfg.Model.ContextWindow()
	}
}

// RegisterModel adds a model to the handler's registry.
// id is the string the frontend sends as modelID (e.g. "deepseek-v3").
// provider identifies which API serves this model (e.g. "deepseek", "openai").
// apiKey and baseURL are stored as the original config, used by SetModel
// (via runtime_set_model_config) to preserve values when only model_id changes.
// The internal key is "provider/id" so different providers can serve the same model name.
func (h *Handler) RegisterModel(id string, model hwcloud.Model, provider, apiKey, baseURL string) {
	h.modelsMu.Lock()
	defer h.modelsMu.Unlock()
	key := provider + "/" + id
	h.models[key] = model
	h.modelConfigs[key] = ModelConfig{Provider: provider, ModelID: id, APIKey: apiKey, BaseURL: baseURL}
	h.modelList = append(h.modelList, ModelInfo{ID: id, Provider: provider})
}

// SetModel replaces or inserts a model in the registry. apiKey and baseURL
// override the originals only when non-empty (update) or are used as-is
// (insert). Used by runtime_set_model_config host export.
func (h *Handler) SetModel(provider, modelID, apiKey, baseURL string, maxInputTokens, maxOutputTokens int) {
	h.modelsMu.Lock()
	defer h.modelsMu.Unlock()
	key := provider + "/" + modelID
	old, ok := h.modelConfigs[key]
	if ok {
		if apiKey == "" {
			apiKey = old.APIKey
		}
		if baseURL == "" {
			baseURL = old.BaseURL
		}
	} else {
		// New model: add to modelList so /models endpoint includes it.
		h.modelList = append(h.modelList, ModelInfo{ID: modelID, Provider: provider})
	}
	cw := maxInputTokens
	if cw == 0 {
		if oldModel, ok := h.models[key]; ok {
			cw = oldModel.ContextWindow()
		}
	}
	m := openai.New(apiKey, modelID, baseURL)
	if cw > 0 {
		m = m.WithContextWindow(cw)
	}
	h.models[key] = m
	h.modelConfigs[key] = ModelConfig{Provider: provider, ModelID: modelID, APIKey: apiKey, BaseURL: baseURL}
}

// SetEmbedding refreshes the embedder's baseURL, apiKey, and model in
// place. Used by runtime_set_embedding_config. No-op when no memory
// provider is configured or the provider does not expose UpdateEmbedder.
func (h *Handler) SetEmbedding(baseURL, apiKey, model string) {
	if u, ok := h.deps.MemoryProvider.(interface{ UpdateEmbedder(string, string, string) }); ok {
		u.UpdateEmbedder(baseURL, apiKey, model)
		slog.Info("rest embedding config updated")
	}
}

// ModelConfig stores the original apiKey/baseURL for a registered model,
// used by SetModel to preserve values when only model_id changes.
type ModelConfig struct {
	Provider string
	ModelID  string
	APIKey   string
	BaseURL  string
}

// lookupModel finds a registered model. When provider is non-empty, it uses
// the exact composite key "provider/modelId". Otherwise it scans all
// registered models for a suffix match on "/modelId" — this handles the
// common case where the frontend sends only modelId without provider.
func (h *Handler) lookupModel(provider, modelID string) hwcloud.Model {
	h.modelsMu.RLock()
	defer h.modelsMu.RUnlock()
	if provider != "" {
		return h.models[provider+"/"+modelID]
	}
	for key, m := range h.models {
		if key == "default" {
			continue
		}
		if strings.HasSuffix(key, "/"+modelID) {
			return m
		}
	}
	return nil
}

// WithPluginManager attaches a WASM plugin manager (agent:observers for
// stage observation, agent:tools for custom tools). Must be called before
// any session is created.
func (h *Handler) WithPluginManager(mgr *wasm.Manager) *Handler {
	h.pluginMgr = mgr
	if obs := mgr.Observer(); obs != nil {
		if h.deps.Observer != nil {
			h.deps.Observer = hwcloud.MultiObserver(h.deps.Observer, obs)
		} else {
			h.deps.Observer = obs
		}
		slog.Info("plugin observer wired", "source", "wasm")
	}
	h.sm.hooks.onDelete = func(e *sessionState) {
		if e.processMgr != nil {
			e.processMgr.Cleanup()
		}
	}
	return h
}

// Register adds the handler's routes to mux using Go 1.22+ patterns.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /sessions", h.handleCreateSession)
	mux.HandleFunc("GET /sessions", h.handleListSessions)
	mux.HandleFunc("GET /sessions/{id}", h.handleGetSession)
	mux.HandleFunc("PATCH /sessions/{id}", h.handleUpdateSession)
	mux.HandleFunc("GET /sessions/{id}/messages", h.handleListMessages)
	mux.HandleFunc("DELETE /sessions/{id}", h.handleDeleteSession)
	mux.HandleFunc("POST /sessions/{id}/chat", h.handleChat)
	mux.HandleFunc("POST /sessions/{id}/approve", h.handleApprove)
	mux.HandleFunc("GET /models", h.handleListModels)
}

// WithSessionStore attaches a persistent session metadata store.
// nil (the default) preserves the current in-memory-only behavior.
func (h *Handler) WithSessionStore(s session.Store) *Handler {
	h.sm.SetStore(s)
	return h
}

// StartJanitor starts a background goroutine that evicts idle session entries.
// See sessionManager.StartJanitor for semantics.
func (h *Handler) StartJanitor(ctx context.Context, interval, maxIdle time.Duration) {
	h.sm.StartJanitor(ctx, interval, maxIdle)
}

// WithCleanupDir registers a callback that is invoked when a session is
// deleted (either via DELETE /sessions/{id} or the idle janitor). Use it
// to clean up per-session temp/artifact directories.
func (h *Handler) WithCleanupDir(fn func(sessionID string)) *Handler {
	h.sm.SetCleanupDir(fn)
	return h
}

// WithApproverEnabled controls whether the human-in-the-loop approver is
// active for per-session agents. Default is true (enabled). Set false to
// auto-approve all tool calls without prompting.
func (h *Handler) WithApproverEnabled(v bool) *Handler {
	h.approverEnabled = v
	return h
}

// WithProcessDir sets the root directory for per-session process output files.
// Shell commands will persist stdout/stderr under <dir>/<sessionID>/.
// When set, long-running commands automatically detach and the model can read
// the output files across turns.
func (h *Handler) WithProcessDir(dir string) *Handler {
	h.processBaseDir = dir
	return h
}

// ── sessionState ──

// sessionState holds the per-session runtime state.
// Events are published to the Handler-level bus via sm so that multiple
// SSE connections (e.g. browser tabs) all receive the full stream.
type sessionState struct {
	info session.SessionInfo // ModelID is the session's model preference; empty → handler default
	cfg  *agent.Agent        // per-session config clone
	deps kernel.Deps         // per-session runtime deps

	// approvalMemory persists session-scoped "allow always" decisions.
	approvalMemory governance.ApprovalMemory

	// processMgr tracks background processes started by the shell tool.
	// Created on session start, cleaned up on deletion.
	processMgr *process.Manager

	mu              sync.Mutex
	running         bool // true while agent goroutine is active
	pendingApproval *pendingApproval
}

func (s *sessionState) sessionInfo() *session.SessionInfo { return &s.info }

// isActive reports whether the session has an ongoing agent run
// or is awaiting tool approval. Eviction skips active sessions.
func (s *sessionState) isActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running || s.pendingApproval != nil
}

// takePending returns and clears the pending approval (nil if none).
func (s *sessionState) takePending() *pendingApproval {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pendingApproval
	s.pendingApproval = nil
	return p
}

// setPending parks the approval responder on the session.
func (s *sessionState) setPending(p *pendingApproval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingApproval = p
}

type pendingApproval struct {
	respond chan approveResponse
}

type approveResponse struct {
	action       string          // allow_once|allow_always|deny|edit (ACP permission option names)
	modifiedArgs json.RawMessage // action=edit
	reason       string
}

// ── Session CRUD handlers ──

func (h *Handler) handleCreateSession(w http.ResponseWriter, r *http.Request) { h.sm.create(w, r) }
func (h *Handler) handleListSessions(w http.ResponseWriter, r *http.Request)  { h.sm.list(w, r) }
func (h *Handler) handleGetSession(w http.ResponseWriter, r *http.Request)    { h.sm.get(w, r) }
func (h *Handler) handleUpdateSession(w http.ResponseWriter, r *http.Request) { h.sm.update(w, r) }
func (h *Handler) handleDeleteSession(w http.ResponseWriter, r *http.Request) { h.sm.del(w, r) }

func (h *Handler) handleListMessages(w http.ResponseWriter, r *http.Request) { h.sm.messages(w, r) }

// parseIntParam parses an integer query parameter with bounds.
func parseIntParam(r *http.Request, name string, min, max int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, fmt.Errorf("missing")
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, fmt.Errorf("invalid integer")
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n, nil
}

// ── Chat handler ──

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}

	s := h.sm.getOrCreate(r.Context(), id)

	// Reject concurrent chats on the same session — two parallel agent
	// runs would interleave their messages in the shared conversation.
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		http.Error(w, `{"error":"session busy — a run is in progress"}`, http.StatusConflict)
		return
	}
	// Reset pending approval for the new chat message.
	if s.pendingApproval != nil {
		slog.Info("discarding pending approval for new chat", "session", id)
	}
	s.running = true
	s.pendingApproval = nil
	s.mu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}
	setSSEHeaders(w)
	flusher.Flush() // flush headers immediately so the client sees streaming start

	// Subscribe to the session's event bus. Live-only — history is NOT
	// replayed because this is a new chat, not a reconnection. Replaying
	// old "done" events would cause the handler to return before the
	// current chat's events arrive.
	sub := h.sm.Bus().SubscribeLive(id)
	defer h.sm.Bus().Unsubscribe(id, sub)

	// Resolve model: chat-level override > session default > handler default.
	provider := body.Provider
	modelID := body.ModelID
	if provider == "" && modelID == "" {
		h.sm.withMeta(id, func(inf *session.SessionInfo) {
			p, _ := session.GetMeta[string](*inf, "provider")
			m, _ := session.GetMeta[string](*inf, "modelId")
			provider = p
			modelID = m
		})
	}
	// Composite key "provider:modelId" for exact match.
	// When provider is empty, find the first registered model for the given ID.
	model := h.lookupModel(provider, modelID)
	if model == nil {
		slog.Warn("unknown model, falling back to default", "session", id, "provider", provider, "model_id", modelID)
		model = h.defaultModel
	}

	// Persist the resolved model so GET /sessions reflects the actual model.
	if inf, ok := h.sm.withMeta(id, func(inf *session.SessionInfo) {
		inf.SetMeta("modelId", modelID)
		inf.SetMeta("provider", provider)
	}); ok {
		h.sm.syncMeta(inf)
	}

	// Start the agent run in a background goroutine. The run context is
	// derived from the request context: when the client disconnects, the
	// run is cancelled (cancel compensation persists in-flight tool
	// results) instead of leaking until the timeout.
	go func() {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		defer func() {
			// A panic in the run must not kill the server: log it and let
			// the subscriber see an error event instead.
			if rec := recover(); rec != nil {
				slog.Error("agent run panicked", "session", id, "panic", rec)
				h.sm.Bus().Publish(id, SSEEvent{Type: "error", Error: "agent run panicked"})
			}
			s.mu.Lock()
			s.running = false
			s.pendingApproval = nil
			s.mu.Unlock()
		}()

		oaSession := hwcloud.Session{
			ID:        id,
			ModelID:   modelID,
			Provider:  provider,
			Model:     model,
			CreatedAt: s.info.CreatedAt,
		}

		rt := kernel.New(s.cfg, s.deps)

		// Inject AgentRuntime into context so runtime_* host exports work
		// in agent:observers / agent:tools plugins during this run.
		if h.pluginMgr != nil {
			wrt := wasm.BuildAgentRuntime(rt, &oaSession, h.SetModel, h.SetEmbedding)
			ctx = wasmhost.WithAgentRuntime(ctx, wrt)
		}

		// Inject ProcessManager so the shell tool can persist
		// long-running process output across turns.
		if s.processMgr != nil {
			ctx = process.WithManager(ctx, s.processMgr)
		}

		ch := rt.RunStream(ctx, oaSession, hwcloud.UserMessage(body.Message))
		for evt := range ch {
			se := streamToSSE(evt)
			select {
			case <-r.Context().Done():
				// Client disconnected — the run context derives from the
				// request, so the run is cancelled here too (cancel
				// compensation persists in-flight tool results).
				return
			default:
			}
			h.sm.Bus().Publish(id, se)
		}
		// Checkpoint: recoverable restore point after a completed run
		// (metadata + message count).
		h.sm.Checkpoint(ctx, id)
	}()

	// Stream events to the SSE response until done/error/disconnect.
	for se := range sub.C {
		if err := writeSSE(w, flusher, se); err != nil {
			return
		}
		if se.Type == "done" || se.Type == "error" {
			return
		}
	}
}

// ── Approve handler ──

func (h *Handler) handleApprove(w http.ResponseWriter, r *http.Request) {
	handleApproveShared(h.sm, w, r)
}

// handleApproveShared is the approval endpoint — shared by the chat and
// orchestrate handlers (they previously carried identical copies).
func handleApproveShared[E sessionEntry](sm *sessionManager[E], w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body ApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid approve request"}`, http.StatusBadRequest)
		return
	}
	switch body.Action {
	case "allow_once", "allow_always", "deny", "edit":
	default:
		http.Error(w, `{"error":"action must be allow_once|allow_always|deny|edit"}`, http.StatusBadRequest)
		return
	}

	s := sm.getOrCreate(r.Context(), id)
	p := s.takePending()

	if p == nil {
		http.Error(w, `{"error":"no pending approval"}`, http.StatusBadRequest)
		return
	}

	resp := approveResponse{action: body.Action, modifiedArgs: body.Args}
	switch body.Action {
	case "deny":
		resp.reason = "denied"
		if body.Feedback != "" {
			resp.reason = "denied: " + body.Feedback
		}
	default:
		resp.reason = "approved"
	}
	p.respond <- resp

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": resp.reason})
}

// ── Models ──

func (h *Handler) handleListModels(w http.ResponseWriter, r *http.Request) {
	h.modelsMu.RLock()
	models := make([]ModelInfo, len(h.modelList))
	copy(models, h.modelList)
	h.modelsMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"models": models})
}

// ── Factory ──

// newEntry creates a fresh sessionState from session.SessionInfo.
// Used by sessionManager when creating or restoring sessions.
func (h *Handler) newEntry(ctx context.Context, info session.SessionInfo) *sessionState {
	s := &sessionState{info: info}

	// Create per-session process manager for tracking long-running shell commands.
	if h.processBaseDir != "" {
		pm, err := process.NewManager(filepath.Join(h.processBaseDir, "sess-"+info.ID))
		if err == nil {
			s.processMgr = pm
		}
	}

	s.cfg = h.cfg.Clone()
	s.deps = h.deps
	s.approvalMemory = governance.NewPersistentApprovalMemory(h.sm.Runtime())
	s.deps.SessionStore = h.sm.Memory()
	s.deps.Observer = nil

	if h.approverEnabled {
		s.deps.HumanApprover = &restApprover{
			submit: func(call hwcloud.ToolCall, resp chan approveResponse) {
				h.submitApproval(s, call, resp)
			},
			memory: s.approvalMemory,
		}
	}
	// Stage observer feeds the SSE bus; combine with the user observer.
	stageObs := &stageObserver{bus: h.sm.Bus(), sid: info.ID}
	if h.deps.Observer != nil {
		s.deps.Observer = hwcloud.MultiObserver(stageObs, h.deps.Observer)
	} else {
		s.deps.Observer = stageObs
	}
	s.deps.Hooks = h.deps.Hooks

	return s
}

// ── Approval bridge ──

type restApprover struct {
	submit func(call hwcloud.ToolCall, resp chan approveResponse)
	memory governance.ApprovalMemory // session-scoped "allow always" persistence
}

// Ask implements governance.HumanApprover — the single approval entry
// point. The UI actions map onto Decisions (ACP allow_once/allow_always
// semantics): allow grants THIS call only (never remembered), always
// persists to the session memory (same tool + args no longer asks this
// session), edit carries the modified args forward.
func (a *restApprover) Ask(ctx context.Context, call hwcloud.ToolCall, def hwcloud.FunctionDefinition, session hwcloud.Session) (governance.Decision, error) {
	resp := make(chan approveResponse, 1)
	a.submit(call, resp)

	var r approveResponse
	select {
	case <-ctx.Done():
		return governance.Decision{Action: governance.Deny, Reason: "cancelled"}, nil
	case r = <-resp:
	}

	switch r.action {
	case "allow_always":
		d := governance.Decision{Action: governance.Allow, Reason: r.reason}
		if a.memory != nil {
			// Multi-key tools (shell command atoms + file accesses,
			// write target) remember every key — the policy chain later
			// requires ALL of them to skip approval.
			keys := governance.MemoryKeys(call.Function.Name, json.RawMessage(call.Function.Arguments))
			if len(keys) == 0 {
				keys = []string{governance.ApprovalKey(call.Function.Name, json.RawMessage(call.Function.Arguments))}
			}
			for _, key := range keys {
				if err := a.memory.Remember(ctx, session.ID, key, d); err != nil {
					slog.Warn("approval always persistence failed", "session", session.ID, "error", err)
				}
			}
		}
		return d, nil
	case "edit":
		return governance.Decision{Action: governance.Allow, Reason: r.reason, ModifiedArgs: r.modifiedArgs}, nil
	case "deny":
		return governance.Decision{Action: governance.Deny, Reason: r.reason}, nil
	case "allow_once": // this call only, never remembered
		return governance.Decision{Action: governance.Allow, Reason: r.reason}, nil
	default:
		// Unrecognized action (malformed request, future protocol value):
		// fail closed — an approval UI hiccup must never auto-execute.
		slog.Warn("approval received unknown action, denying", "action", r.action)
		return governance.Decision{Action: governance.Deny, Reason: "unknown action: " + r.action}, nil
	}
}

func (h *Handler) submitApproval(s *sessionState, call hwcloud.ToolCall, resp chan approveResponse) {
	submitApprovalShared(h.sm, s, call, resp)
}

// submitApprovalShared publishes an approval request and parks the
// responder on the session — shared by the chat and orchestrate handlers.
func submitApprovalShared[E sessionEntry](sm *sessionManager[E], s E, call hwcloud.ToolCall, resp chan approveResponse) {
	tcj := &SSEToolCall{
		ID: call.ID,
		Function: SSEToolCallFunction{
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		},
	}

	evt := SSEEvent{
		Type:     "tool_approval",
		ToolCall: tcj,
	}

	s.setPending(&pendingApproval{respond: resp})
	sm.Bus().Publish(s.sessionInfo().ID, evt)
}

// ── SSE conversion ──

func streamToSSE(evt hwcloud.StreamEvent) SSEEvent {
	switch evt.Type {
	case hwcloud.StreamThought:
		return SSEEvent{Type: "thought", Text: evt.Text}

	case hwcloud.StreamTextDelta:
		return SSEEvent{Type: "text_delta", Text: evt.Text}

	case hwcloud.StreamToolCall:
		// The kernel emits one event per call, but a custom context/execution
		// layer could emit an empty ToolCalls — guard instead of panicking
		// the run goroutine.
		if len(evt.Message.ToolCalls) == 0 {
			return SSEEvent{Type: "tool_call"}
		}
		tc := evt.Message.ToolCalls[0]
		return SSEEvent{
			Type: "tool_call",
			ToolCall: &SSEToolCall{
				ID: tc.ID,
				Function: SSEToolCallFunction{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			},
		}

	case hwcloud.StreamToolResult:
		return SSEEvent{
			Type:       "tool_result",
			ToolCallID: evt.Message.ToolCallID,
			Text:       evt.Message.Content,
		}

	case hwcloud.StreamRetrying:
		msg := "retrying"
		if evt.Error != nil {
			msg = evt.Error.Error()
		}
		return SSEEvent{Type: "retrying", Text: msg}

	case hwcloud.StreamToolProgress:
		return SSEEvent{Type: "tool_progress", Text: evt.Text, ToolCallID: evt.ToolCallID}

	case hwcloud.StreamAborted:
		se := SSEEvent{Type: "aborted"}
		if evt.Error != nil {
			se.Text = evt.Error.Error()
		}
		return se

	case hwcloud.StreamDone:
		se := SSEEvent{Type: "done"}
		if evt.Result != nil {
			se.FinalOutput = evt.Result.FinalOutput
			se.PromptTokens = evt.Result.Usage.PromptTokens
			se.ContextWindow = evt.Result.ContextWindow
		}
		return se

	case hwcloud.StreamError:
		msg := "unknown error"
		if evt.Error != nil {
			msg = evt.Error.Error()
		}
		return SSEEvent{Type: "error", Text: msg}

	default:
		slog.Warn("unknown stream event type", "type", evt.Type)
		return SSEEvent{Type: "unknown"}
	}
}

// ── Helpers ──

// idSeq disambiguates session ids when crypto/rand fails (all-zero hex
// would collide across sessions).
var idSeq atomic.Uint64

func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), idSeq.Add(1))
	}
	return hex.EncodeToString(b)
}

// stageObserver publishes pipeline stage events to the SSE bus so
// frontends can render a live pipeline visualization.
type stageObserver struct {
	bus *eventbus.Bus[SSEEvent]
	sid string
}

func (o *stageObserver) ObserveStage(ctx context.Context, evt hwcloud.StageEvent) {
	sd := struct {
		Name       string         `json:"name"`
		Phase      string         `json:"phase"`
		Detail     map[string]any `json:"detail,omitempty"`
		DurationMs int64          `json:"duration_ms,omitempty"`
		Err        string         `json:"error,omitempty"`
	}{
		Name:   evt.Name,
		Phase:  evt.Phase,
		Detail: evt.Detail,
	}
	if evt.Phase == "leave" {
		sd.DurationMs = evt.Duration.Milliseconds()
	}
	if evt.Err != nil {
		sd.Err = evt.Err.Error()
	}
	b, err := json.Marshal(sd)
	if err != nil {
		o.bus.Publish(o.sid, SSEEvent{Type: "error", Text: "stage marshal failed: " + err.Error()})
		return
	}
	o.bus.Publish(o.sid, SSEEvent{Type: "stage", Stage: b})
}

var _ hwcloud.RunObserver = (*stageObserver)(nil)
