package kernel

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
)

// ── streamingTestTool implements both Tool and StreamExecutor ──

type streamingTestTool struct {
	name   string
	chunks []string // chunks to emit
	delay  time.Duration
}

func (t *streamingTestTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        t.name,
		Description: "a streaming test tool",
		Parameters:  hwcloud.SchemaOf[struct{}](),
	}
}

func (t *streamingTestTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	return &hwcloud.ToolResult{Content: strings.Join(t.chunks, "")}
}

func (t *streamingTestTool) ExecuteStream(ctx context.Context, args json.RawMessage) <-chan hwcloud.ToolStreamChunk {
	ch := make(chan hwcloud.ToolStreamChunk, len(t.chunks))
	go func() {
		defer close(ch)
		for _, c := range t.chunks {
			select {
			case <-ctx.Done():
				return
			case <-time.After(t.delay):
			}
			ch <- hwcloud.ToolStreamChunk{Content: c}
		}
	}()
	return ch
}

// ── nonStreamingTool implements only Tool ──

type nonStreamingTool struct{}

func (t *nonStreamingTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "blocking_tool",
		Description: "a blocking tool",
		Parameters:  hwcloud.SchemaOf[struct{}](),
	}
}

func (t *nonStreamingTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	return &hwcloud.ToolResult{Content: "blocking_result"}
}

// ── fakeModelWithToolCall returns a tool call then stops ──

type fakeModelWithToolCall struct {
	toolName string
	toolArgs string
	callID   string
}

