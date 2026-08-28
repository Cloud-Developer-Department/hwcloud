package orchestrate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
)

// Planner generates a PlanDef from a goal and available agents.
type Planner interface {
	// Plan analyses the goal and returns a DAG of steps.
	// agents lists the available agents with their descriptions.
	// history is optional conversation context (may be nil/empty).
	Plan(ctx context.Context, goal string, agents []agent.AgentInfo, history []hwcloud.Message) (*PlanDef, error)

	// Replan generates replacement steps for a failed plan based on the
	// context in [ReplanInput]. The returned steps replace the affected
	// subtree; the caller merges them with the surviving steps.
	// onChunk is an optional streaming callback (nil = synchronous).
	Replan(ctx context.Context, input ReplanInput, onChunk func(string)) ([]StepDef, error)
}

// ── LLMPlanner ──

// LLMPlanner uses an LLM to decompose a goal into a DAG of steps.
// Generated plans are validated; if validation fails, the LLM is asked
// to correct the plan (up to maxRetries times).
type LLMPlanner struct {
	model      hwcloud.Model
	maxRetries int
}

// NewLLMPlanner creates a Planner backed by a Model.
func NewLLMPlanner(model hwcloud.Model) *LLMPlanner {
	return &LLMPlanner{model: model, maxRetries: 3}
}

// WithMaxRetries sets the maximum number of correction rounds on validation failure.
func (p *LLMPlanner) WithMaxRetries(n int) *LLMPlanner {
	p.maxRetries = n
	return p
}

// Plan implements [Planner].
func (p *LLMPlanner) Plan(ctx context.Context, goal string, agents []agent.AgentInfo, history []hwcloud.Message) (*PlanDef, error) {
	return p.planWithModel(ctx, goal, agents, history, false, nil)
}

// PlanStream generates a PlanDef with streaming text chunks emitted via onChunk.
// Each token/reasoning delta from the LLM is passed to onChunk as it arrives.
// The final PlanDef is returned after the full response is received and validated.
func (p *LLMPlanner) PlanStream(ctx context.Context, goal string, agents []agent.AgentInfo, history []hwcloud.Message, onChunk func(string)) (*PlanDef, error) {
	return p.planWithModel(ctx, goal, agents, history, true, onChunk)
}

// Replan implements [Planner]. It builds a replan-specific prompt from
// [ReplanInput] and returns a replacement []StepDef for the affected subtree.
func (p *LLMPlanner) Replan(ctx context.Context, input ReplanInput, onChunk func(string)) ([]StepDef, error) {
	messages := []hwcloud.Message{
		{Role: hwcloud.RoleSystem, Content: plannerSystemPrompt},
		{Role: hwcloud.RoleUser, Content: buildReplanPrompt(input)},
	}

	var fullText string
	var err error
	if onChunk != nil {
		fullText, err = modelStreamCall(ctx, p.model, messages, onChunk)
	} else {
		fullText, err = modelSyncCall(ctx, p.model, messages)
	}
	if err != nil {
		return nil, fmt.Errorf("replan: model call failed: %w", err)
	}

	steps, err := parseStepsJSON(fullText)
	if err != nil {
		return nil, fmt.Errorf("replan: %w", err)
	}

	agentNames := make(map[string]bool)
	for _, a := range input.Agents {
		agentNames[a.Name] = true
	}
	// Validate just the replacement steps as a mini-plan.
	if err := Validate(&PlanDef{Goal: input.Goal, Steps: steps}, agentNames); err != nil {
		return nil, fmt.Errorf("replan validation failed: %w", err)
	}

	return steps, nil
}

