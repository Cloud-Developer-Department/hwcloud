package agent

import (
	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
)

// Option is a functional option for configuring an Agent.
type Option func(*Agent)

// WithModel sets the LLM provider.
func WithModel(m hwcloud.Model) Option {
	return func(a *Agent) { a.Model = m }
}

// WithDescription sets a human-readable description of the agent.
func WithDescription(desc string) Option {
	return func(a *Agent) { a.Description = desc }
}

// WithSystemPrompts replaces all system prompts.
func WithSystemPrompts(prompts ...string) Option {
	return func(a *Agent) { a.SystemPrompts = prompts }
}

// WithPrompt replaces the default prompt builder.
func WithPrompt(pb hwcloud.PromptBuilder) Option {
	return func(a *Agent) { a.Prompt = pb }
}

// WithInputGuard sets the input guard.
func WithInputGuard(g governance.InputGuard) Option {
	return func(a *Agent) { a.InGuard = g }
}

// WithOutputGuard sets the output guard.
func WithOutputGuard(g governance.OutputGuard) Option {
	return func(a *Agent) { a.OutGuard = g }
}

// WithMaxTurns sets the maximum number of loop iterations per run.
func WithMaxTurns(n int) Option {
	return func(a *Agent) { a.MaxTurns = n }
}

// WithMaxWorkingTokens sets the max token budget for the working message set.
// When exceeded, the runtime triggers incremental compression.
// 0 (default) means auto: 70% of the model's context window, or 20000 as fallback.
func WithMaxWorkingTokens(n int) Option {
	return func(a *Agent) { a.MaxWorkingTokens = n }
}

// WithMaxCompressedTokens sets the maximum token budget for the compressed
// summary. The summarizer will merge and de-duplicate facts when the budget
// is exceeded. 0 means no explicit limit (default 8192).
func WithMaxCompressedTokens(n int) Option {
	return func(a *Agent) { a.MaxCompressedTokens = n }
}

// WithReasoningEffort sets the reasoning effort passed through to the model.
func WithReasoningEffort(effort string) Option {
	return func(a *Agent) { a.ReasoningEffort = effort }
}
