package kernel

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
	"github.com/Cloud-Developer-Department/hwcloud/provider/resource"
	"github.com/Cloud-Developer-Department/hwcloud/tokenizer"
)

// buildPrompt assembles the full prompt: static context (system prompts,
// project context) + dynamic context (skills, semantic memory, summary)
// via the agent's PromptBuilder. The builder's error is surfaced (not
// silently dropped) — an empty prompt must not silently reach the model.
func (rt *Runtime) buildPrompt(ctx context.Context, session hwcloud.Session, ac *ctxpkg.AgentContext) ([]hwcloud.Message, error) {
	// ── Static context (assembled once per run, never changes) ──
	// Snapshot under the lock: SetSystemPrompts (wasm runtime_set export)
	// can run concurrently from a tool callback.
	rt.mu.RLock()
	static := strings.Join(rt.cfg.SystemPrompts, "\n\n")
	rt.mu.RUnlock()
	if session.ProjectContext != "" {
		static += "\n\n## Project Context\n\n" + session.ProjectContext
	}

	// ── Dynamic context (re-assembled every turn) ──
	var dynamicParts []string
	dynamicParts = append(dynamicParts, fmt.Sprintf(`
IMPORTANT: The context below is generated fresh for this turn. If it conflicts with static instructions or earlier conversation, the latest context here is authoritative. Earlier summaries, skill lists, semantic memory, or plan state may be outdated.

OS: %s
Arch: %s
Date: %s
`, runtime.GOOS, runtime.GOARCH, time.Now().Format("2006-01-02")))

	// Skills catalog (full, from the AgentContext — the context layer owns
	// skill selection). Skill bodies are NOT injected here: the model loads
	// them via load_skill and the body enters the conversation as a tool
	// result (industry pattern).
	if len(ac.Skills) > 0 {
		dynamicParts = append(dynamicParts, buildSkillsSection(ac.Skills))
	} else {
		dynamicParts = append(dynamicParts, "\nIMPORTANT: No available skills.")
	}

	// ACP / plan-mode context (injected by Session.DynamicContext).
	if session.DynamicContext != "" {
		dynamicParts = append(dynamicParts, session.DynamicContext)
	}

	// Recalled durable knowledge (self-evolution layer).
	if len(ac.Memories) > 0 {
		dynamicParts = append(dynamicParts, buildKnowledgeSection(ac.Memories))
	}

	// External reference resources (docs/templates/API specs).
	if len(ac.Resources) > 0 {
		dynamicParts = append(dynamicParts, buildResourcesSection(ac.Resources))
	}

	// Compressed conversation summary — Layer 2 of the memory model.
	// MaxCompressedTokens is enforced HERE (the only place the summary
	// enters the prompt); the summarizer backend uses it as a target, but
	// the prompt must never carry an oversized summary.
	if rt.compressed != nil && rt.compressed.Summary != "" {
		section := buildCompressedSection(rt.compressed)
		// MaxCompressedTokens is immutable after New, but the summary
		// section uses rt.runModel which the run snapshots under the lock.
		if rt.cfg.MaxCompressedTokens > 0 {
			if n := tokenizer.Count(hwcloud.TokenizerModelID(rt.Model()), section); n > rt.cfg.MaxCompressedTokens {
				section = truncateTokens(section, rt.cfg.MaxCompressedTokens) + "\n\n[summary truncated: exceeds MaxCompressedTokens]"
			}
		}
		dynamicParts = append(dynamicParts, section)
	} else {
		dynamicParts = append(dynamicParts, "## Conversation Summary\n\n(no prior conversation history)")
	}

	input := hwcloud.PromptInput{
		StaticContext:   static,
		DynamicContext:  strings.Join(dynamicParts, "\n\n"),
		WorkingMessages: ac.Messages,
	}

	if rt.cfg.Prompt == nil {
		return hwcloud.BuildPrompt(ctx, input)
	}
	msgs, err := rt.cfg.Prompt(ctx, input)
	if err != nil {
		// Surface the builder error instead of silently sending an empty
		// prompt to the model (fixes the legacy silent-drop).
		return nil, fmt.Errorf("prompt build: %w", err)
	}
	return msgs, nil
}

