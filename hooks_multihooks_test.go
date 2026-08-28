package hwcloud

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// hrecord is a minimal RunHooks that records calls and carries a per-start
// state through to its End so we can assert the MultiHooks state pairing.
type hrecord struct {
	name      string
	ends      []string // states seen by OnToolEnd, "state-i"
	discarded bool
}

func (h *hrecord) OnAgentStart(ctx context.Context, _ ChatCompletionRequest) (context.Context, any, error) {
	return ctx, h.name + ":agent-start", nil
}
func (h *hrecord) OnAgentEnd(context.Context, ChatCompletionRequest, *ChatCompletionResponse, error, any) {
}
func (h *hrecord) OnToolStart(ctx context.Context, _ FunctionDefinition, _ json.RawMessage) (context.Context, any, error) {
	return ctx, h.name + ":tool-start", nil
}
func (h *hrecord) OnToolEnd(_ context.Context, _ FunctionDefinition, _ json.RawMessage, result *ToolResult, startState any) {
	if startState == nil {
		h.discarded = true
		return
	}
	s, _ := startState.(string)
	h.ends = append(h.ends, s)
	if result != nil {
		result.Content = result.Content + "<" + s + ">"
	}
}

// TestMultiHooks_StatePairing: each child hook must receive the exact state
// its OnToolStart returned — not a sibling's, not nil (the prior bug silently
// zero-filled on a shape mismatch, dropping every hook's state).
func TestMultiHooks_StatePairing(t *testing.T) {
	a := &hrecord{name: "a"}
	b := &hrecord{name: "b"}
	mh := MultiHooks(a, b).(*multiHooks)

	// Simulate the runner's start→end sequence.
	ss := &FunctionDefinition{Name: "t"}
	_, startState, err := mh.OnToolStart(context.Background(), *ss, nil)
	if err != nil {
		t.Fatalf("OnToolStart: %v", err)
	}
	states, ok := startState.([]any)
	if !ok || len(states) != 2 {
		t.Fatalf("startState = %T %v, want []any len=2", startState, startState)
	}

	result := &ToolResult{Content: ""}
	mh.OnToolEnd(context.Background(), *ss, nil, result, startState)

	if a.discarded || b.discarded {
		t.Fatal("a hook state was discarded (nil) — MultiHooks dropped state")
	}
	if len(a.ends) != 1 || a.ends[0] != "a:tool-start" {
		t.Fatalf("hook a ends = %v, want [a:tool-start]", a.ends)
	}
	if len(b.ends) != 1 || b.ends[0] != "b:tool-start" {
		t.Fatalf("hook b ends = %v, want [b:tool-start]", b.ends)
	}
	// Each hook saw ITS OWN state, not the sibling's, and in list order.
	if !strings.Contains(result.Content, "<a:tool-start>") || !strings.Contains(result.Content, "<b:tool-start>") {
		t.Fatalf("result = %q, expected both a and b states applied", result.Content)
	}
}

// TestMultiHooks_NoSingleHookWrapping: WithRunHooks(a) should NOT wrap a
// single hook in multiHooks (MultiHooks returns the hook directly when
// len==1), so no nested-state shape mismatch path is taken.
func TestMultiHooks_SingleReturnsDirect(t *testing.T) {
	a := &hrecord{name: "a"}
	mh := MultiHooks(a)
	if _, ok := mh.(*multiHooks); ok {
		t.Fatal("MultiHooks(single) wrapped a single hook — should return it directly")
	}
	if mh != a {
		t.Fatal("MultiHooks(single) did not return the exact hook")
	}
}