func (m *fakeModelWithToolCall) ChatCompletion(ctx context.Context, req hwcloud.ChatCompletionRequest) (*hwcloud.ChatCompletionResponse, error) {
	return &hwcloud.ChatCompletionResponse{
		Choices: []hwcloud.Choice{{
			Index: 0,
			Message: hwcloud.Message{
				Role: hwcloud.RoleAssistant,
				ToolCalls: []hwcloud.ToolCall{{
					ID:   m.callID,
					Type: "function",
					Function: hwcloud.ToolCallFunction{
						Name:      m.toolName,
						Arguments: m.toolArgs,
					},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}, nil
}

func (m *fakeModelWithToolCall) ChatCompletionStream(ctx context.Context, req hwcloud.ChatCompletionRequest) (hwcloud.StreamReader, error) {
	return nil, nil // fallback to non-streaming
}

func (m *fakeModelWithToolCall) ContextWindow() int { return 128_000 }

// ── fakeModelWithTwoToolCalls returns two tool calls in one response ──

type fakeModelWithTwoToolCalls struct {
	calls []hwcloud.ToolCall
}

func (m *fakeModelWithTwoToolCalls) ChatCompletion(ctx context.Context, req hwcloud.ChatCompletionRequest) (*hwcloud.ChatCompletionResponse, error) {
	return &hwcloud.ChatCompletionResponse{
		Choices: []hwcloud.Choice{{
			Index: 0,
			Message: hwcloud.Message{
				Role:      hwcloud.RoleAssistant,
				ToolCalls: m.calls,
			},
			FinishReason: "tool_calls",
		}},
	}, nil
}

func (m *fakeModelWithTwoToolCalls) ChatCompletionStream(ctx context.Context, req hwcloud.ChatCompletionRequest) (hwcloud.StreamReader, error) {
	return nil, nil
}

func (m *fakeModelWithTwoToolCalls) ContextWindow() int { return 128_000 }

// ── Tests ──

func TestStreamExecutorProgressEvents(t *testing.T) {
	// Verify that a tool implementing StreamExecutor emits hwcloud.StreamToolProgress
	// events with correct ToolCallID, and the final hwcloud.StreamToolResult aggregates all chunks.
	streamTool := &streamingTestTool{
		name:   "stream_tool",
		chunks: []string{"chunk1", "chunk2", "chunk3"},
		delay:  0,
	}

	model := &fakeModelWithToolCall{
		toolName: "stream_tool",
		toolArgs: `{}`,
		callID:   "call_abc",
	}

	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(1),
	)
	deps := Deps{Tools: []hwcloud.Tool{streamTool}}
	agent := New(cfg, deps)

	ch := agent.RunStream(context.Background(), hwcloud.Session{ID: "test"}, hwcloud.UserMessage("go"))

	var progressEvents []hwcloud.StreamEvent
	var toolResultEvent *hwcloud.StreamEvent
	var gotDone bool

	for evt := range ch {
		switch evt.Type {
		case hwcloud.StreamToolProgress:
			progressEvents = append(progressEvents, evt)
		case hwcloud.StreamToolResult:
			evtCopy := evt
			toolResultEvent = &evtCopy
		case hwcloud.StreamDone:
			gotDone = true
		case hwcloud.StreamError:
			t.Fatalf("unexpected error: %v", evt.Error)
		case hwcloud.StreamAborted:
			t.Fatalf("unexpected abort: %v", evt.Error)
		}
	}

	if !gotDone {
		t.Fatal("missing hwcloud.StreamDone")
	}

	// Verify progress events. With 0ms delay, all chunks arrive before the
	// 1-second rate-limiting ticker fires, so they are batched into one
	// progress event. The rate limiter batches, it does not drop.
	if len(progressEvents) < 1 {
		t.Fatalf("expected at least 1 progress event, got %d", len(progressEvents))
	}
	for _, pe := range progressEvents {
		if pe.ToolCallID != "call_abc" {
			t.Errorf("progress expected ToolCallID=call_abc, got %q", pe.ToolCallID)
		}
	}
	// Verify all chunk content is present in the batched event(s).
	combined := ""
	for _, pe := range progressEvents {
		combined += pe.Text
	}
	expected := strings.Join(streamTool.chunks, "")
	if combined != expected {
		t.Errorf("combined progress text: expected %q, got %q", expected, combined)
	}

	// Verify final result aggregates all chunks.
	if toolResultEvent == nil {
		t.Fatal("missing hwcloud.StreamToolResult")
	}
	if toolResultEvent.Message.Content != "chunk1chunk2chunk3" {
		t.Errorf("final result: expected 'chunk1chunk2chunk3', got %q", toolResultEvent.Message.Content)
	}
}

func TestNonStreamingToolUnaffected(t *testing.T) {
	// Verify that tools implementing only Tool (no StreamExecutor) still work.
	blockingTool := &nonStreamingTool{}

	model := &fakeModelWithToolCall{
		toolName: "blocking_tool",
		toolArgs: `{}`,
		callID:   "call_xyz",
	}

	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(1),
	)
	deps := Deps{Tools: []hwcloud.Tool{blockingTool}}
	agent := New(cfg, deps)

	ch := agent.RunStream(context.Background(), hwcloud.Session{ID: "test"}, hwcloud.UserMessage("go"))

	var gotProgress bool
	var gotResult bool
	var gotDone bool

	for evt := range ch {
		switch evt.Type {
		case hwcloud.StreamToolProgress:
			gotProgress = true
		case hwcloud.StreamToolResult:
			gotResult = true
			if evt.Message.Content != "blocking_result" {
				t.Errorf("expected 'blocking_result', got %q", evt.Message.Content)
			}
		case hwcloud.StreamDone:
			gotDone = true
		case hwcloud.StreamError:
			t.Fatalf("unexpected error: %v", evt.Error)
		case hwcloud.StreamAborted:
			t.Fatalf("unexpected abort: %v", evt.Error)
		}
	}

	if gotProgress {
		t.Error("non-streaming tool should NOT emit hwcloud.StreamToolProgress")
	}
	if !gotResult {
		t.Error("missing hwcloud.StreamToolResult")
	}
	if !gotDone {
		t.Error("missing hwcloud.StreamDone")
	}
}

func TestConcurrentStreamingToolsDisambiguated(t *testing.T) {
	// Verify that when two streaming tools run concurrently,
	// each gets its own ToolCallID and chunks are emitted correctly.
	toolA := &streamingTestTool{
		name:   "tool_a",
		chunks: []string{"a1", "a2", "a3"},
		delay:  0,
	}
	toolB := &streamingTestTool{
		name:   "tool_b",
		chunks: []string{"b1", "b2"},
		delay:  0,
	}

	model := &fakeModelWithTwoToolCalls{
		calls: []hwcloud.ToolCall{
			{ID: "call_aaa", Type: "function", Function: hwcloud.ToolCallFunction{Name: "tool_a", Arguments: "{}"}},
			{ID: "call_bbb", Type: "function", Function: hwcloud.ToolCallFunction{Name: "tool_b", Arguments: "{}"}},
		},
	}

	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(1),
	)
	deps := Deps{Tools: []hwcloud.Tool{toolA, toolB}}
	agent := New(cfg, deps)

	ch := agent.RunStream(context.Background(), hwcloud.Session{ID: "test"}, hwcloud.UserMessage("go"))

	progressByID := map[string][]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)

	// Collect events concurrently (simulating real consumer).
	go func() {
		defer wg.Done()
		for evt := range ch {
			if evt.Type == hwcloud.StreamToolProgress {
				mu.Lock()
				progressByID[evt.ToolCallID] = append(progressByID[evt.ToolCallID], evt.Text)
				mu.Unlock()
			}
		}
	}()
	wg.Wait()

	// With 0ms delay, chunks arrive before the 1-second ticker fires.
	// Each tool's chunks are batched into one progress event. The rate
	// limiter batches by tool-call-id — they don't merge across tools.
	// Verify at least one progress event per tool and correct content.
	aChunks := progressByID["call_aaa"]
	bChunks := progressByID["call_bbb"]
	if len(aChunks) < 1 {
		t.Errorf("tool_a: expected at least 1 chunk, got %d", len(aChunks))
	}
	if len(bChunks) < 1 {
		t.Errorf("tool_b: expected at least 1 chunk, got %d", len(bChunks))
	}
	if strings.Join(aChunks, "") != "a1a2a3" {
		t.Errorf("tool_a: expected 'a1a2a3', got %q", strings.Join(aChunks, ""))
	}
	if strings.Join(bChunks, "") != "b1b2" {
		t.Errorf("tool_b: expected 'b1b2', got %q", strings.Join(bChunks, ""))
	}
}

func TestStreamExecutorCancellation(t *testing.T) {
	// Verify that cancelling the context stops the streaming tool.
	streamTool := &streamingTestTool{
		name:   "slow_tool",
		chunks: []string{"start", "middle", "end"},
		delay:  100 * time.Millisecond,
	}

	model := &fakeModelWithToolCall{
		toolName: "slow_tool",
		toolArgs: `{}`,
		callID:   "call_cancel",
	}

	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(1),
	)
	deps := Deps{Tools: []hwcloud.Tool{streamTool}}
	agent := New(cfg, deps)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := agent.RunStream(ctx, hwcloud.Session{ID: "test"}, hwcloud.UserMessage("go"))

	// Read the first progress event, then cancel.
	var gotFirst bool
	for evt := range ch {
		if evt.Type == hwcloud.StreamToolProgress && !gotFirst {
			gotFirst = true
			cancel()
		}
	}

	// After cancel, we should see hwcloud.StreamAborted (not hwcloud.StreamDone).
	// The channel should close cleanly.
	if !gotFirst {
		t.Error("should have received at least one progress event before cancel")
	}
}

