package summarizer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// scriptModel plays a scripted sequence of ChatCompletion outcomes. When
// the script is exhausted it repeats the last entry, so a single-entry
// script models "always returns X".
type scriptModel struct {
	mu    sync.Mutex
	calls int
	steps []scriptStep
}

type scriptStep struct {
	resp *hwcloud.ChatCompletionResponse
	err  error
}

func (m *scriptModel) ChatCompletion(_ context.Context, _ hwcloud.ChatCompletionRequest) (*hwcloud.ChatCompletionResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.calls
	if i >= len(m.steps) {
		i = len(m.steps) - 1
	}
	m.calls++
	return m.steps[i].resp, m.steps[i].err
}

func (m *scriptModel) ChatCompletionStream(_ context.Context, _ hwcloud.ChatCompletionRequest) (hwcloud.StreamReader, error) {
	return nil, nil
}

func (m *scriptModel) ContextWindow() int { return 128_000 }

func (m *scriptModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func okResponse(content string) *hwcloud.ChatCompletionResponse {
	return &hwcloud.ChatCompletionResponse{
		Choices: []hwcloud.Choice{{
			Message:      hwcloud.Message{Role: hwcloud.RoleAssistant, Content: content},
			FinishReason: "stop",
		}},
	}
}

// newTestCompressor builds a Compressor whose backoff is a no-op so retry
// paths run without sleeping.
func newTestCompressor(m hwcloud.Model) *Compressor {
	c := New(m)
	c.backoff = func(int, *hwcloud.RetryableError) time.Duration { return 0 }
	return c
}

func TestSummarize_RetriesOnRetryableError(t *testing.T) {
	m := &scriptModel{steps: []scriptStep{
		{err: &hwcloud.RetryableError{Err: errors.New("504 Gateway Timeout")}},
		{resp: okResponse("1. Primary Request: ...\n8. Next Step: ...")},
	}}
	c := newTestCompressor(m)

	cc, err := c.Summarize(context.Background(), []hwcloud.Message{
		{Role: hwcloud.RoleUser, Content: "hello"},
	}, nil)
	if err != nil {
		t.Fatalf("expected success after retry, got error: %v", err)
	}
	if cc == nil || !strings.Contains(cc.Summary, "Primary Request") {
		t.Fatalf("expected summary content, got %+v", cc)
	}
	if got := m.callCount(); got != 2 {
		t.Fatalf("expected 2 attempts (1 retry), got %d", got)
	}
}

func TestSummarize_RetriesExhausted(t *testing.T) {
	transient := &hwcloud.RetryableError{Err: errors.New("504 Gateway Timeout")}
	// maxRetries=2 → 3 attempts total. scriptModel repeats the last step
	// when exhausted, so 3 transient steps model "always fails".
	m := &scriptModel{steps: []scriptStep{
		{err: transient},
		{err: transient},
		{err: transient},
	}}
	c := newTestCompressor(m)

	cc, err := c.Summarize(context.Background(), []hwcloud.Message{
		{Role: hwcloud.RoleUser, Content: "hello"},
	}, nil)
	if err == nil {
		t.Fatal("expected error after retries exhausted, got nil")
	}
	if cc != nil {
		t.Fatalf("expected nil CompressedContext on failure, got %+v", cc)
	}
	if !strings.Contains(err.Error(), "summarizer: model call:") {
		t.Fatalf("expected error wrapped as 'summarizer: model call', got: %v", err)
	}
	if !errors.Is(err, transient) {
		t.Fatalf("expected underlying RetryableError preserved, got: %v", err)
	}
	if got := m.callCount(); got != 3 {
		t.Fatalf("expected exactly 3 attempts (2 retries), got %d", got)
	}
}

func TestSummarize_NilResponseNoPanic(t *testing.T) {
	m := &scriptModel{steps: []scriptStep{{resp: nil, err: nil}}}
	c := newTestCompressor(m)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Summarize panicked on nil response: %v", r)
		}
	}()
	cc, err := c.Summarize(context.Background(), []hwcloud.Message{
		{Role: hwcloud.RoleUser, Content: "hello"},
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil response, got nil")
	}
	if cc != nil {
		t.Fatalf("expected nil CompressedContext, got %+v", cc)
	}
	if !strings.Contains(err.Error(), "nil response") {
		t.Fatalf("expected 'nil response' error, got: %v", err)
	}
	if got := m.callCount(); got != 1 {
		t.Fatalf("nil response must not be retried, expected 1 call, got %d", got)
	}
}

func TestSummarize_NonRetryableNoRetry(t *testing.T) {
	perm := errors.New("400 Bad Request: invalid model")
	m := &scriptModel{steps: []scriptStep{{err: perm}}}
	c := newTestCompressor(m)

	cc, err := c.Summarize(context.Background(), []hwcloud.Message{
		{Role: hwcloud.RoleUser, Content: "hello"},
	}, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if cc != nil {
		t.Fatalf("expected nil CompressedContext, got %+v", cc)
	}
	if !errors.Is(err, perm) {
		t.Fatalf("expected underlying error preserved, got: %v", err)
	}
	if got := m.callCount(); got != 1 {
		t.Fatalf("non-retryable error must not retry, expected 1 call, got %d", got)
	}
}

func TestSummarize_EmptySummaryAfterRetry(t *testing.T) {
	m := &scriptModel{steps: []scriptStep{
		{err: &hwcloud.RetryableError{Err: errors.New("503")}},
		{resp: okResponse("   ")},
	}}
	c := newTestCompressor(m)

	cc, err := c.Summarize(context.Background(), []hwcloud.Message{
		{Role: hwcloud.RoleUser, Content: "hello"},
	}, nil)
	if err == nil {
		t.Fatal("expected error for empty summary, got nil")
	}
	if cc != nil {
		t.Fatalf("expected nil CompressedContext, got %+v", cc)
	}
	if !strings.Contains(err.Error(), "empty summary") {
		t.Fatalf("expected 'empty summary' error, got: %v", err)
	}
	if got := m.callCount(); got != 2 {
		t.Fatalf("expected 2 attempts (retry then empty), got %d", got)
	}
}

func TestSummarize_CancelledDuringBackoff(t *testing.T) {
	m := &scriptModel{steps: []scriptStep{
		{err: &hwcloud.RetryableError{Err: errors.New("504")}},
		{resp: okResponse("ok")},
	}}
	c := New(m)
	// Real backoff (2s for attempt 1) so the cancel below actually fires
	// during the wait rather than racing the backoff timer.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		cancel()
	}()
	cc, err := c.Summarize(ctx, []hwcloud.Message{
		{Role: hwcloud.RoleUser, Content: "hello"},
	}, nil)
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if cc != nil {
		t.Fatalf("expected nil CompressedContext, got %+v", cc)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled in chain, got: %v", err)
	}
}
