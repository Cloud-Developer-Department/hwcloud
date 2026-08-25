package rest

import (
	"context"
	"encoding/json"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
)

// scriptedSubmit answers the approval request with the given action.
func scriptedSubmit(action string) func(hwcloud.ToolCall, chan approveResponse) {
	return func(_ hwcloud.ToolCall, resp chan approveResponse) {
		resp <- approveResponse{action: action, reason: "user"}
	}
}

func sess(id string) hwcloud.Session {
	return hwcloud.Session{ID: id}
}

// "allow" grants THIS call only — nothing is remembered, the same
// tool+args asks again (ACP allow_once semantics).
func TestRestApproverAllowIsOnce(t *testing.T) {
	mem := governance.NewSessionApprovalMemory()
	a := &restApprover{submit: scriptedSubmit("allow_once"), memory: mem}
	ctx := context.Background()

	d, err := a.Ask(ctx, hwcloud.ToolCall{ID: "c1", Function: hwcloud.ToolCallFunction{Name: "shell", Arguments: `{"command":"ls"}`}}, hwcloud.FunctionDefinition{}, sess("s1"))
	if err != nil || d.Action != governance.Allow {
		t.Fatalf("allow action = %v err = %v", d.Action, err)
	}
	// Nothing remembered: an identical call must ask again.
	key := governance.ApprovalKey("shell", json.RawMessage(`{"command":"ls"}`))
	if _, ok := mem.Recall(ctx, "s1", key); ok {
		t.Fatal("allow must not write session memory")
	}
}

// "always" remembers the exact tool+args for the session (ACP
// allow_always semantics); a CHANGED argument is a different operation.
// "deny" and "edit" never write memory.
func TestRestApproverAlwaysSessionScoped(t *testing.T) {
	mem := governance.NewSessionApprovalMemory()
	a := &restApprover{submit: scriptedSubmit("allow_always"), memory: mem}
	ctx := context.Background()

	if _, err := a.Ask(ctx, hwcloud.ToolCall{ID: "c1", Function: hwcloud.ToolCallFunction{Name: "shell", Arguments: `{"command":"ls -la"}`}}, hwcloud.FunctionDefinition{}, sess("s1")); err != nil {
		t.Fatal(err)
	}
	// The command atom is remembered (session-scoped) — the multi-key
	// decomposition of the shell call.
	key := governance.ShellCmdKey("ls -la")
	if _, ok := mem.Recall(ctx, "s1", key); !ok {
		t.Fatal("always must write session memory under the command-atom key")
	}
	// Another session is NOT covered (session-scoped).
	if _, ok := mem.Recall(ctx, "s2", key); ok {
		t.Fatal("session grant leaked to another session")
	}
	// A changed command is a different key.
	if _, ok := mem.Recall(ctx, "s1", governance.ShellCmdKey("rm -rf x")); ok {
		t.Fatal("changed command must not match")
	}
	// deny / edit never write memory.
	a2 := &restApprover{submit: scriptedSubmit("deny"), memory: mem}
	if _, err := a2.Ask(ctx, hwcloud.ToolCall{ID: "c2", Function: hwcloud.ToolCallFunction{Name: "shell", Arguments: `{"command":"rm"}`}}, hwcloud.FunctionDefinition{}, sess("s1")); err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.Recall(ctx, "s1", governance.ApprovalKey("shell", json.RawMessage(`{"command":"rm"}`))); ok {
		t.Fatal("deny must not write memory")
	}
	a3 := &restApprover{submit: scriptedSubmit("edit"), memory: mem}
	if _, err := a3.Ask(ctx, hwcloud.ToolCall{ID: "c3", Function: hwcloud.ToolCallFunction{Name: "shell", Arguments: `{"command":"mv"}`}}, hwcloud.FunctionDefinition{}, sess("s1")); err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.Recall(ctx, "s1", governance.ApprovalKey("shell", json.RawMessage(`{"command":"mv"}`))); ok {
		t.Fatal("edit must not write memory")
	}
}

// An unrecognized action fails CLOSED (deny) — an approval UI hiccup or
// a malformed request must never auto-execute the tool.
func TestRestApproverUnknownActionFailsClosed(t *testing.T) {
	a := &restApprover{submit: scriptedSubmit("maybe"), memory: governance.NewSessionApprovalMemory()}
	d, err := a.Ask(context.Background(), hwcloud.ToolCall{ID: "c1", Function: hwcloud.ToolCallFunction{Name: "shell", Arguments: `{}`}}, hwcloud.FunctionDefinition{}, sess("s1"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != governance.Deny {
		t.Fatalf("unknown action = %v, want Deny (fail closed)", d.Action)
	}
}

// countingHuman counts Ask invocations.
type countingHuman struct{ asked int }

func (h *countingHuman) Ask(context.Context, hwcloud.ToolCall, hwcloud.FunctionDefinition, hwcloud.Session) (governance.Decision, error) {
	h.asked++
	return governance.Decision{Action: governance.Allow, Reason: "human"}, nil
}

// The end-to-end equivalence: the key the approver writes on Ask is
// EXACTLY the key the policy engine looks up on the next Evaluate. Both
// sides derive it independently (approver and engine live in different
// packages) — this test pins the contract so a canonicalization change
// cannot silently break remembered grants.
func TestRestApproverAskToEvaluateRoundTrip(t *testing.T) {
	mem := governance.NewSessionApprovalMemory()
	human := &countingHuman{}
	engine := governance.NewEngine(nil, governance.NewToolClassifier(), mem, human)
	ctx := context.Background()
	s := sess("s1")

	call := hwcloud.ToolCall{
		ID:       "c1",
		Type:     "function",
		Function: hwcloud.ToolCallFunction{Name: "shell", Arguments: `{"command":"ls -la","timeout":10}`},
	}
	def := hwcloud.FunctionDefinition{Name: "shell"}

	// First call: engine defers to human, user picks "always", the bridge
	// remembers under its own ApprovalKey derivation.
	if _, err := engine.Evaluate(ctx, call, def, s); err != nil {
		t.Fatal(err)
	}
	if human.asked != 1 {
		t.Fatalf("first call human asks = %d, want 1", human.asked)
	}
	a := &restApprover{submit: scriptedSubmit("allow_always"), memory: mem}
	if _, err := a.Ask(ctx, call, def, s); err != nil {
		t.Fatal(err)
	}

	// Second call with the SAME call (same args byte-for-byte): the
	// engine's own key derivation must hit what the approver wrote.
	if _, err := engine.Evaluate(ctx, call, def, s); err != nil {
		t.Fatal(err)
	}
	if human.asked != 1 {
		t.Fatalf("second call human asks = %d, want 1 (memory must hit)", human.asked)
	}
}
