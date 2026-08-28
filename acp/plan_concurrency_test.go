package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"strings"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	openacp "github.com/Cloud-Developer-Department/hwcloud/acp/sdk"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/plan"
	"github.com/Cloud-Developer-Department/hwcloud/session"
)

// This file guards against the cross-goroutine data races that the prior
// plan-mode wiring had: the runner runs tool calls in parallel goroutines
// (runner.go executeTools), and the plan_create/plan_update/enter_plan_mode/
// exit_plan_mode closures all mutated ss.mode/planEntries/previousMode/
// injectedPlanTools with only a partial guard (planMu wrapped SendPlanUpdate
// but not the state fields). Under -race this is now clean via the modeMu
// accessors; these tests exercise the real OnPrompt path to prove it.

// ── fake streaming model ──

// fakeStream is a StreamReader yielding a fixed sequence of StreamChunks.
type fakeStream struct {
	chunks []hwcloud.StreamChunk
	i      int
}

func (s *fakeStream) Next() bool {
	if s.i >= len(s.chunks) {
		return false
	}
	s.i++
	return true
}
func (s *fakeStream) Current() hwcloud.StreamChunk { return s.chunks[s.i-1] }
func (s *fakeStream) Err() error                     { return nil }
func (s *fakeStream) Close() error                   { return nil }

// planModel is a Model that plays out scripted turns. turns is a list of
// turns; turn i emits the hwcloud.StreamChunks for turn i, then a final
// "stop" chunk if none of the turn's chunks set FinishReason. After the
// last scripted turn, it returns a stop with no tool calls.
type planModel struct {
	mu    sync.Mutex
	turns [][]hwcloud.StreamChunk
	idx   int
}

func (m *planModel) ChatCompletion(ctx context.Context, req hwcloud.ChatCompletionRequest) (*hwcloud.ChatCompletionResponse, error) {
	return nil, fmt.Errorf("planModel: use ChatCompletionStream")
}

func (m *planModel) ChatCompletionStream(ctx context.Context, req hwcloud.ChatCompletionRequest) (hwcloud.StreamReader, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.turns) {
		return &fakeStream{chunks: []hwcloud.StreamChunk{stopChunk()}}, nil
	}
	chunks := m.turns[m.idx]
	m.idx++
	// Ensure the turn ends with a finish_reason so the runner stops the
	// turn; if the scripted chunks already carry one, don't double up.
	hasFinish := false
	for _, c := range chunks {
		for _, d := range c.Choices {
			if d.FinishReason != "" {
				hasFinish = true
			}
		}
	}
	if !hasFinish {
		chunks = append(append([]hwcloud.StreamChunk{}, chunks...), stopChunk())
	}
	return &fakeStream{chunks: chunks}, nil
}

func (m *planModel) ContextWindow() int { return 128_000 }

func stopChunk() hwcloud.StreamChunk {
	return hwcloud.StreamChunk{
		Choices: []hwcloud.StreamDelta{{FinishReason: "stop", Content: "done"}},
	}
}

// toolCallChunk builds a chunk carrying a complete tool call (id, name,
// args). Index distinguishes multiple tool calls within a turn: the runner
// accumulates ToolCallDeltas by Index into separate ToolCalls.
func toolCallChunk(index int, id, name string, args any) hwcloud.StreamChunk {
	argsJSON, _ := json.Marshal(args)
	return hwcloud.StreamChunk{
		Choices: []hwcloud.StreamDelta{{
			ToolCalls: []hwcloud.ToolCallDelta{{
				Index:    index,
				ID:       id,
				Type:     "function",
				Function: hwcloud.FunctionDelta{Name: name, Arguments: string(argsJSON)},
			}},
		}},
	}
}

// ── fake SessionEventSender (records notifications in order) ──

type recordedUpdate struct {
	kind    string // "plan", "tool_call", "usage_update", ...
	entries []openacp.PlanEntry
	toolID  string
	status  string
}

type recordingSender struct {
	mu      sync.Mutex
	updates []recordedUpdate
}

func (s *recordingSender) record(u recordedUpdate) {
	s.mu.Lock()
	s.updates = append(s.updates, u)
	s.mu.Unlock()
}

func (s *recordingSender) snapshot() []recordedUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedUpdate, len(s.updates))
	copy(out, s.updates)
	return out
}