// buildModelRequest assembles the model request: registered tools +
// built-in tools, session model/temperature/max-tokens, reasoning effort.
func (rt *Runtime) buildModelRequest(session hwcloud.Session, messages []hwcloud.Message) hwcloud.ChatCompletionRequest {
	tools := toolDefinitions(rt.SnapshotTools())
	if len(rt.builtinTools) > 0 {
		tools = append(tools, rt.builtinTools...)
	}
	rt.mu.RLock()
	reasoningEffort := rt.cfg.ReasoningEffort
	rt.mu.RUnlock()
	return hwcloud.ChatCompletionRequest{
		Model:           session.ModelID,
		Messages:        messages,
		Tools:           tools,
		Temperature:     session.Temperature,
		MaxTokens:       session.MaxTokens,
		ReasoningEffort: reasoningEffort,
	}
}

// ── Section builders ──

// buildResourcesSection lists reference resources relevant to the goal.
func buildResourcesSection(resources []resource.Resource) string {
	var b strings.Builder
	b.WriteString("## Reference Resources\n")
	b.WriteString("External reference material relevant to this task. Load content with the file tools when needed.\n")
	for _, r := range resources {
		b.WriteString("\n- " + r.URI)
		if r.MIMEType != "" {
			b.WriteString(" (" + r.MIMEType + ")")
		}
	}
	return b.String()
}

// buildKnowledgeSection renders recalled durable knowledge into a prompt
// section with provenance (kind per item).
func buildKnowledgeSection(memories []ctxpkg.MemoryEntry) string {
	var b strings.Builder
	b.WriteString("## Recalled Knowledge\n")
	b.WriteString("Durable knowledge about you and this project, recalled from prior sessions.\n")
	for _, m := range memories {
		b.WriteString("\n- [" + string(m.Kind) + "] " + m.Content)
	}
	return b.String()
}

// truncateTokens keeps the first n tokens of s (approximate: truncates at
// the first rune boundary past the token budget).
func truncateTokens(s string, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	runes := []rune(s)
	for _, r := range runes {
		b.WriteRune(r)
		if tokenizer.Count("gpt-4", b.String()) >= n {
			break
		}
	}
	return b.String()
}

func buildSkillsSection(skills []hwcloud.SkillInfo) string {
	var b strings.Builder
	b.WriteString("## Available Skills\n")
	b.WriteString("To load a skill, call the `load_skill` tool with the skill name.\n")
	for _, s := range skills {
		b.WriteString("\n### " + s.Name + "\n")
		for k, v := range s.Frontmatter {
			fmt.Fprintf(&b, "%s: %v\n", k, v)
		}
	}
	return b.String()
}

func buildCompressedSection(cc *hwcloud.CompressedContext) string {
	var b strings.Builder
	b.WriteString("## Conversation Summary\n")
	b.WriteString(cc.Summary)
	return b.String()
}

// estimatePromptOverhead returns the estimated token count of everything
// the prompt adds BEFORE the working messages. Subtracted from the working
// token budget so the total prompt fits within the model's context window.
func (rt *Runtime) estimatePromptOverhead(ctx context.Context, session hwcloud.Session, modelID string) int {
	var n int

	// SystemPrompts is mutable (wasm runtime_set exports) — snapshot under
	// the same lock buildPrompt uses.
	rt.mu.RLock()
	static := strings.Join(rt.cfg.SystemPrompts, "\n\n")
	rt.mu.RUnlock()
	if session.ProjectContext != "" {
		static += "\n\n## Project Context\n\n" + session.ProjectContext
	}
	if static != "" {
		n += tokenizer.Count(modelID, static) + 4
	}

	// Skills catalog is excluded: the budget runs at setup before the
	// catalog is discovered, and the prompt-time context-window hard check
	// covers its small addition (name+description per skill).
	if session.DynamicContext != "" {
		n += tokenizer.Count(modelID, session.DynamicContext) + 4
	}

	if rt.compressed != nil && rt.compressed.Summary != "" {
		n += tokenizer.Count(modelID, buildCompressedSection(rt.compressed)) + 4
	}

	return n
}
