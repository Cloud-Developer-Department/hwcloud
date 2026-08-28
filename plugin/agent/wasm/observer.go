package wasm

import (
	"context"
	"encoding/json"
	"fmt"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// wasmObserver wraps a WASM observer plugin. It stores the filter (stage name + phase)
// and calls the guest's run() when events match.
type wasmObserver struct {
	mod  *module
	meta PluginMeta
	name string // human label for logging
}

// matches returns true if this plugin should fire for the given event.
func (s *wasmObserver) matches(event hwcloud.StageEvent) bool {
	if s.meta.Stage != "" && s.meta.Stage != PhaseAll && s.meta.Stage != event.Name {
		return false
	}
	if s.meta.Phase == "" || s.meta.Phase == PhaseAll {
		return true
	}
	return s.meta.Phase == event.Phase
}

// invoke calls the guest's run() and returns the parsed stage output.
func (s *wasmObserver) invoke(ctx context.Context, event hwcloud.StageEvent) (*StageOutput, error) {
	errStr := ""
	if event.Err != nil {
		errStr = event.Err.Error()
	}

	input := StageInput{
		Name:   event.Name,
		Phase:  event.Phase,
		Detail: event.Detail,
		Error:  errStr,
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("wasm stage %q: marshal: %w", s.meta.Name, err)
	}

	outputJSON, err := s.mod.invoke(ctx, "run", inputJSON)
	if err != nil {
		return nil, fmt.Errorf("wasm stage %q: %w", s.meta.Name, err)
	}

	var out StageOutput
	if err := json.Unmarshal(outputJSON, &out); err != nil {
		return nil, fmt.Errorf("wasm stage %q: parse output: %w", s.meta.Name, err)
	}

	// ActionAbort is an output (stop the run), not an error — the caller
	// decides from Action, never from err.
	return &out, nil
}

// invokeDecision calls the guest's observe_decision() and returns the
// parsed output. Only called for modules that probed hasDecision at load —
// the export is guaranteed present. Mirrors invoke() but targets the
// decision ABI; there is no abort path (decisions are observations).
func (s *wasmObserver) invokeDecision(ctx context.Context, event hwcloud.DecisionEvent) (*DecisionOutput, error) {
	input := DecisionInput{
		Layer:   event.Layer,
		Outcome: event.Outcome,
		Subject: event.Subject,
		Detail:  event.Detail,
		RunID:   event.RunID,
		TurnID:  event.TurnID,
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("wasm decision %q: marshal: %w", s.meta.Name, err)
	}
	outputJSON, err := s.mod.invoke(ctx, "observe_decision", inputJSON)
	if err != nil {
		return nil, fmt.Errorf("wasm decision %q: %w", s.meta.Name, err)
	}
	var out DecisionOutput
	if err := json.Unmarshal(outputJSON, &out); err != nil {
		return nil, fmt.Errorf("wasm decision %q: parse output: %w", s.meta.Name, err)
	}
	return &out, nil
}
