package governance

import (
	"context"
	"encoding/json"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// fakeHuman records Ask calls and returns a scripted decision.
type fakeHuman struct {
	asked    int
	decision Decision
}

func (f *fakeHuman) Ask(context.Context, hwcloud.ToolCall, hwcloud.FunctionDefinition, hwcloud.Session) (Decision, error) {
	f.asked++
	return f.decision, nil
}

func call(name string) hwcloud.ToolCall {
	return hwcloud.ToolCall{
		ID:   "c1",
		Type: "function",
		Function: hwcloud.ToolCallFunction{
			Name:      name,
			Arguments: "{}",
		},
	}
}

func def(name string) hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{Name: name}
}

func callWithArgs(name, args string) hwcloud.ToolCall {
	c := call(name)
	c.Function.Arguments = args
	return c
}

// TestEngine_RulesLayer: every glob form gates tool calls before the
// safety/human layers. The default engine's handoff rule
// ("transfer_to_*") must actually match — this was broken until the
// prefix-glob fix.
func TestEngine_RulesLayer(t *testing.T) {
	human := &fakeHuman{decision: Decision{Action: Deny, Reason: "user said no"}}
	e := NewEngine([]Rule{
		{ToolPattern: "transfer_to_*", Action: Allow, Reason: "handoff"},
		{ToolPattern: "*_agent", Action: Allow},
		{ToolPattern: "*grep*", Action: Allow},
		{ToolPattern: "shell", Action: Deny, Reason: "no shell"},
		{ToolPattern: "*", Action: Allow}, // catch-all last (first match wins)
	}, NewToolClassifier(), nil, human)

	cases := map[string]ApprovalAction{
		"transfer_to_designer": Allow, // prefix glob
		"build_agent":          Allow, // suffix glob
		"mygrepx":              Allow, // contains glob
		"anything":             Allow, // bare *
		"shell":                Deny,  // earlier exact rule beats the catch-all
	}
	for name, want := range cases {
		d, err := e.Evaluate(context.Background(), call(name), def(name), hwcloud.Session{})
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != want {
			t.Errorf("%s action = %v, want %v", name, d.Action, want)
		}
	}
	if human.asked != 0 {
		t.Fatalf("human consulted for rule-covered tools (%d asks)", human.asked)
	}
}