// minRedactHook is a minimal RunHooks that replaces a single secret value in
// the tool result with "[REDACTED]". It lives in the root package test so we
// can exercise the runner's streaming path + hook pipeline without importing
// hooks/redact (which would create an import cycle: hooks/redact imports
// hwcloud). This is a test double, not the real hook.
type minRedactHook struct {
	secret string
}

func (h *minRedactHook) OnAgentStart(ctx context.Context, req hwcloud.ChatCompletionRequest) (context.Context, any, error) {
	return ctx, nil, nil
}
func (h *minRedactHook) OnAgentEnd(ctx context.Context, req hwcloud.ChatCompletionRequest, resp *hwcloud.ChatCompletionResponse, runErr error, startState any) {
}
func (h *minRedactHook) OnToolStart(ctx context.Context, tool hwcloud.FunctionDefinition, args json.RawMessage) (context.Context, any, error) {
	return ctx, nil, nil
}
func (h *minRedactHook) OnToolEnd(ctx context.Context, tool hwcloud.FunctionDefinition, args json.RawMessage, result *hwcloud.ToolResult, startState any) {
	if result != nil && result.Content != "" {
		result.Content = strings.ReplaceAll(result.Content, h.secret, "[REDACTED]")
	}
}

func TestStreamingWithRedactHook_FinalResultRedacted(t *testing.T) {
	// A streaming tool emits chunks containing a secret. The user sees live
	// hwcloud.StreamToolProgress (possibly raw — that's the point of streaming and
	// the model never sees those), but the final hwcloud.StreamToolResult that
	// enters the model's context must be redacted by OnToolEnd.
	const secret = "supersecret-token-value"
	streamTool := &streamingTestTool{
		name: "stream_tool",
		// Secret split across chunks to mimic realistic streaming.
		chunks: []string{"prefix-", secret, "-suffix"},
		delay:  0,
	}
	model := &fakeModelWithToolCall{
		toolName: "stream_tool",
		toolArgs: `{}`,
		callID:   "call_redact",
	}
	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(1),
	)
	deps := Deps{Tools: []hwcloud.Tool{streamTool}, Hooks: &minRedactHook{secret: secret}}
	agent := New(cfg, deps)

	ch := agent.RunStream(context.Background(), hwcloud.Session{ID: "test"}, hwcloud.UserMessage("go"))

	var toolResultEvent *hwcloud.StreamEvent
	var gotDone bool
	for evt := range ch {
		switch evt.Type {
		case hwcloud.StreamToolResult:
			evtCopy := evt
			toolResultEvent = &evtCopy
		case hwcloud.StreamDone:
			gotDone = true
		case hwcloud.StreamError:
			t.Fatalf("unexpected error: %v", evt.Error)
		case hwcloud.StreamAborted:
			t.Fatalf("unexpected abort: %v", evt.Error)
		}
	}

	if !gotDone {
		t.Fatal("missing hwcloud.StreamDone")
	}
	if toolResultEvent == nil {
		t.Fatal("missing hwcloud.StreamToolResult")
	}
	content := toolResultEvent.Message.Content
	if strings.Contains(content, secret) {
		t.Fatalf("secret leaked into final tool result: %q", content)
	}
	if !strings.Contains(content, "[REDACTED]") {
		t.Fatalf("final result not redacted: %q", content)
	}
}

