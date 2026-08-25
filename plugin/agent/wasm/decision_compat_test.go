package wasm

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/plugin/wasmhost"
)

// WASM decision-ABI compatibility tests.
//
// These verify the graceful-degradation contract for the DecisionObserver
// ABI: a binary compiled against the new PDK (export! macro emits
// observe_decision) is decision-aware; a binary compiled against an older
// PDK (no observe_decision export) is silently skipped for decision events
// while stage routing is unaffected.
//
// They load REAL compiled PDK binaries from examples/plugin/:
//   - observer (examples/plugin/observer, agent:observers, built against the
//     updated PDK) — exports observe_decision → hasDecision=true.
//   - tool (examples/plugin/tool, agent:tools, built before the
//     DecisionObserver ABI) — no observe_decision export → hasDecision=false.
//
// The binaries are not committed (no .wasm lives in git) and are not built
// by `go test`. If they are absent the tests skip — they are a local-dev
// verification gate over real PDK output, not a CI-runnable unit test.

// observerWasm/toolWasm are relative to this package's test working dir.
const (
	observerWasm = "../../../examples/plugin/observer/target/wasm32-unknown-unknown/release/example_agent_observer.wasm"
	toolWasm     = "../../../examples/plugin/tool/target/wasm32-unknown-unknown/release/example_agent_tool.wasm"
)

// newLoaderWithHost builds a wazero runtime with the host module registered,
// so observer.wasm (which imports host::log_info from observe_stage) can
// instantiate. The nil keyring is safe — registration only installs function
// stubs; a nil Keyring makes keyring_* return errors at call time, which the
// observer never invokes. Returns a cleanup that closes the runtime.
func newLoaderWithHost(t *testing.T, ctx context.Context) (loader, func()) {
	t.Helper()
	ldr, err := newLoader(ctx)
	if err != nil {
		t.Fatalf("newLoader: %v", err)
	}
	hostAPI := wasmhost.NewHostAPI(nil)
	if err := hostAPI.RegisterHostModule(ctx, ldr.runtime); err != nil {
		_ = ldr.Close(ctx)
		t.Fatalf("RegisterHostModule: %v", err)
	}
	return ldr, func() { _ = ldr.Close(context.Background()) }
}