func (s *recordingSender) SendAgentMessage(text string) error { return nil }
func (s *recordingSender) SendAgentThought(text string) error { return nil }
func (s *recordingSender) SendPlanUpdate(entries []openacp.PlanEntry) error {
	s.record(recordedUpdate{kind: "plan", entries: append([]openacp.PlanEntry{}, entries...)})
	return nil
}
func (s *recordingSender) SendToolCall(tc openacp.ToolCallUpdate) error {
	s.record(recordedUpdate{kind: "tool_call", toolID: tc.ToolCallID, status: tc.Status})
	return nil
}
func (s *recordingSender) SendToolCallWithMeta(tc openacp.ToolCallUpdate, meta map[string]any) error {
	return s.SendToolCall(tc)
}
func (s *recordingSender) SendAvailableCommands(cmds []openacp.AvailableCommand) error { return nil }
func (s *recordingSender) SendModeUpdate(modeID openacp.SessionModeId) error           { return nil }
func (s *recordingSender) SendConfigOptionUpdate(opts []openacp.SessionConfigOption) error {
	return nil
}
func (s *recordingSender) SendUsageUpdate(used, total int, cost *openacp.Cost) error {
	s.record(recordedUpdate{kind: "usage_update"})
	return nil
}
func (s *recordingSender) SendSessionInfo(title string, metadata map[string]any) error { return nil }
func (s *recordingSender) SendHistoryMessage(sessionUpdate, text, messageID string) error {
	return nil
}
func (s *recordingSender) SendHistoryMessageWithMeta(sessionUpdate, text, messageID string, meta map[string]any) error {
	return s.SendHistoryMessage(sessionUpdate, text, messageID)
}

// ── fake session.Store (in-memory) ──

type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]hwcloud_sessionInfoShim
}

type hwcloud_sessionInfoShim struct {
	info session.SessionInfo
}

func newFakeStore() *fakeStore { return &fakeStore{sessions: map[string]hwcloud_sessionInfoShim{}} }

func (s *fakeStore) Save(ctx context.Context, info session.SessionInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[info.ID] = hwcloud_sessionInfoShim{info: info}
	return nil
}
func (s *fakeStore) Get(ctx context.Context, id string) (*session.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[id]
	if !ok {
		return nil, nil
	}
	return &v.info, nil
}
func (s *fakeStore) List(ctx context.Context) ([]session.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]session.SessionInfo, 0, len(s.sessions))
	for _, v := range s.sessions {
		out = append(out, v.info)
	}
	return out, nil
}
func (s *fakeStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}
func (s *fakeStore) Close() error { return nil }

// ── harness ──

// newPlanTestServer builds an AgentServer wired for one session with a
// scripted planModel and a recording sender. Returns the server, the
// session id, the agentSession handle, and the sender.
func newPlanTestServer(t *testing.T, sid string, mode string, turns [][]hwcloud.StreamChunk) (
	*AgentServer, openacp.SessionId, *agentSession, *recordingSender, *planModel,
) {
	t.Helper()
	mdl := &planModel{turns: turns}
	cfg := agent.New("test",
		agent.WithModel(mdl),
		agent.WithSystemPrompts("test"),
		agent.WithMaxTurns(20),
	)
	store := newFakeStore()
	srv := NewAgentServer(cfg, kernel.Deps{}, store, map[string]hwcloud.Model{"test/m": mdl})

	// Build the session directly (avoids needing the SDK mux/client RPC).
	ss := &agentSession{
		id:          openacp.SessionId(sid),
		cwd:         t.TempDir(),
		mode:        mode,
		config:      map[openacp.SessionConfigId]any{"thought_level": "medium", "model": "test/m"},
		firstPrompt: false,
	}
	srv.putSession(openacp.SessionId(sid), ss)

	sender := &recordingSender{}
	return srv, openacp.SessionId(sid), ss, sender, mdl
}

// runPrompt drives OnPrompt once with a text prompt and the recording sender.
func runPrompt(ctx context.Context, srv *AgentServer, sid openacp.SessionId, sender *recordingSender) error {
	req := openacp.PromptRequest{
		SessionID: sid,
		Prompt:    []openacp.ContentBlock{{Type: "text", Text: "go"}},
	}
	_, err := srv.OnPrompt(ctx, req, sender)
	return err
}

// ── tests ──