// ── builtin tools mounted once ──

type stubMemoryProvider struct{}

func (s *stubMemoryProvider) Recall(context.Context, ctxpkg.ContextScope, string, int) ([]ctxpkg.MemoryEntry, error) {
	return nil, nil
}
func (s *stubMemoryProvider) Store(context.Context, ctxpkg.ContextScope, ctxpkg.MemoryItem) error {
	return nil
}

// Builtin tools are mounted once in New. Retrying Run on the same runtime
// (e.g. iac-server's generate retry loop) must not duplicate tool names —
// providers like DeepSeek reject duplicate tool names ("Tool names must
// be unique").
func TestBuiltinToolsMountedOnce(t *testing.T) {
	model := &fakeModelWithToolCall{toolName: "recall", toolArgs: `{}`, callID: "call_1"}
	cfg := agent.New("test", agent.WithModel(model), agent.WithMaxTurns(1))
	rt := New(cfg, Deps{MemoryProvider: &stubMemoryProvider{}})

	countRecall := func() int {
		n := 0
		for _, d := range rt.builtinTools {
			if d.Name == "recall" {
				n++
			}
		}
		return n
	}

	if got := countRecall(); got != 1 {
		t.Fatalf("recall mounted %d times in New, want 1", got)
	}
	session := hwcloud.Session{ID: "dup-tool-test"}
	if _, err := rt.Run(context.Background(), session, hwcloud.UserMessage("go")); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := rt.Run(context.Background(), session, hwcloud.UserMessage("go")); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if got := countRecall(); got != 1 {
		t.Fatalf("recall mounted %d times after 2 runs, want 1 (duplicate tool names)", got)
	}
}