// TestEngine_ArgsPattern: a rule with ArgsPattern only matches calls whose
// args contain every key with the same value.
func TestEngine_ArgsPattern(t *testing.T) {
	human := &fakeHuman{decision: Decision{Action: Ask}}
	e := NewEngine([]Rule{{
		ToolPattern: "write",
		ArgsPattern: map[string]any{"path": "/etc/passwd"},
		Action:      Deny,
	}}, nil, nil, human)

	d, err := e.Evaluate(context.Background(), callWithArgs("write", `{"path":"/etc/passwd"}`), def("write"), hwcloud.Session{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != Deny {
		t.Fatalf("matching args action = %v, want Deny", d.Action)
	}
	if human.asked != 0 {
		t.Fatalf("human consulted for rule-matched call (%d asks)", human.asked)
	}

	d, err = e.Evaluate(context.Background(), callWithArgs("write", `{"path":"/tmp/ok"}`), def("write"), hwcloud.Session{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != Ask {
		t.Fatalf("non-matching args action = %v, want Ask (defer to human)", d.Action)
	}
}

// TestEngine_RememberedAskRoutesToHuman: a remembered Ask decision must
// still consult the human, never short-circuit into execution.
func TestEngine_RememberedAskRoutesToHuman(t *testing.T) {
	mem := NewSessionApprovalMemory()
	human := &fakeHuman{decision: Decision{Action: Allow}}
	e := NewEngine(nil, nil, mem, human)
	ctx := context.Background()
	sess := hwcloud.Session{ID: "s1"}

	if err := mem.Remember(ctx, sess.ID, ApprovalKey("shell", json.RawMessage("{}")), Decision{Action: Ask, Reason: "ask always"}); err != nil {
		t.Fatal(err)
	}
	d, err := e.Evaluate(ctx, call("shell"), def("shell"), sess)
	if err != nil {
		t.Fatal(err)
	}
	if human.asked != 1 {
		t.Fatalf("human asks = %d, want 1 (Ask must not short-circuit)", human.asked)
	}
	if d.Action != Allow {
		t.Fatalf("action = %v, want Allow (the human's decision)", d.Action)
	}
}

// TestEngine_ReadOnlyAutoAllowed: platform classification allows read-only
// tools without consulting the human layer (replaces the legacy
// CanSelfApprove self-declaration).
func TestEngine_ReadOnlyAutoAllowed(t *testing.T) {
	human := &fakeHuman{decision: Decision{Action: Deny}}
	e := NewEngine(nil, NewToolClassifier(), nil, human)

	d, err := e.Evaluate(context.Background(), call("read"), def("read"), hwcloud.Session{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != Allow {
		t.Fatalf("read-only tool action = %v, want Allow", d.Action)
	}
	if human.asked != 0 {
		t.Fatalf("human consulted for read-only tool (%d asks)", human.asked)
	}

	d, _ = e.Evaluate(context.Background(), call("ls"), def("ls"), hwcloud.Session{})
	if d.Action != Allow {
		t.Fatalf("ls action = %v, want Allow", d.Action)
	}
}

// TestEngine_DangerousConsultsHuman: non-readonly tools reach the human layer.
func TestEngine_DangerousConsultsHuman(t *testing.T) {
	human := &fakeHuman{decision: Decision{Action: Deny, Reason: "user said no"}}
	e := NewEngine(nil, NewToolClassifier(), nil, human)

	d, err := e.Evaluate(context.Background(), call("shell"), def("shell"), hwcloud.Session{})
	if err != nil {
		t.Fatal(err)
	}
	if human.asked != 1 {
		t.Fatalf("human asks = %d, want 1", human.asked)
	}
	if d.Action != Deny {
		t.Fatalf("action = %v, want Deny", d.Action)
	}
}

// TestEngine_AlwaysRememberedSkipsHuman: after an "always" decision is
// remembered for a tool, subsequent calls short-circuit without asking.
func TestEngine_AlwaysRememberedSkipsHuman(t *testing.T) {
	mem := NewSessionApprovalMemory()
	human := &fakeHuman{decision: Decision{Action: Allow, Reason: "always allow"}}
	e := NewEngine(nil, NewToolClassifier(), mem, human)
	ctx := context.Background()
	sess := hwcloud.Session{ID: "s1"}

	// First call: human decides; the bridge would Remember on "always" —
	// simulate that (as rest/acp bridges do).
	d1, _ := e.Evaluate(ctx, call("shell"), def("shell"), sess)
	if d1.Action != Allow {
		t.Fatalf("first action = %v", d1.Action)
	}
	// Memory is keyed by tool + canonical args (the bridge computes this).
	_ = mem.Remember(ctx, sess.ID, ApprovalKey("shell", json.RawMessage("{}")), d1)

	// Second call: memory layer short-circuits, human not asked again.
	d2, _ := e.Evaluate(ctx, call("shell"), def("shell"), sess)
	if d2.Action != Allow {
		t.Fatalf("remembered action = %v, want Allow", d2.Action)
	}
	if human.asked != 1 {
		t.Fatalf("human asks = %d, want 1 (memory should have skipped)", human.asked)
	}
}

// ── Multi-key (shell/write) memory semantics ──

// Shell allow-always needs EVERY command atom and file access remembered:
// a new command in a chain re-asks, a reused one doesn't.
func TestEngine_ShellAllKeysMustMatch(t *testing.T) {
	mem := NewSessionApprovalMemory()
	human := &fakeHuman{decision: Decision{Action: Allow, Reason: "always allow"}}
	e := NewEngine(nil, NewToolClassifier(), mem, human)
	ctx := context.Background()
	sess := hwcloud.Session{ID: "s1"}

	// Remember the atoms of `cat a | grep x` (as the bridge does on
	// allow_always) — but NOT the file access of a redirection variant.
	cmd := `{"command":"cat a | grep x"}`
	keys := MemoryKeys("shell", json.RawMessage(cmd))
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want [cat a, grep x]", keys)
	}
	for _, k := range keys {
		if err := mem.Remember(ctx, sess.ID, k, Decision{Action: Allow, Reason: "always allow"}); err != nil {
			t.Fatal(err)
		}
	}

	// Same chain: all atoms remembered → skips the human.
	d, _ := e.Evaluate(ctx, callWithArgs("shell", cmd), def("shell"), sess)
	if d.Action != Allow {
		t.Fatalf("same chain action = %v, want Allow", d.Action)
	}
	if human.asked != 0 {
		t.Fatalf("human asks = %d, want 0 (all keys remembered)", human.asked)
	}

	// Changed chain member: `cat b | grep x` — cat b not remembered → ask.
	if _, _ = e.Evaluate(ctx, callWithArgs("shell", `{"command":"cat b | grep x"}`), def("shell"), sess); human.asked != 1 {
		t.Fatalf("human asks = %d, want 1 (cat b is new)", human.asked)
	}

	// New file access: `cat a | grep x > /etc/shadow` — write not
	// remembered → ask (sensitive dir keeps single-file granularity).
	if _, _ = e.Evaluate(ctx, callWithArgs("shell", `{"command":"cat a | grep x > /etc/shadow"}`), def("shell"), sess); human.asked != 2 {
		t.Fatalf("human asks = %d, want 2 (write /etc/shadow is new)", human.asked)
	}
}

// write grants are directory-level: approving one file in a directory
// covers the directory's other files (dynamic filenames), and approving
// a read never covers a write.
func TestEngine_WriteDirectoryLevelGrant(t *testing.T) {
	mem := NewSessionApprovalMemory()
	human := &fakeHuman{decision: Decision{Action: Allow, Reason: "always allow"}}
	e := NewEngine(nil, NewToolClassifier(), mem, human)
	ctx := context.Background()
	sess := hwcloud.Session{ID: "s1"}

	// Approve writing out/result-1.txt — the grant unit is out/.
	k := MemoryKeys("write", json.RawMessage(`{"path":"out/result-1.txt","content":"x"}`))
	if len(k) != 1 {
		t.Fatalf("write keys = %v", k)
	}
	if err := mem.Remember(ctx, sess.ID, k[0], Decision{Action: Allow, Reason: "always allow"}); err != nil {
		t.Fatal(err)
	}

	// Another file in the same directory: covered.
	if d, _ := e.Evaluate(ctx, callWithArgs("write", `{"path":"out/result-2.txt","content":"y"}`), def("write"), sess); d.Action != Allow {
		t.Fatalf("same-dir write action = %v, want Allow", d.Action)
	}
	if human.asked != 0 {
		t.Fatalf("human asks = %d, want 0 (directory-level grant)", human.asked)
	}

	// A different directory: not covered.
	if _, _ = e.Evaluate(ctx, callWithArgs("write", `{"path":"src/result-1.txt","content":"z"}`), def("write"), sess); human.asked != 1 {
		t.Fatalf("human asks = %d, want 1 (src/ not granted)", human.asked)
	}

	// A sensitive path: /etc/passwd grant does NOT cover /etc/shadow.
	keys := MemoryKeys("write", json.RawMessage(`{"path":"/etc/passwd","content":"x"}`))
	if err := mem.Remember(ctx, sess.ID, keys[0], Decision{Action: Allow, Reason: "always allow"}); err != nil {
		t.Fatal(err)
	}
	if _, _ = e.Evaluate(ctx, callWithArgs("write", `{"path":"/etc/shadow","content":"y"}`), def("write"), sess); human.asked != 2 {
		t.Fatalf("human asks = %d, want 2 (sensitive dir keeps single-file)", human.asked)
	}
}

// ── DecisionObserver: per-layer emit ──

// captureDec records every DecisionEvent the Engine emits, in order.
type captureDec struct {
	events []hwcloud.DecisionEvent
}

func (c *captureDec) ObserveDecision(_ context.Context, e hwcloud.DecisionEvent) {
	c.events = append(c.events, e)
}

// findLayer returns the first event for the given layer, or nil.
func (c *captureDec) findLayer(layer string) *hwcloud.DecisionEvent {
	for i := range c.events {
		if c.events[i].Layer == layer {
			return &c.events[i]
		}
	}
	return nil
}

// runInfoCtx stamps a RunID/TurnID into ctx so emitted events carry them —
// verifies the Engine reads RunInfo from ctx (not from a stale field).
func runInfoCtx(runID string, turn int) context.Context {
	return hwcloud.WithRunInfo(context.Background(), hwcloud.RunInfo{RunID: runID, TurnID: turn})
}

// TestEngine_EmitsRuleLayer: a matched rule emits one DecisionPolicyRule
// event with the rule's outcome (Allow/Deny/Ask), and short-circuits so no
// lower layer fires.
func TestEngine_EmitsRuleLayer(t *testing.T) {
	c := &captureDec{}
	e := NewEngine([]Rule{
		{ToolPattern: "shell", Action: Deny, Reason: "no shell"},
	}, NewToolClassifier(), NewSessionApprovalMemory(), &fakeHuman{decision: Decision{Action: Allow}}).
		WithDecisionObserver(c)

	ctx := runInfoCtx("run-1", 2)
	d, err := e.Evaluate(ctx, call("shell"), def("shell"), hwcloud.Session{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != Deny {
		t.Fatalf("action = %v, want Deny", d.Action)
	}

	if got := len(c.events); got != 1 {
		t.Fatalf("emitted %d events, want 1 (rule short-circuit)", got)
	}
	ev := c.events[0]
	if ev.Layer != hwcloud.DecisionPolicyRule {
		t.Errorf("layer = %q, want %q", ev.Layer, hwcloud.DecisionPolicyRule)
	}
	if ev.Outcome != string(Deny) {
		t.Errorf("outcome = %q, want %q", ev.Outcome, Deny)
	}
	if ev.Subject != "shell" {
		t.Errorf("subject = %q, want shell", ev.Subject)
	}
	if ev.RunID != "run-1" || ev.TurnID != 2 {
		t.Errorf("RunID/TurnID = %q/%d, want run-1/2 (from ctx)", ev.RunID, ev.TurnID)
	}
	if ev.Detail["reason"] != "no shell" {
		t.Errorf("detail.reason = %v, want no shell", ev.Detail["reason"])
	}
}

// TestEngine_EmitsSafetyLayer: a read-only tool emits DecisionPolicySafety
// with OutcomeAllow and short-circuits before memory/human.
func TestEngine_EmitsSafetyLayer(t *testing.T) {
	c := &captureDec{}
	e := NewEngine(nil, NewToolClassifier(), nil, &fakeHuman{decision: Decision{Action: Deny}}).
		WithDecisionObserver(c)

	d, err := e.Evaluate(runInfoCtx("run-2", 0), call("read"), def("read"), hwcloud.Session{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != Allow {
		t.Fatalf("read-only action = %v, want Allow", d.Action)
	}

	if got := len(c.events); got != 1 {
		t.Fatalf("emitted %d events, want 1 (safety short-circuit)", got)
	}
	ev := c.events[0]
	if ev.Layer != hwcloud.DecisionPolicySafety {
		t.Errorf("layer = %q, want %q", ev.Layer, hwcloud.DecisionPolicySafety)
	}
	if ev.Outcome != hwcloud.OutcomeAllow {
		t.Errorf("outcome = %q, want %q", ev.Outcome, hwcloud.OutcomeAllow)
	}
	if ev.Detail["classifier"] != "readonly" {
		t.Errorf("detail.classifier = %v, want readonly", ev.Detail["classifier"])
	}
}

// TestEngine_EmitsMemoryLayerHit: a remembered Allow emits
// DecisionPolicyMemory with OutcomeHit (single-key mode) and short-circuits
// past the human layer.
func TestEngine_EmitsMemoryLayerHit(t *testing.T) {
	mem := NewSessionApprovalMemory()
	human := &fakeHuman{decision: Decision{Action: Deny}}
	e := NewEngine(nil, NewToolClassifier(), mem, human).WithDecisionObserver(&captureDec{})
	ctx := context.Background()
	sess := hwcloud.Session{ID: "s1"}

	// Seed memory with an Allow for "shell {}".
	key := ApprovalKey("shell", json.RawMessage("{}"))
	_ = mem.Remember(ctx, sess.ID, key, Decision{Action: Allow, Reason: "always allow"})

	c := &captureDec{}
	e.WithDecisionObserver(c)
	d, _ := e.Evaluate(runInfoCtx("run-3", 1), call("shell"), def("shell"), sess)
	if d.Action != Allow {
		t.Fatalf("remembered action = %v, want Allow", d.Action)
	}
	if human.asked != 0 {
		t.Fatalf("human asked %d times; memory should have short-circuited", human.asked)
	}

	if ev := c.findLayer(hwcloud.DecisionPolicyMemory); ev == nil {
		t.Fatalf("no DecisionPolicyMemory event; got %v", c.events)
	} else if ev.Outcome != hwcloud.OutcomeHit {
		t.Errorf("memory outcome = %q, want %q", ev.Outcome, hwcloud.OutcomeHit)
	} else if ev.Detail["mode"] != "single" {
		t.Errorf("memory mode = %v, want single", ev.Detail["mode"])
	}
	// Human layer must NOT have fired.
	if ev := c.findLayer(hwcloud.DecisionPolicyHuman); ev != nil {
		t.Errorf("human layer fired (%+v); memory should short-circuit", ev)
	}
}

// TestEngine_EmitsMemoryLayerMiss: when memory has no entry, the memory
// layer emits OutcomeMiss and defers to the human layer (which then emits
// OutcomeAsk). Two events total, in order.
func TestEngine_EmitsMemoryLayerMiss(t *testing.T) {
	mem := NewSessionApprovalMemory()
	human := &fakeHuman{decision: Decision{Action: Allow}}
	c := &captureDec{}
	e := NewEngine(nil, NewToolClassifier(), mem, human).WithDecisionObserver(c)

	_, _ = e.Evaluate(runInfoCtx("run-4", 0), call("shell"), def("shell"), hwcloud.Session{ID: "s1"})

	// memory miss → human ask. Two layers fired, in that order.
	if ev := c.findLayer(hwcloud.DecisionPolicyMemory); ev == nil {
		t.Fatalf("no memory event; got %v", c.events)
	} else if ev.Outcome != hwcloud.OutcomeMiss {
		t.Errorf("memory outcome = %q, want %q", ev.Outcome, hwcloud.OutcomeMiss)
	}
	if ev := c.findLayer(hwcloud.DecisionPolicyHuman); ev == nil {
		t.Fatalf("no human event; got %v", c.events)
	} else if ev.Outcome != hwcloud.OutcomeAsk {
		t.Errorf("human outcome = %q, want %q", ev.Outcome, hwcloud.OutcomeAsk)
	}
	// Order: memory before human.
	var memIdx, humanIdx int
	for i, ev := range c.events {
		if ev.Layer == hwcloud.DecisionPolicyMemory {
			memIdx = i
		}
		if ev.Layer == hwcloud.DecisionPolicyHuman {
			humanIdx = i
		}
	}
	if humanIdx <= memIdx {
		t.Errorf("human event (%d) not after memory event (%d)", humanIdx, memIdx)
	}
}

// TestEngine_EmitsHumanLayerFailClosed: a nil Human fails closed (Deny). The
// human layer emits OutcomeDeny — not OutcomeAsk — so a consumer can tell a
// fail-closed denial from a real human escalation.
func TestEngine_EmitsHumanLayerFailClosed(t *testing.T) {
	c := &captureDec{}
	e := NewEngine(nil, NewToolClassifier(), nil, nil).WithDecisionObserver(c)

	d, _ := e.Evaluate(runInfoCtx("run-5", 0), call("shell"), def("shell"), hwcloud.Session{})
	if d.Action != Deny {
		t.Fatalf("action = %v, want Deny (fail closed)", d.Action)
	}
	if ev := c.findLayer(hwcloud.DecisionPolicyHuman); ev == nil {
		t.Fatalf("no human event; got %v", c.events)
	} else if ev.Outcome != hwcloud.OutcomeDeny {
		t.Errorf("human outcome = %q, want %q (fail closed)", ev.Outcome, hwcloud.OutcomeDeny)
	}
}

// TestEngine_NoObserverSilent: with DecObserver == nil, Evaluate runs the
// full chain and produces the right Decision without emitting anything. This
// is the old-observer contract — a non-DecisionObserver RunObserver wired
// through the kernel never sets DecObserver, so governance stays silent.
func TestEngine_NoObserverSilent(t *testing.T) {
	// nil observer, nil human → fail-closed Deny at the human layer.
	e := NewEngine(nil, NewToolClassifier(), nil, nil)
	d, err := e.Evaluate(context.Background(), call("shell"), def("shell"), hwcloud.Session{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != Deny {
		t.Fatalf("action = %v, want Deny", d.Action)
	}
}

// ── #2/#4: four-tuple join-key stamping ──

// callWithID builds a ToolCall with a caller-chosen ID (the `call` helper
// hardcodes "c1"; these tests need distinct IDs to prove CallID is stamped
// per-call, not hardcoded).
func callWithID(id, name string) hwcloud.ToolCall {
	c := call(name)
	c.ID = id
	return c
}

// assertFourTuple verifies a DecisionEvent carries the full four-tuple join
// key (session_id, run_id, turn_id, call_id) the Engine stamps at every emit
// site. This is the #2/#4 regression — without it, two calls to the same tool
// in one turn produce indistinguishable events (same Subject, same RunID,
// same TurnID; only CallID disambiguates).
func assertFourTuple(t *testing.T, ev *hwcloud.DecisionEvent, wantRunID, wantSessionID, wantCallID string, wantTurnID int) {
	t.Helper()
	if ev.RunID != wantRunID {
		t.Errorf("%s RunID = %q, want %q", ev.Layer, ev.RunID, wantRunID)
	}
	if ev.TurnID != wantTurnID {
		t.Errorf("%s TurnID = %d, want %d", ev.Layer, ev.TurnID, wantTurnID)
	}
	if ev.SessionID != wantSessionID {
		t.Errorf("%s SessionID = %q, want %q (eventbus join key incomplete)", ev.Layer, ev.SessionID, wantSessionID)
	}
	if ev.CallID != wantCallID {
		t.Errorf("%s CallID = %q, want %q (same-tool-same-turn disambiguator lost)", ev.Layer, ev.CallID, wantCallID)
	}
}

// TestEngine_FourTupleOnEveryLayer: every layer that fires (rule, safety,
// memory, human) must stamp the full four-tuple join key from ctx RunInfo +
// the Evaluate-scoped call/session. This is the #2/#4 regression for the
// direct emit sites in policy.go (Evaluate's emit closure + askHuman).
func TestEngine_FourTupleOnEveryLayer(t *testing.T) {
	// Rule layer.
	{
		c := &captureDec{}
		e := NewEngine([]Rule{{ToolPattern: "shell", Action: Deny, Reason: "no"}},
			nil, nil, nil).WithDecisionObserver(c)
		ctx := runInfoCtx("run-4t", 5)
		sess := hwcloud.Session{ID: "sess-4t"}
		_, _ = e.Evaluate(ctx, callWithID("call-rule", "shell"), def("shell"), sess)
		if ev := c.findLayer(hwcloud.DecisionPolicyRule); ev == nil {
			t.Fatalf("no rule event; got %v", c.events)
		} else {
			assertFourTuple(t, ev, "run-4t", "sess-4t", "call-rule", 5)
		}
	}
	// Safety layer (read-only short-circuit).
	{
		c := &captureDec{}
		e := NewEngine(nil, NewToolClassifier(), nil, nil).WithDecisionObserver(c)
		ctx := runInfoCtx("run-4t", 5)
		sess := hwcloud.Session{ID: "sess-4t"}
		_, _ = e.Evaluate(ctx, callWithID("call-safe", "read"), def("read"), sess)
		if ev := c.findLayer(hwcloud.DecisionPolicySafety); ev == nil {
			t.Fatalf("no safety event; got %v", c.events)
		} else {
			assertFourTuple(t, ev, "run-4t", "sess-4t", "call-safe", 5)
		}
	}
	// Human layer (fail-closed Deny, nil human).
	{
		c := &captureDec{}
		e := NewEngine(nil, NewToolClassifier(), nil, nil).WithDecisionObserver(c)
		ctx := runInfoCtx("run-4t", 5)
		sess := hwcloud.Session{ID: "sess-4t"}
		_, _ = e.Evaluate(ctx, callWithID("call-human", "shell"), def("shell"), sess)
		if ev := c.findLayer(hwcloud.DecisionPolicyHuman); ev == nil {
			t.Fatalf("no human event; got %v", c.events)
		} else {
			assertFourTuple(t, ev, "run-4t", "sess-4t", "call-human", 5)
		}
	}
}

// TestEngine_CallIDDisambiguatesSameToolSameTurn: #4 core — when the same
// tool is called twice in one turn, the two DecisionEvents share Subject,
// RunID, TurnID, and SessionID, but MUST differ on CallID. Without CallID a
// consumer cannot tell which decision governed which call.
func TestEngine_CallIDDisambiguatesSameToolSameTurn(t *testing.T) {
	c := &captureDec{}
	e := NewEngine(nil, NewToolClassifier(), nil, &fakeHuman{decision: Decision{Action: Allow}}).
		WithDecisionObserver(c)
	ctx := runInfoCtx("run-dual", 3)
	sess := hwcloud.Session{ID: "sess-dual"}

	// Two calls to "shell" in the same turn, distinct call IDs.
	_, _ = e.Evaluate(ctx, callWithID("call-A", "shell"), def("shell"), sess)
	_, _ = e.Evaluate(ctx, callWithID("call-B", "shell"), def("shell"), sess)

	humanEvents := []*hwcloud.DecisionEvent{}
	for i := range c.events {
		if c.events[i].Layer == hwcloud.DecisionPolicyHuman {
			humanEvents = append(humanEvents, &c.events[i])
		}
	}
	if len(humanEvents) != 2 {
		t.Fatalf("human events = %d, want 2", len(humanEvents))
	}
	// Shared join keys (subject/run/turn/session).
	for _, ev := range humanEvents {
		if ev.Subject != "shell" || ev.RunID != "run-dual" || ev.TurnID != 3 || ev.SessionID != "sess-dual" {
			t.Errorf("human event join keys = %+v, want shell/run-dual/3/sess-dual", ev)
		}
	}
	// Distinct CallIDs.
	if humanEvents[0].CallID == humanEvents[1].CallID {
		t.Errorf("two calls share CallID %q; same-tool-same-turn events are indistinguishable", humanEvents[0].CallID)
	}
	// Both call IDs present (order-agnostic).
	got := map[string]bool{humanEvents[0].CallID: true, humanEvents[1].CallID: true}
	if !got["call-A"] || !got["call-B"] {
		t.Errorf("CallIDs = %v, want {call-A, call-B}", got)
	}
}