// TestPlanCreateExitConcurrency: a single model response requests BOTH
// plan_create and exit_plan_mode. Under the old code the parallel tool
// goroutines raced ss.mode/ss.planEntries; under -race this now completes
// cleanly and the plan notifications arrive: entries first, then empty.
func TestPlanCreateExitConcurrency(t *testing.T) {
	sid := "s1"
	turn0 := []hwcloud.StreamChunk{
		toolCallChunk(0, "c1", "plan_create",
			map[string]any{"goal": "g", "steps": []map[string]any{
				{"id": "step-1", "content": "do A", "priority": "high"},
			}}),
		toolCallChunk(1, "e1", "exit_plan_mode", map[string]any{}),
	}
	srv, ssid, ss, sender, _ := newPlanTestServer(t, sid, "plan", [][]hwcloud.StreamChunk{turn0})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runPrompt(ctx, srv, ssid, sender); err != nil && ctx.Err() == nil {
		t.Fatalf("OnPrompt: %v", err)
	}

	// Final mode should be auto (default previous for a session that
	// started in plan). exit_plan_mode clears the client plan panel
	// (sends an empty-plan notification) but does NOT wipe the in-memory
	// planEntries, so we only assert mode here — the entries state is
	// plan_create's responsibility and is asserted separately.
	if got := ss.Mode(); got != "auto" {
		t.Fatalf("final mode = %q, want auto", got)
	}

	// Ordering invariant: IF both a non-empty and an empty plan
	// notification were recorded, the non-empty one must come first
	// (FIFO writer preserves enqueue order, and the modeMu lock makes
	// create's notify + exit's empty-notify mutually ordered). When
	// exit's goroutine wins the race, create runs after the mode flip
	// and (correctly) skips its notify, so only the empty-plan notice
	// is recorded — that is a valid outcome, not an ordering violation.
	ups := sender.snapshot()
	// If both a non-empty and an empty plan notification are present, the
	// non-empty one MUST precede the empty one: create and exit both send
	// while holding modeMu, so the FIFO wire order reflects the lock
	// serialization (create-wins → entries then clear; exit-wins → only
	// clear, no non-empty notice to misorder). An empty-before-nonempty
	// pattern would mean the lock failed to serialize the two sends.
	sawNonEmpty := false
	sawEmptyAfterNonEmpty := false
	for _, u := range ups {
		if u.kind != "plan" {
			continue
		}
		if len(u.entries) > 0 {
			if sawEmptyAfterNonEmpty {
				t.Fatalf("non-empty plan notification after an empty one: lock did not serialize: %v", ups)
			}
			sawNonEmpty = true
		} else {
			if sawNonEmpty {
				sawEmptyAfterNonEmpty = true
			}
		}
	}
}