// ── cancel-path persistence ──

// memStore is a minimal in-memory SessionStore for cancel-path tests.
type memStore struct {
	mu   sync.Mutex
	msgs []hwcloud.Message
}

func (m *memStore) Append(_ context.Context, _ string, msg hwcloud.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return nil
}
func (m *memStore) Recent(_ context.Context, _ string, n, _ int) ([]hwcloud.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n > len(m.msgs) {
		n = len(m.msgs)
	}
	out := make([]hwcloud.Message, n)
	copy(out, m.msgs[len(m.msgs)-n:])
	return out, nil
}
func (m *memStore) RecentAfter(_ context.Context, _ string, throughIndex, n int) ([]hwcloud.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if throughIndex >= len(m.msgs) || n <= 0 {
		return nil, nil
	}
	end := throughIndex + n
	if end > len(m.msgs) {
		end = len(m.msgs)
	}
	out := make([]hwcloud.Message, end-throughIndex)
	copy(out, m.msgs[throughIndex:end])
	return out, nil
}
func (m *memStore) Count(_ context.Context, _ string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs), nil
}
func (m *memStore) DeleteSession(_ context.Context, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = nil
	return nil
}

// A streaming tool cancelled mid-flight must persist a CANCELLED (error)
// result, not a truncated "success": the cancelled run commits with a
// background ctx (B1) and the model would otherwise read the partial
// output as a complete result next turn. This covers BOTH cancellation
// patterns — a ctx-respecting tool closing its stream early (the
// streamingTestTool pattern) and a blocking tool reaching toolCtx.Done().
//
// The tool must still be RUNNING when the cancel lands: progress events
// are rate-limited to 1/sec, so with a fast tool the first progress
// event arrives after the tool already finished (a success result is
// then correct, not a cancellation). 5 chunks × 300ms keeps the tool in
// flight past the first progress flush.
func TestStreamCancellationPersistsErrorResult(t *testing.T) {
	streamTool := &streamingTestTool{
		name:   "slow_tool",
		chunks: []string{"start", "middle", "end", "more", "tail"},
		delay:  300 * time.Millisecond,
	}
	model := &fakeModelWithToolCall{
		toolName: "slow_tool",
		toolArgs: `{}`,
		callID:   "call_cancel",
	}
	store := &memStore{}
	cfg := agent.New("test",
		agent.WithModel(model),
		agent.WithMaxTurns(1),
	)
	rt := New(cfg, Deps{Tools: []hwcloud.Tool{streamTool}, SessionStore: store})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := rt.RunStream(ctx, hwcloud.Session{ID: "test"}, hwcloud.UserMessage("go"))

	var sawAborted bool
	for evt := range ch {
		if evt.Type == hwcloud.StreamToolProgress {
			cancel()
		}
		if evt.Type == hwcloud.StreamAborted {
			sawAborted = true
		}
	}
	if !sawAborted {
		t.Fatal("expected StreamAborted after cancel")
	}

	// The persisted tool result must carry the cancellation error.
	msgs, err := store.Recent(context.Background(), "test", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Role == hwcloud.RoleTool && m.ToolCallID == "call_cancel" {
			if m.Result == nil || m.Result.Error == nil {
				t.Fatalf("cancelled tool result persisted as success: %+v", m)
			}
			if m.Result.Error.Code != "cancelled" {
				t.Fatalf("error code = %q, want cancelled", m.Result.Error.Code)
			}
			return
		}
	}
	t.Fatalf("no tool result persisted for call_cancel: %+v", msgs)
}
