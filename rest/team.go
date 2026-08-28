package rest

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/eventbus"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// ── TeamHandler ──

// TeamHandler serves a REST API for an hwcloud Team.
type TeamHandler struct {
	agents []TeamAgentTemplate
	model  hwcloud.Model // from first template, for dynamically added agents and as fallback

	// Model registry, mirroring Handler. Frontend model selection in team mode
	// resolves through this; the chosen model overrides every team agent's
	// model for that run via hwcloud.Session.Model (runner.go:68-70).
	models    map[string]hwcloud.Model // "provider/modelId" → model instance
	modelList []ModelInfo                // ordered list for /models endpoint
	modelsMu  sync.RWMutex

	sm *sessionManager[*teamSessionState] // session CRUD, store, bus
}

// TeamAgentTemplate describes an agent to include in every new team session.
type TeamAgentTemplate struct {
	Name        string
	Description string
	Agent       *agent.Agent // configuration: Model, Instructions, MaxTurns
	Deps        kernel.Deps  // runtime deps: Tools, Memory, Hooks, Observer
}

// NewTeamHandler creates a TeamHandler.
// At least one agent template is required (its Model is used for dynamically added agents).
func NewTeamHandler(mem session.SessionStore, agents ...TeamAgentTemplate) *TeamHandler {
	var model hwcloud.Model
	if len(agents) > 0 {
		model = agents[0].Agent.Model
	}
	for _, t := range agents {
		if t.Agent != nil && t.Agent.Model == nil {
			slog.Warn("team agent has nil model", "agent", t.Name)
		}
	}
	if len(agents) > 0 && model == nil {
		slog.Warn("team primary agent has nil model")
	}

	h := &TeamHandler{agents: agents, model: model, models: make(map[string]hwcloud.Model)}

	bus := eventbus.New[SSEEvent](500)
	h.sm = newSessionManager[*teamSessionState](mem, bus, sessionHooks[*teamSessionState]{
		kind:       "team",
		newEntry:   h.newEntry,
		fillDetail: h.fillDetail,
		onDelete: func(s *teamSessionState) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, tam := range s.agentMems {
				_ = tam.DeleteSession(ctx, s.info.ID)
			}
		},
	})

	return h
}

// RegisterModel adds a model to the team handler's registry, enabling
// frontend model selection in team mode. Mirrors Handler.RegisterModel.
// The chosen model overrides every team agent's model for that run.
func (h *TeamHandler) RegisterModel(id string, model hwcloud.Model, provider string) {
	h.modelsMu.Lock()
	defer h.modelsMu.Unlock()
	// Composite key "provider/modelId" — must match Handler.RegisterModel
	// (they were "provider:id" and "provider/id" respectively; a mixed key
	// would silently miss on lookup).
	h.models[provider+"/"+id] = model
	h.modelList = append(h.modelList, ModelInfo{ID: id, Provider: provider})
}

// lookupModel finds a registered model. Mirrors Handler.lookupModel.
// When provider is non-empty, uses the exact composite key "provider/modelId";
// otherwise scans for the first registered model matching modelId.
func (h *TeamHandler) lookupModel(provider, modelID string) hwcloud.Model {
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

// Register adds the team handler's routes to mux.
func (h *TeamHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /team/sessions", h.handleCreateSession)
	mux.HandleFunc("GET /team/sessions", h.handleListSessions)
	mux.HandleFunc("GET /team/sessions/{id}", h.handleGetSession)
	mux.HandleFunc("GET /team/sessions/{id}/messages", h.handleListMessages)
	mux.HandleFunc("PATCH /team/sessions/{id}", h.handleUpdateSession)
	mux.HandleFunc("DELETE /team/sessions/{id}", h.handleDeleteSession)
	mux.HandleFunc("POST /team/sessions/{id}/chat", h.handleChat)
	mux.HandleFunc("POST /team/sessions/{id}/approve", h.handleApprove)
	mux.HandleFunc("GET /team/sessions/{id}/agents", h.handleListAgents)
	mux.HandleFunc("POST /team/sessions/{id}/agents", h.handleAddAgent)
	mux.HandleFunc("DELETE /team/sessions/{id}/agents", h.handleRemoveAgent)
}

// WithSessionStore attaches a persistent session metadata store.
func (h *TeamHandler) WithSessionStore(s session.Store) *TeamHandler {
	h.sm.SetStore(s)
	return h
}

// StartJanitor starts a background goroutine that evicts idle team session entries.
func (h *TeamHandler) StartJanitor(ctx context.Context, interval, maxIdle time.Duration) {
	h.sm.StartJanitor(ctx, interval, maxIdle)
}

// WithCleanupDir registers a callback invoked when a team session is deleted.
func (h *TeamHandler) WithCleanupDir(fn func(sessionID string)) *TeamHandler {
	h.sm.SetCleanupDir(fn)
	return h
}

// ── teamSessionState ──

type teamSessionState struct {
	info      session.SessionInfo
	team      *agent.Team
	agentList []agentInfo
	agentMems []*teamAgentMemory // per-agent memory wrappers for cleanup

	// approvalMemory persists session-scoped "allow always" decisions.
	approvalMemory governance.ApprovalMemory

	mu              sync.Mutex
	running         bool // true while agent goroutine is active
	pendingApproval *pendingApproval
}

func (s *teamSessionState) sessionInfo() *session.SessionInfo { return &s.info }

// isActive reports whether the team session has an ongoing agent run
// or is awaiting tool approval. Eviction skips active sessions.
func (s *teamSessionState) isActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running || s.pendingApproval != nil
}

