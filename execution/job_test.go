package execution

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// retryTool fails N times with a Retryable error, then succeeds.
type retryTool struct {
	failures int32
}

func (t *retryTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{Name: "retry_tool", Parameters: hwcloud.SchemaOf[struct{}]()}
}

func (t *retryTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	if atomic.AddInt32(&t.failures, -1) >= 0 {
		return hwcloud.ErrorResult(&retryErr{}, true, "transient")
	}
	return &hwcloud.ToolResult{Content: "ok"}
}

type retryErr struct{}

func (retryErr) Error() string { return "transient failure" }

func newTestExec(tools ...hwcloud.Tool) *ExecutionRuntime {
	return New(Config{
		ToolSnapshot: func() []hwcloud.Tool { return tools },
	})
}

// TestJob_OrderedOutputs verifies concurrent jobs collect in call order.
func TestJob_OrderedOutputs(t *testing.T) {
	e := newTestExec(&slowTool{name: "a", delay: 50 * time.Millisecond, out: "A"},
		&slowTool{name: "b", delay: 5 * time.Millisecond, out: "B"})
	ctx := context.Background()
	session := hwcloud.Session{ID: "s"}

	h1 := e.Start(ctx, session, hwcloud.ToolCall{ID: "1", Function: hwcloud.ToolCallFunction{Name: "a", Arguments: "{}"}}, nil)
	h2 := e.Start(ctx, session, hwcloud.ToolCall{ID: "2", Function: hwcloud.ToolCallFunction{Name: "b", Arguments: "{}"}}, nil)

	// h2 finishes first, but Wait+Output in start order must yield a then b.
	if err := h1.Wait(ctx); err != nil {
		t.Fatalf("h1: %v", err)
	}
	if err := h2.Wait(ctx); err != nil {
		t.Fatalf("h2: %v", err)
	}
	if got := h1.Output().Content; got != "A" {
		t.Fatalf("h1 output = %q, want A", got)
	}
	if got := h2.Output().Content; got != "B" {
		t.Fatalf("h2 output = %q, want B", got)
	}
}

// TestJob_CancelMidExecution verifies Cancel aborts a running job.
func TestJob_CancelMidExecution(t *testing.T) {
	e := newTestExec(&blockingTool{})
	ctx := context.Background()
	session := hwcloud.Session{ID: "s"}

	h := e.Start(ctx, session, hwcloud.ToolCall{ID: "1", Function: hwcloud.ToolCallFunction{Name: "block", Arguments: "{}"}}, nil)
	h.Cancel()
	done := make(chan struct{})
	go func() {
		h.Wait(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not terminate after Cancel")
	}
}

// TestJob_RetryableToolRetries verifies Retryable errors trigger retries.
func TestJob_RetryableToolRetries(t *testing.T) {
	tool := &retryTool{failures: 2} // fails twice, then succeeds
	e := newTestExec(tool)
	ctx := context.Background()

	msg := e.execute(ctx, hwcloud.Session{ID: "s"},
		hwcloud.ToolCall{ID: "1", Function: hwcloud.ToolCallFunction{Name: "retry_tool", Arguments: "{}"}}, nil)
	if msg.Content != "ok" {
		t.Fatalf("after retries content = %q, want ok (failures left=%d)", msg.Content, atomic.LoadInt32(&tool.failures))
	}
}

// slowTool is a Tool that sleeps then returns its output.
type slowTool struct {
	name  string
	delay time.Duration
	out   string
}

func (t *slowTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{Name: t.name, Parameters: hwcloud.SchemaOf[struct{}]()}
}

func (t *slowTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	select {
	case <-time.After(t.delay):
	case <-ctx.Done():
		return &hwcloud.ToolResult{Error: &hwcloud.ToolError{Message: ctx.Err().Error()}}
	}
	return &hwcloud.ToolResult{Content: t.out}
}

// blockingTool blocks until its context is cancelled.
type blockingTool struct{}

func (t *blockingTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{Name: "block", Parameters: hwcloud.SchemaOf[struct{}]()}
}

func (t *blockingTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	<-ctx.Done()
	return &hwcloud.ToolResult{
		Error: &hwcloud.ToolError{
			Message: ctx.Err().Error(),
		},
	}
}