// TestMultiplePlanUpdateConcurrency: turn 0 seeds a plan, turn 1 issues
// two plan_update calls in one response updating different ids. Asserts
// no race and both statuses land.
func TestMultiplePlanUpdateConcurrency(t *testing.T) {
	sid := "s2"
	turn0 := []hwcloud.StreamChunk{
		toolCallChunk(0, "c1", "plan_create",
			map[string]any{"goal": "g", "steps": []map[string]any{
				{"id": "a", "content": "A", "priority": "high"},
				{"id": "b", "content": "B", "priority": "low"},
			}}),
	}
	turn1 := []hwcloud.StreamChunk{
		toolCallChunk(0, "u1", "plan_update",
			map[string]any{"updates": []map[string]any{
				{"id": "a", "status": "in_progress"},
			}}),
		toolCallChunk(1, "u2", "plan_update",
			map[string]any{"updates": []map[string]any{
				{"id": "b", "status": "completed"},
			}}),
	}
	srv, ssid, ss, _, _ := newPlanTestServer(t, sid, "plan", [][]hwcloud.StreamChunk{turn0, turn1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = runPrompt(ctx, srv, ssid, &recordingSender{})

	entries := ss.PlanEntries()
	byID := map[string]string{}
	for _, e := range entries {
		byID[e.ID] = string(e.Status)
	}
	if byID["a"] != "in_progress" || byID["b"] != "completed" {
		t.Fatalf("plan_update statuses = %v, want a=in_progress b=completed", byID)
	}
}

// TestExitInjectsExecutionToolsSameTurn: exit_plan_mode on turn 0 must
// make execution tools visible to the model on turn 1. We assert by
// capturing the tool definitions the model receives on turn 1 and
// checking for a sentinel execution tool injected via ToolFactory.
func TestExitInjectsExecutionToolsSameTurn(t *testing.T) {
	sid := "s3"

	// A sentinel execution tool, advertised via ToolFactory.
	sentinel := &staticTool{name: "exec_sentinel"}
	turn0 := []hwcloud.StreamChunk{
		toolCallChunk(0, "e1", "exit_plan_mode", map[string]any{}),
	}
	// turn 1: just observe the offered tools by emitting a 'stop'.
	turn1 := []hwcloud.StreamChunk{stopChunk()}
	srv, ssid, ss, _, _ := newPlanTestServer(t, sid, "plan", [][]hwcloud.StreamChunk{turn0, turn1})
	srv.ToolFactory = func(cwd string) []hwcloud.Tool { return []hwcloud.Tool{sentinel} }

	var sawSentinel bool
	mdl := &planModel{turns: [][]hwcloud.StreamChunk{turn0, turn1}}
	// Replace the agent's model with a capturing one to inspect offered tools.
	capturing := &capturingModel{inner: mdl, saw: func(defs []hwcloud.FunctionDefinition) {
		for _, d := range defs {
			if d.Name == "exec_sentinel" {
				sawSentinel = true
			}
		}
	}}
	srv.Cfg.Model = capturing
	srv.Models["test/m"] = capturing
	_ = ss // session exists

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runPrompt(ctx, srv, ssid, &recordingSender{}); err != nil && ctx.Err() == nil {
		t.Fatalf("OnPrompt: %v", err)
	}
	if !sawSentinel {
		t.Fatal("execution tool not offered on turn 1 — injectExecutionTools not visible within the turn")
	}
}

// TestEnterExitEnterRestoresMode: auto → enter → exit → enter → exit
// restores auto each time, previousMode is auto before each exit, no race.
func TestEnterExitEnterRestoresMode(t *testing.T) {
	sid := "s4"
	turn0 := []hwcloud.StreamChunk{toolCallChunk(0, "en1", "enter_plan_mode", map[string]any{})}
	turn1 := []hwcloud.StreamChunk{toolCallChunk(0, "c1", "plan_create",
		map[string]any{"goal": "g", "steps": []map[string]any{
			{"id": "s", "content": "x", "priority": "medium"},
		}})}
	turn2 := []hwcloud.StreamChunk{toolCallChunk(0, "ex1", "exit_plan_mode", map[string]any{})}
	turn3 := []hwcloud.StreamChunk{toolCallChunk(0, "en2", "enter_plan_mode", map[string]any{})}
	turn4 := []hwcloud.StreamChunk{toolCallChunk(0, "ex2", "exit_plan_mode", map[string]any{})}
	turns := [][]hwcloud.StreamChunk{turn0, turn1, turn2, turn3, turn4}
	srv, ssid, ss, _, _ := newPlanTestServer(t, sid, "auto", turns)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = runPrompt(ctx, srv, ssid, &recordingSender{})

	if got := ss.Mode(); got != "auto" {
		t.Fatalf("final mode = %q, want auto after enter→exit→enter→exit", got)
	}
}

// ── helpers for the capturing/sentinel tools ──

// staticTool is a trivial Tool with a fixed name — used as an injectable
// execution-tool sentinel.
type staticTool struct{ name string }

func (t *staticTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{Name: t.name, Parameters: hwcloud.SchemaOf[struct{}]()}
}
func (t *staticTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	return &hwcloud.ToolResult{Content: "ok"}
}

// capturingModel wraps a planModel and records the tool definitions offered
// each call (so a test can assert execution tools became visible).
type capturingModel struct {
	inner *planModel
	saw   func([]hwcloud.FunctionDefinition)
}

func (m *capturingModel) ChatCompletion(ctx context.Context, req hwcloud.ChatCompletionRequest) (*hwcloud.ChatCompletionResponse, error) {
	return m.inner.ChatCompletion(ctx, req)
}
func (m *capturingModel) ChatCompletionStream(ctx context.Context, req hwcloud.ChatCompletionRequest) (hwcloud.StreamReader, error) {
	m.saw(append([]hwcloud.FunctionDefinition{}, req.Tools...))
	return m.inner.ChatCompletionStream(ctx, req)
}
func (m *capturingModel) ContextWindow() int { return m.inner.ContextWindow() }

// keep the plan package referenced (the production code wires plan.NewCreateTool
// etc.; these tests rely on plan tools being offered by OnPrompt).
var _ = plan.StatusPending

// TestApplyPlanUpdatesNoPlan: plan_update without a plan must return an
// actionable error ("create one first"), not the confusing "unknown
// step id" — a model that sees plan_update in a session that never ran
// plan_create would otherwise retry forever against an empty plan.
func TestApplyPlanUpdatesNoPlan(t *testing.T) {
	ss := &agentSession{}
	_, err := ss.ApplyPlanUpdates([]plan.Update{{ID: "1", Status: "in_progress"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no plan in the current session") {
		t.Fatalf("err = %v, want no-plan-yet error", err)
	}
}

// TestApplyPlanUpdatesNoPlanModes: the actionable exit in the error
// depends on the mode — plan mode can create or exit; auto/manual must
// enter_plan_mode first (plan_create/exit_plan_mode are plan-only).
func TestApplyPlanUpdatesNoPlanModes(t *testing.T) {
	planSess := &agentSession{mode: "plan"}
	_, err := planSess.ApplyPlanUpdates([]plan.Update{{ID: "1", Status: "in_progress"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "exit_plan_mode") {
		t.Fatalf("plan-mode err = %v, want exit_plan_mode hint", err)
	}
	autoSess := &agentSession{mode: "auto"}
	_, err = autoSess.ApplyPlanUpdates([]plan.Update{{ID: "1", Status: "in_progress"}}, nil)
	if err == nil || strings.Contains(err.Error(), "exit_plan_mode") || !strings.Contains(err.Error(), "enter_plan_mode") {
		t.Fatalf("auto-mode err = %v, want enter_plan_mode hint, no exit_plan_mode", err)
	}
}