func buildReplanPrompt(input ReplanInput) string {
	var b strings.Builder
	b.WriteString("## Replanning with User Feedback\n\n")
	b.WriteString(fmt.Sprintf("**Original goal:** %s\n\n", input.Goal))

	if input.FailureError != "" {
		b.WriteString(fmt.Sprintf("**Failure:** Step %q failed: %s\n\n", input.FailedStepID, input.FailureError))
	}
	b.WriteString(fmt.Sprintf("**User feedback:** %s\n\n", input.Feedback))

	if len(input.DoneSteps) > 0 {
		b.WriteString("## Completed Steps (do not regenerate these)\n\n")
		for _, s := range input.DoneSteps {
			b.WriteString(fmt.Sprintf("- **%s** (%s): %s\n", s.ID, s.Agent, s.Summary))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Steps that Need Replanning\n\n")
	for _, s := range input.Affected {
		b.WriteString(fmt.Sprintf("- %s (agent: %s, task: %s)\n", s.ID, s.Agent, s.Task))
	}

	if len(input.SurvivingIDs) > 0 {
		b.WriteString("\n## Surviving Step IDs (DO NOT reuse these)\n")
		b.WriteString(strings.Join(input.SurvivingIDs, ", "))
		b.WriteString("\n")
	}

	b.WriteString("\n**IMPORTANT**: The user has provided feedback above. Use it to guide your replanning — ")
	b.WriteString("choose different agents, rephrase tasks, or restructure the approach based on their suggestions.\n\n")
	b.WriteString("Generate a replacement plan for only the failed and affected steps. ")
	b.WriteString("Return ONLY the replacement steps as a JSON array:\n")
	b.WriteString(`[{"id": "...", "agent": "...", "task": "...", "depends_on": [...], "final": false}]`)
	return b.String()
}

// parseStepsJSON parses a JSON array of StepDef from raw model output.
func parseStepsJSON(raw string) ([]StepDef, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		if idx := strings.LastIndex(raw, "```"); idx >= 0 {
			raw = raw[:idx]
		}
		raw = strings.TrimSpace(raw)
	}

	var steps []StepDef
	if err := json.Unmarshal([]byte(raw), &steps); err != nil {
		return nil, fmt.Errorf("failed to parse steps JSON: %w\nRaw:\n%s", err, truncateStr(raw, 500))
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("no replacement steps generated")
	}
	return steps, nil
}

// planWithModel is the shared implementation for Plan and PlanStream.
func (p *LLMPlanner) planWithModel(ctx context.Context, goal string, agents []agent.AgentInfo, history []hwcloud.Message, streaming bool, onChunk func(string)) (*PlanDef, error) {
	agentNames := make(map[string]bool)
	for _, a := range agents {
		agentNames[a.Name] = true
	}

	prompt := buildPlannerPrompt(goal, agents, history)
	var lastErr error

	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		messages := []hwcloud.Message{
			{Role: hwcloud.RoleSystem, Content: plannerSystemPrompt},
			{Role: hwcloud.RoleUser, Content: prompt},
		}

		if attempt > 0 && lastErr != nil {
			messages = append(messages, hwcloud.Message{
				Role:    hwcloud.RoleUser,
				Content: fmt.Sprintf("Your previous plan was invalid. Please fix these issues:\n%s\n\nGenerate a corrected plan.", lastErr.Error()),
			})
		}

		var fullText string
		var err error

		if streaming {
			fullText, err = modelStreamCall(ctx, p.model, messages, onChunk)
		} else {
			fullText, err = modelSyncCall(ctx, p.model, messages)
		}
		if err != nil {
			return nil, fmt.Errorf("planner: model call failed: %w", err)
		}

		def, err := parsePlanJSON(fullText)
		if err != nil {
			lastErr = err
			continue
		}

		if err := Validate(def, agentNames); err != nil {
			lastErr = err
			continue
		}

		return def, nil
	}

	return nil, fmt.Errorf("planner: failed to generate a valid plan after %d attempts: %w", p.maxRetries+1, lastErr)
}