// takePending returns and clears the pending approval (nil if none).
func (s *teamSessionState) takePending() *pendingApproval {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pendingApproval
	s.pendingApproval = nil
	return p
}

// setPending parks the approval responder on the session.
func (s *teamSessionState) setPending(p *pendingApproval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingApproval = p
}

type agentInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"` // "internal"
}

// ── Session CRUD ──

func (h *TeamHandler) handleCreateSession(w http.ResponseWriter, r *http.Request) { h.sm.create(w, r) }
func (h *TeamHandler) handleListSessions(w http.ResponseWriter, r *http.Request)  { h.sm.list(w, r) }
func (h *TeamHandler) handleGetSession(w http.ResponseWriter, r *http.Request)    { h.sm.get(w, r) }
func (h *TeamHandler) handleUpdateSession(w http.ResponseWriter, r *http.Request) { h.sm.update(w, r) }
func (h *TeamHandler) handleDeleteSession(w http.ResponseWriter, r *http.Request) { h.sm.del(w, r) }

func (h *TeamHandler) handleListMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	mem := h.sm.Memory()
	if mem == nil {
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

	// Shared partition: user messages, handoffs, agent text responses.
	msgs, err := mem.Recent(ctx, id, limit+before, 0)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch messages"}`, http.StatusInternalServerError)
		return
	}

	// Each agent's private partition: tool calls and tool results.
	s := h.sm.getOrCreate(r.Context(), id)
	for _, tam := range s.agentMems {
		priv, _ := tam.PrivateRecent(ctx, id, limit+before, 0)
		msgs = append(msgs, priv...)
	}

	// Sort by global insertion index to restore chronological order
	// across shared + private partitions.
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Index < msgs[j].Index })
	if before > 0 && len(msgs) > before {
		msgs = msgs[:len(msgs)-before]
	} else if before > 0 {
		msgs = nil
	}
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	if msgs == nil {
		msgs = []hwcloud.Message{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msgs)
}

// ── Chat ──

func (h *TeamHandler) handleChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}

	s := h.sm.getOrCreate(r.Context(), id)

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		http.Error(w, `{"error":"session busy — a run is in progress"}`, http.StatusConflict)
		return
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
	flusher.Flush() // flush headers immediately

	sub := h.sm.Bus().SubscribeLive(id)
	defer h.sm.Bus().Unsubscribe(id, sub)

	// Resolve model: chat-level override > session default > handler default.
	// Mirrors Handler.handleChat (handler.go:268-292). The chosen model is
	// set on hwcloud.Session and overrides every team agent's model for
	// this run (runner.go:68-70: r.runModel = session.Model if non-nil).
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
	model := h.lookupModel(provider, modelID)
	if model == nil {
		model = h.model
	}

	// Persist the resolved model so GET /team/sessions/{id} reflects it.
	if inf, ok := h.sm.withMeta(id, func(inf *session.SessionInfo) {
		inf.SetMeta("modelId", modelID)
		inf.SetMeta("provider", provider)
	}); ok {
		h.sm.syncMeta(inf)
	}

	go func() {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		defer func() {
			// A panic in the run must not kill the server: log it and let
			// the subscriber see an error event instead (mirrors
			// handler.go's run goroutine).
			if rec := recover(); rec != nil {
				slog.Error("team run panicked", "session", id, "panic", rec)
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
			Model:     model,
			CreatedAt: s.info.CreatedAt,
		}

		ch := s.team.RunStream(ctx, oaSession, hwcloud.UserMessage(body.Message))
		for evt := range ch {
			se := teamEventToSSE(evt)
			if se.Type == "" {
				continue
			}
			h.sm.Bus().Publish(id, se)
		}
	}()

	for se := range sub.C {
		if err := writeSSE(w, flusher, se); err != nil {
			return
		}
		if se.Type == "done" || se.Type == "error" {
			return
		}
	}
}

