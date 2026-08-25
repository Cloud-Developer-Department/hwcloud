// Package wasm provides a runtime plugin system via WASM modules.
// Plugins are loaded from a directory and exposed as standard hwcloud
// interfaces: Tool plugins → []Tool, Stage plugins → RunObserver.
//
// The plugin system is itself pluggable: if the user never creates a Manager
// or specifies no plugin directory, the system is inert.
//
//	// Without plugins: nothing changes.
//	cfg := agent.New("bot", agent.WithModel(model))
//	rt := kernel.New(cfg, kernel.Deps{})
//
//	// With plugins:
//	mgr := wasm.NewManager("./plugins")
//	mgr.Discover(ctx)
//	cfg := agent.New("bot", agent.WithModel(model))
//	rt := kernel.New(cfg, kernel.Deps{
//	    Tools:    mgr.Tools(),
//	    Observer: mgr.Observer(),
//	})
package wasm

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/plugin/wasmhost"
)

// ── Plugin metadata (returned by guest's metadata() export) ──

// PluginMeta is the JSON metadata blob every .wasm module exports via metadata().
type PluginMeta struct {
	Type        string          `json:"type"`                 // "agent:tools", "agent:observers"
	Name        string          `json:"name"`                 // unique name
	Description string          `json:"description"`          // human-readable
	Parameters  json.RawMessage `json:"parameters,omitempty"` // tools: JSON Schema
	Stage       string          `json:"stage,omitempty"`      // observers: which stage
	Phase       string          `json:"phase,omitempty"`      // observers: "enter" | "leave" | "*"
	Schedules   []Schedule      `json:"schedules,omitempty"`  // cron jobs (registered at load)
}

// Schedule is one cron job declared by a plugin. The host registers it
// with the scheduler at load time and calls the plugin's run_scheduled
// export when it fires.
type Schedule struct {
	ID          string `json:"id"`
	Cron        string `json:"cron"`
	Description string `json:"description,omitempty"`
}

// ── Stage input/output ──

// StageInput is passed to observers plugins' run().
type StageInput struct {
	Name   string         `json:"name"`             // stage constant e.g. "model.call"
	Phase  string         `json:"phase"`            // "enter" or "leave"
	Detail map[string]any `json:"detail,omitempty"` // optional metadata
	Error  string         `json:"error,omitempty"`  // non-empty if stage failed
}

// StageOutput is returned from stage plugins' run().
type StageOutput struct {
	Action string `json:"action"` // "continue" or "abort"
	Reason string `json:"reason,omitempty"`
}

// ── Decision input/output ──
//
// Decision events are delivered to observer plugins that export
// "observe_decision" (probed at load — see module.hasDecision). Older
// binaries without the export never receive these; the host skips them
// silently. The shape mirrors hwcloud.DecisionEvent so a plugin sees the
// same fields a Go DecisionObserver would.

// DecisionInput is passed to observer plugins' observe_decision().
type DecisionInput struct {
	Layer   string         `json:"layer"`           // decision-layer constant
	Outcome string         `json:"outcome"`         // decision-value constant
	Subject string         `json:"subject"`         // what was decided on
	Detail  map[string]any `json:"detail,omitempty"`
	RunID   string         `json:"run_id,omitempty"`  // trajectory grouping key
	TurnID  int            `json:"turn_id,omitempty"` // turn index (-1 = pre-loop)
}

// DecisionOutput is returned from observe_decision(). Advisory only — a
// plugin may surface or record the event; no action alters the run (unlike
// StageOutput.ActionAbort). Kept struct-shaped for forward compatibility
// (a future plugin may return a derived insight).
type DecisionOutput struct {
	Action string `json:"action,omitempty"` // reserved (always empty today)
}

// ── Tool input/output ──

// ToolInput is passed to tool plugins' execute().
type ToolInput struct {
	Args json.RawMessage `json:"args"`
}

// ToolOutput is returned from tool plugins' execute().
type ToolOutput struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ── Plugin type constants ──

// PluginAgentPrefix is the type prefix for agent-level plugins ("agent:tools", "agent:observers").
// CLI plugins use "cli:" prefix. See [PluginTypeTools] and [PluginTypeObservers].
const PluginAgentPrefix = "agent:"

const (
	PluginTypeTools     = PluginAgentPrefix + "tools"
	PluginTypeObservers = PluginAgentPrefix + "observers"
)

// Stage names come from the root package (hwcloud.StageXxx) — the
// plugin's stage filter matches against those, no duplicate literals.
//
// ── Stage phase constants ──

const (
	PhaseEnter = "enter"
	PhaseLeave = "leave"
	PhaseAll   = "*"
)

// ── Stage action constants ──

const (
	ActionContinue = "continue"
	ActionAbort    = "abort"
)

// ── Agent runtime ──

// BuildAgentRuntime constructs a wasmhost.AgentRuntime backed by the given
// Agent config and Session. The Get/Set closures directly read/write
// Agent and Session fields. setModel is called by runtime_set_model_config
// to replace a model in the global registry; setEmbedding is called by
// runtime_set_embedding_config to refresh the embedder's credentials.
// Both may be nil.
func BuildAgentRuntime(rt *kernel.Runtime, session *hwcloud.Session, setModel func(provider, modelID, apiKey, baseURL string, maxInputTokens, maxOutputTokens int), setEmbedding func(baseURL, apiKey, model string)) *wasmhost.AgentRuntime {
	return &wasmhost.AgentRuntime{
		SetModel:     setModel,
		SetEmbedding: setEmbedding,
		Get: func(key string) (string, bool) {
			switch key {
			case wasmhost.RuntimeKeySessionID:
				return session.ID, true
			case wasmhost.RuntimeKeyUserID:
				return session.UserID, true
			case wasmhost.RuntimeKeyTurnCount:
				return fmt.Sprint(session.Turn), true
			case wasmhost.RuntimeKeyModelID:
				return session.ModelID, true
			case wasmhost.RuntimeKeyProvider:
				return session.Provider, true
			default:
				if strings.HasPrefix(key, wasmhost.RuntimeKeyMetadataPrefix) {
					k := strings.TrimPrefix(key, wasmhost.RuntimeKeyMetadataPrefix)
					v, ok := session.Metadata[k]
					if !ok {
						return "", false
					}
					s, _ := v.(string)
					return s, true
				}
				return "", false
			}
		},
		Set: func(key string, value string) error {
			switch key {
			case wasmhost.RuntimeKeyModelID:
				session.ModelID = value
			case "system_prompts":
				var prompts []string
				if err := json.Unmarshal([]byte(value), &prompts); err != nil {
					return err
				}
				rt.SetSystemPrompts(prompts)
				return nil
			case "max_turns":
				n, err := strconv.Atoi(value)
				if err != nil {
					return err
				}
				rt.SetMaxTurns(n)
				return nil
			default:
				if strings.HasPrefix(key, wasmhost.RuntimeKeyMetadataPrefix) {
					k := strings.TrimPrefix(key, wasmhost.RuntimeKeyMetadataPrefix)
					if session.Metadata == nil {
						session.Metadata = make(map[string]any)
					}
					session.Metadata[k] = value
					return nil
				}
				return fmt.Errorf("unknown runtime key: %s", key)
			}
			return nil
		},
	}
}