// modelSyncCall calls the model synchronously and returns the response text.
// MaxTokens is intentionally NOT set — see comment below.
func modelSyncCall(ctx context.Context, model hwcloud.Model, messages []hwcloud.Message) (string, error) {
	resp, err := model.ChatCompletion(ctx, hwcloud.ChatCompletionRequest{
		Messages: messages,
		// MaxTokens is intentionally NOT set. The model uses its own
		// default, which for reasoning models (deepseek-r1, o1) is a
		// shared budget for reasoning + output. A hardcoded limit would
		// unpredictably truncate the JSON when the model spends heavily
		// on reasoning — and no single number works for all goals.
		// The system prompt's "Reply with ONLY the JSON object" is the
		// sufficient semantic constraint.
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("model returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// modelStreamCall calls the model with streaming and returns the accumulated
// text. ReasoningContent deltas are sent to onChunk for display but NOT
// included in the returned text — only Content carries the plan JSON.
func modelStreamCall(ctx context.Context, model hwcloud.Model, messages []hwcloud.Message, onChunk func(string)) (string, error) {
	reader, err := model.ChatCompletionStream(ctx, hwcloud.ChatCompletionRequest{
		Messages: messages,
		// MaxTokens intentionally unset — see modelSyncCall.
	})
	if err != nil {
		return modelSyncCall(ctx, model, messages)
	}
	defer reader.Close()

	var fullText strings.Builder
	for reader.Next() {
		chunk := reader.Current()
		for _, delta := range chunk.Choices {
			if delta.ReasoningContent != "" {
				onChunk(delta.ReasoningContent)
			}
			if delta.Content != "" {
				fullText.WriteString(delta.Content)
				onChunk(delta.Content)
			}
		}
	}
	if err := reader.Err(); err != nil {
		return "", err
	}
	return fullText.String(), nil
}

// ── Prompt ──

const plannerSystemPrompt = `You are an expert planner. Decompose a goal into a DAG of steps that maximizes parallel execution.

Output ONLY a JSON object:
{
  "goal": "the original goal",
  "steps": [
    {
      "id": "unique_descriptive_id",
      "agent": "agent_name",
      "task": "specific task for this step only",
      "depends_on": ["step_ids_this_must_wait_for"],
      "final": false,
      "gate": false
    }
  ]
}

Rules:
- Every step that can run independently MUST run in parallel (no unnecessary depends_on).
- agent must be from the available agents list.
- final: true only for the last answer-producing step(s).
- gate: true when this step requires human approval before proceeding. Use gate=true for steps
  where the agent's description mentions "approval", "requires approval", "review", "gate",
  or "approval gate". After a gate step completes, execution pauses so a human can review its
  output before continuing.

Reply with ONLY the JSON object. No markdown fences, no explanation.`

func buildPlannerPrompt(goal string, agents []agent.AgentInfo, history []hwcloud.Message) string {
	var b strings.Builder

	b.WriteString("## Available Agents\n\n")
	for _, a := range agents {
		desc := a.Description
		if desc == "" {
			desc = "No description provided."
		}
		b.WriteString(fmt.Sprintf("- **%s**: %s\n", a.Name, desc))
	}

	b.WriteString("\n## Goal\n\n")
	b.WriteString(goal)

	if len(history) > 0 {
		b.WriteString("\n\n## Conversation History\n\n")
		for _, m := range history {
			b.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, truncateStr(m.Content, 500)))
		}
	}

	b.WriteString("\n\nGenerate the execution plan as JSON.")
	return b.String()
}

// ── JSON parsing ──

func parsePlanJSON(raw string) (*PlanDef, error) {
	// Strip markdown fences if present.
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		if idx := strings.LastIndex(raw, "```"); idx >= 0 {
			raw = raw[:idx]
		}
		raw = strings.TrimSpace(raw)
	}

	var def PlanDef
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		return nil, fmt.Errorf("failed to parse plan JSON: %w\nRaw response:\n%s", err, truncateStr(raw, 500))
	}

	if def.Goal == "" {
		return nil, fmt.Errorf("plan has no goal")
	}
	if len(def.Steps) == 0 {
		return nil, fmt.Errorf("plan has no steps")
	}

	return &def, nil
}

// truncateStr truncates s to at most n runes, counting by rune not byte.
func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