// ── Approve ──

func (h *TeamHandler) handleApprove(w http.ResponseWriter, r *http.Request) {
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

	s := h.sm.getOrCreate(r.Context(), id)

	s.mu.Lock()
	p := s.pendingApproval
	s.pendingApproval = nil
	s.mu.Unlock()

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

// ── Agents ──

func (h *TeamHandler) handleListAgents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s := h.sm.getOrCreate(r.Context(), id)

	s.mu.Lock()
	list := make([]agentInfo, len(s.agentList))
	copy(list, s.agentList)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"agents": list})
}

func (h *TeamHandler) handleAddAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Instructions string `json:"instructions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}

	s := h.sm.getOrCreate(r.Context(), id)

	if h.model == nil {
		http.Error(w, `{"error":"no model available for new agent"}`, http.StatusInternalServerError)
		return
	}

	// Wrap memory for agent-private persistence, same as newEntry.
	var agentMem session.SessionStore
	var tam *teamAgentMemory
	if mem := h.sm.Memory(); mem != nil {
		tam = newTeamAgentMemory(body.Name, mem)
		agentMem = tam
	}
	agentCfg := agent.New(body.Name,
		agent.WithModel(h.model),
		agent.WithSystemPrompts(body.Instructions),
		agent.WithMaxTurns(3),
	)
	agentDeps := kernel.Deps{
		SessionStore: agentMem,
		HumanApprover: &restApprover{
			submit: func(call hwcloud.ToolCall, resp chan approveResponse) {
				h.submitApproval(s, call, resp)
			},
			memory: s.approvalMemory,
		},
	}
	binder := func(tc agent.TeamContext) (agent.AgentRunner, error) {
		// Inject the team's transfer_to_* handoff tools + TeamPrompt
		// (same as the template binder above).
		sub := agentCfg.Clone()
		sub.SystemPrompts = append(sub.SystemPrompts, tc.TeamPrompt)
		deps := agentDeps
		deps.Tools = append(deps.Tools, tc.HandoffTools...)
		return kernel.New(sub, deps), nil
	}
	if err := s.team.AddBinderAgent(body.Name, body.Description, binder); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.agentList = append(s.agentList, agentInfo{
		Name: body.Name, Description: body.Description, Type: "internal",
	})
	if tam != nil {
		s.agentMems = append(s.agentMems, tam)
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "added"})
}

func (h *TeamHandler) handleRemoveAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, `{"error":"name query param is required"}`, http.StatusBadRequest)
		return
	}

	s := h.sm.getOrCreate(r.Context(), id)

	s.team.RemoveAgent(name)

	s.mu.Lock()
	// Clean up the agent's private memory partition.
	filteredMems := s.agentMems[:0]
	for _, tam := range s.agentMems {
		if tam.agentName == name {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			_ = tam.DeleteSession(ctx, id)
			cancel()
		} else {
			filteredMems = append(filteredMems, tam)
		}
	}
	s.agentMems = filteredMems

	// Remove from the agent list.
	filtered := s.agentList[:0]
	for _, a := range s.agentList {
		if a.Name != name {
			filtered = append(filtered, a)
		}
	}
	s.agentList = filtered
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// ── Factory ──

func (h *TeamHandler) newEntry(ctx context.Context, info session.SessionInfo) *teamSessionState {
	s := &teamSessionState{
		info: info,
	}

	teamOpts := make([]agent.TeamOption, 0, len(h.agents)+1)
	teamOpts = append(teamOpts, agent.WithTeamMaxHandoffs(10))

	mem := h.sm.Memory()
	for _, t := range h.agents {
		// Wrap memory so each agent gets agent-private persistence
		// (tool calls/results) separate from team-shared messages
		// (user input, handoffs, text output).
		var agentMem session.SessionStore
		var tam *teamAgentMemory
		if mem != nil {
			tam = newTeamAgentMemory(t.Name, mem)
			agentMem = tam
		} else if t.Deps.SessionStore != nil {
			agentMem = t.Deps.SessionStore
		}
		agentCfg := t.Agent.Clone()
		agentDeps := t.Deps
		agentDeps.SessionStore = agentMem
		agentDeps.HumanApprover = &restApprover{
			submit: func(call hwcloud.ToolCall, resp chan approveResponse) {
				h.submitApproval(s, call, resp)
			},
		}
		binder := func(tc agent.TeamContext) (agent.AgentRunner, error) {
			// Inject the team's transfer_to_* handoff tools and the
			// "## Team Context" prompt block — without them the model
			// cannot hand off nor knows the team.
			sub := agentCfg.Clone()
			sub.SystemPrompts = append(sub.SystemPrompts, tc.TeamPrompt)
			deps := agentDeps
			deps.Tools = append(deps.Tools, tc.HandoffTools...)
			return kernel.New(sub, deps), nil
		}
		teamOpts = append(teamOpts,
			agent.WithTeamAgent(t.Name, t.Description, agentCfg, binder),
		)
		s.agentList = append(s.agentList, agentInfo{
			Name: t.Name, Description: t.Description, Type: "internal",
		})
		if tam != nil {
			s.agentMems = append(s.agentMems, tam)
		}
	}

	s.team = agent.NewTeam(teamOpts...)
	return s
}

// fillDetail enriches the SessionDetail with the team session's ContextWindow,
// derived from the effective model (stored meta override > handler default).
// Mirrors Handler.fillDetail (handler.go:92-96).
func (h *TeamHandler) fillDetail(e *teamSessionState, detail *SessionDetail) {
	provider, _ := session.GetMeta[string](e.info, "provider")
	modelID, _ := session.GetMeta[string](e.info, "modelId")
	m := h.lookupModel(provider, modelID)
	if m == nil {
		m = h.model
	}
	if m != nil {
		detail.ContextWindow = m.ContextWindow()
	}
}

// ── Approval bridge ──

func (h *TeamHandler) submitApproval(s *teamSessionState, call hwcloud.ToolCall, resp chan approveResponse) {
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

	s.mu.Lock()
	s.pendingApproval = &pendingApproval{respond: resp}
	s.mu.Unlock()

	h.sm.Bus().Publish(s.info.ID, evt)
}

// ── TeamEvent → SSE ──

func teamEventToSSE(evt agent.TeamEvent) SSEEvent {
	switch evt.Type {
	case agent.TeamAgentStart:
		return SSEEvent{Type: "agent_start", Agent: evt.Agent}

	case agent.TeamAgentEnd:
		se := SSEEvent{Type: "agent_end", Agent: evt.Agent}
		if evt.Error != nil {
			se.Error = evt.Error.Error()
		}
		return se
	case agent.TeamThought:
		return SSEEvent{Type: "thought", Agent: evt.Agent, Text: evt.Text}

	case agent.TeamTextDelta:
		return SSEEvent{Type: "text_delta", Agent: evt.Agent, Text: evt.Text}

	case agent.TeamToolCall:
		var tcj *SSEToolCall
		if evt.ToolCall != nil {
			tcj = &SSEToolCall{
				ID: evt.ToolCall.ID,
				Function: SSEToolCallFunction{
					Name:      evt.ToolCall.Function.Name,
					Arguments: evt.ToolCall.Function.Arguments,
				},
			}
		}
		return SSEEvent{Type: "tool_call", Agent: evt.Agent, ToolCall: tcj}

	case agent.TeamToolProgress:
		return SSEEvent{Type: "tool_progress", Agent: evt.Agent, ToolCallID: evt.ToolCallID, Text: evt.Text}

	case agent.TeamToolResult:
		return SSEEvent{Type: "tool_result", Agent: evt.Agent, ToolCallID: evt.ToolCallID, Text: evt.Text}

	case agent.TeamRetrying:
		msg := "retrying"
		if evt.Error != nil {
			msg = evt.Error.Error()
		}
		return SSEEvent{Type: "retrying", Agent: evt.Agent, Text: msg}

	case agent.TeamHandoff:
		return SSEEvent{Type: "handoff", Agent: evt.Agent, HandoffTo: evt.Target}

	case agent.TeamDone:
		se := SSEEvent{Type: "done"}
		if evt.Result != nil {
			se.FinalOutput = evt.Result.FinalOutput
			se.PromptTokens = evt.Result.Usage.PromptTokens
		}
		return se

	case agent.TeamError:
		se := SSEEvent{Type: "error"}
		if evt.Error != nil {
			se.Text = evt.Error.Error()
		}
		return se

	default:
		slog.Warn("unknown team event type", "type", evt.Type)
		return SSEEvent{Type: "unknown"}
	}
}