// loadBinary reads a .wasm file and loads it via the real loadModule path
// (the production probe that sets hasDecision). Skips the test if the binary
// is not built.
func loadBinary(t *testing.T, ctx context.Context, ldr loader, path string) *module {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("PDK binary not built (run `make` in examples/plugin): %s: %v", path, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	mod, err := ldr.loadModule(ctx, filepath.Base(path), b)
	if err != nil {
		t.Fatalf("loadModule %s: %v", path, err)
	}
	return mod
}

// TestLoadModule_DecisionProbe: the load-time probe must set hasDecision
// from the observe_decision export's presence. New PDK binary → true; old
// binary (compiled before the ABI) → false. This is the branch point for
// every downstream skip.
func TestLoadModule_DecisionProbe(t *testing.T) {
	ctx := context.Background()
	ldr, cleanup := newLoaderWithHost(t, ctx)
	defer cleanup()

	newMod := loadBinary(t, ctx, ldr, observerWasm)
	if !newMod.hasDecision {
		t.Error("observer.wasm (new PDK): hasDecision=false, want true; observe_decision export not detected")
	}

	oldMod := loadBinary(t, ctx, ldr, toolWasm)
	if oldMod.hasDecision {
		t.Error("tool.wasm (old PDK): hasDecision=true, want false; observe_decision export should be absent")
	}
}

// TestInvokeDecision_NewBinaryCallable: on a hasDecision=true module,
// invokeDecision must reach the guest's observe_decision export and return
// the advisory output without error. LoggerPlugin does not override
// observe_decision, so the PDK default fires and returns an empty
// DecisionOutput — proving the export is wired end-to-end (host marshals
// DecisionInput → guest reads it → guest returns DecisionOutput → host
// unmarshals). This is the positive "invokeDecision 被调" assertion.
func TestInvokeDecision_NewBinaryCallable(t *testing.T) {
	ctx := context.Background()
	ldr, cleanup := newLoaderWithHost(t, ctx)
	defer cleanup()

	mod := loadBinary(t, ctx, ldr, observerWasm)
	obs := &wasmObserver{mod: mod, meta: PluginMeta{Type: PluginTypeObservers, Name: "observer_logger", Stage: PhaseAll, Phase: PhaseAll}, name: "observer_logger"}

	out, err := obs.invokeDecision(ctx, hwcloud.DecisionEvent{
		Layer:   hwcloud.DecisionPolicyHuman,
		Outcome: hwcloud.OutcomeAsk,
		Subject: "blocking_tool",
		Detail:  map[string]any{"reason": "test"},
		RunID:   "run-1",
		TurnID:  0,
	})
	if err != nil {
		t.Fatalf("invokeDecision on new binary failed: %v", err)
	}
	if out == nil {
		t.Fatal("invokeDecision returned nil output")
	}
	if out.Action != "" {
		t.Errorf("observe_decision default Action=%q, want empty (no-op default)", out.Action)
	}
}

// TestInvokeDecision_OldBinaryExportAbsent: on a hasDecision=false module,
// the observe_decision export is genuinely missing — a direct invokeDecision
// call errors with "observe_decision not found". This proves the skip guard
// in runDecisionObservers is load-bearing: without it, every decision event
// would error into the missing export on old binaries.
func TestInvokeDecision_OldBinaryExportAbsent(t *testing.T) {
	ctx := context.Background()
	ldr, cleanup := newLoaderWithHost(t, ctx)
	defer cleanup()

	mod := loadBinary(t, ctx, ldr, toolWasm)
	// Wrap the tool module as an observer to exercise invokeDecision — the
	// module itself is a real wazero instance; only the observe_decision
	// export is absent.
	obs := &wasmObserver{mod: mod, meta: PluginMeta{Type: PluginTypeObservers, Name: "old_tool", Stage: PhaseAll, Phase: PhaseAll}, name: "old_tool"}

	_, err := obs.invokeDecision(ctx, hwcloud.DecisionEvent{
		Layer:   hwcloud.DecisionPolicyRule,
		Outcome: hwcloud.OutcomeAllow,
		Subject: "echo",
	})
	if err == nil {
		t.Fatal("invokeDecision on old binary succeeded; want error (observe_decision export absent)")
	}
	if !strings.Contains(err.Error(), "observe_decision") {
		t.Errorf("invokeDecision error = %q, want it to mention observe_decision", err.Error())
	}
}

// TestRunDecisionObservers_SkipsOldBinary: end-to-end skip behavior. A
// Manager holding TWO observers — a new binary (hasDecision=true) and an old
// binary wrapped as an observer (hasDecision=false) — dispatches a decision
// event. runDecisionObservers must call invokeDecision on the new observer
// (succeeds, empty output → no log) and SKIP the old observer (no
// invokeDecision → no "export not found" error). Any "wasm decision observer
// error" in the log means either the skip guard regressed or the new
// observer's invokeDecision broke — either is a failure.
func TestRunDecisionObservers_SkipsOldBinary(t *testing.T) {
	ctx := context.Background()
	ldr, cleanup := newLoaderWithHost(t, ctx)
	defer cleanup()

	newMod := loadBinary(t, ctx, ldr, observerWasm)
	oldMod := loadBinary(t, ctx, ldr, toolWasm)

	mgr := &Manager{
		observers: []*wasmObserver{
			{mod: newMod, meta: PluginMeta{Type: PluginTypeObservers, Name: "observer_logger", Stage: PhaseAll, Phase: PhaseAll}, name: "observer_logger"},
			{mod: oldMod, meta: PluginMeta{Type: PluginTypeObservers, Name: "old_tool", Stage: PhaseAll, Phase: PhaseAll}, name: "old_tool"},
		},
	}

	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	mgr.runDecisionObservers(hwcloud.DecisionEvent{
		Layer:   hwcloud.DecisionPolicyHuman,
		Outcome: hwcloud.OutcomeAsk,
		Subject: "blocking_tool",
		RunID:   "run-1",
		TurnID:  0,
	})

	logged := buf.String()
	if strings.Contains(logged, "wasm decision observer error") {
		t.Errorf("runDecisionObservers logged a decision error — skip guard failed or new observer broke:\n%s", logged)
	}
}

// TestRunObservers_StageRoutingIndependentOfDecisionProbe: stage routing
// must not depend on hasDecision. The observer binary (hasDecision=true)
// routes a stage event through runObservers → guest run() → observe_stage →
// host::log_info, returning action=continue. The host logs "wasm observer"
// after a successful non-abort dispatch — that log line is the positive
// signal that the stage path still works alongside the new decision path.
//
// (The old binaries under examples/plugin/ are all agent:tools, not
// observers — they do not receive stage events at all, by type, not by
// hasDecision. hasDecision gates only decision events; stage routing is
// keyed on the "run" export, which observer binaries of any PDK vintage
// export.)
func TestRunObservers_StageRoutingIndependentOfDecisionProbe(t *testing.T) {
	ctx := context.Background()
	ldr, cleanup := newLoaderWithHost(t, ctx)
	defer cleanup()

	mod := loadBinary(t, ctx, ldr, observerWasm)
	mgr := &Manager{
		observers: []*wasmObserver{
			{mod: mod, meta: PluginMeta{Type: PluginTypeObservers, Name: "observer_logger", Stage: PhaseAll, Phase: PhaseAll}, name: "observer_logger"},
		},
	}

	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	mgr.runObservers(hwcloud.StageEvent{Name: hwcloud.StageModelCall, Phase: PhaseEnter})

	logged := buf.String()
	if !strings.Contains(logged, "wasm observer") || !strings.Contains(logged, "observer_logger") {
		t.Errorf("stage event did not route to observer (no host-side 'wasm observer' log); log:\n%s", logged)
	}
}
