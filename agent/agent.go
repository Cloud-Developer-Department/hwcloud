// Package agent defines the Agent configuration — a pure description of
// what an agent is (model, prompts, guards, skills, limits). It holds no
// runtime capabilities: no tools, no memory, no approver, no hooks.
//
// Execution is driven by kernel.Runtime, which the application assembles
// from an Agent config plus runtime dependencies (tools, stores, policy,
// hooks). This separation is the core of the Context Architecture: the
// Agent reasons and decides; infrastructure lives elsewhere.
package agent

import (
	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
)

// Agent is a configured agent ready to run — configuration only.
//
// Create via New + WithXxx options:
//
//	cfg := agent.New("billing",
//	    agent.WithModel(openaiModel),
//	    agent.WithSystemPrompts("You are a billing assistant."),
//	)
//	rt := kernel.New(cfg, kernel.Deps{Tools: ..., Policy: ...})
//	result, err := rt.Run(ctx, session, input)
//
// The Agent is immutable by convention after construction (Clone for
// per-run variants). Tools/Memory/Approver/Hooks/Observer are NOT agent
// concerns anymore — they are runtime dependencies (kernel.Deps).
type Agent struct {
	Name          string
	Description   string
	SystemPrompts []string

	Model    hwcloud.Model
	Prompt   hwcloud.PromptBuilder // nil = default build prompt
	InGuard  governance.InputGuard    // nil = no input guard
	OutGuard governance.OutputGuard   // nil = no output guard

	// Configuration
	MaxTurns            int // max loop iterations, 0 = default (20)
	MaxWorkingTokens    int // max tokens for working set before compaction; 0 = 70% of model context window
	MaxCompressedTokens int // max tokens for compressed summary, 0 = no limit (default 8192)

	// ReasoningEffort is passed through to the Model's ChatCompletionRequest
	// for providers that support it (OpenAI o-series, Anthropic extended thinking).
	// Empty string means use the model default.
	ReasoningEffort string

	// SubAgents are pre-configured delegation targets: each becomes a tool
	// the model can call with a task, running in an isolated context with
	// its own system prompt (v2.0 §22). The runtime registers them as
	// tools; the model decides when to delegate from the Description.
	SubAgents []SubAgent
}

// SubAgent is a pre-configured sub-agent. Unlike the dynamic spawn tool
// (removed), its identity is declared in config — name, system prompt, and
// tool scope — so delegation is governed and reviewable; the model only
// supplies the task. Sub-agents never spawn further sub-agents.
type SubAgent struct {
	// Name is the tool name the model calls (e.g. "researcher").
	Name string
	// Description is the tool contract the model sees: what the sub-agent
	// does AND when to delegate ("Use for focused, self-contained work...").
	Description string
	// SystemPrompt is the sub-agent's own system prompt — it does NOT
	// inherit the parent's.
	SystemPrompt string
	// Tools restricts the sub-agent to these tool names (nil = inherit all
	// tools except other sub-agents).
	Tools []string
	// Model overrides the parent model (nil = inherit).
	Model hwcloud.Model
	// MaxTurns caps the sub-agent loop; 0 = default (3).
	MaxTurns int
}

// New creates an Agent with the given name and options.
func New(name string, opts ...Option) *Agent {
	a := &Agent{
		Name:                name,
		MaxTurns:            20,
		MaxCompressedTokens: 8192,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	if a.Prompt == nil {
		a.Prompt = hwcloud.BuildPrompt
	}
	return a
}

// Clone returns a shallow copy of the Agent that is safe to mutate.
// Interface fields share the same underlying implementation — you don't
// want a new DB connection or LLM client per clone.
func (a *Agent) Clone() *Agent {
	clone := *a
	return &clone
}
