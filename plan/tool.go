package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// OnPlan is called after plan_create or plan_update produces new entries.
// The caller receives the complete snapshot in execution order.
type OnPlan func(entries []Entry)

// CreateTool is an hwcloud.Tool named "plan_create". The LLM outputs
// structured plan entries directly via function-calling arguments — there
// is no internal model call. The tool validates, persists, and notifies.
type CreateTool struct {
	onPlan OnPlan
}

// NewCreateTool creates a plan_create tool.
func NewCreateTool(onPlan OnPlan) *CreateTool {
	return &CreateTool{onPlan: onPlan}
}

// Definition implements hwcloud.Tool.
func (t *CreateTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "plan_create",
		Description: `Create a structured execution plan for a complex task. Use this when a task involves multiple steps, spans multiple files, or requires careful sequencing.

Once the plan is complete, call exit_plan_mode to return to your previous mode and begin executing each step. Use plan_update to track progress during execution.`,
		Parameters: hwcloud.SchemaOf[PlanCreateParams](),
	}
}

// Execute implements hwcloud.Tool.
func (t *CreateTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[PlanCreateParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("plan_create: %w", err), false, "")
	}
	if strings.TrimSpace(params.Goal) == "" {
		return hwcloud.ErrorResult(fmt.Errorf("plan_create: goal is required"), false, "")
	}
	if len(params.Steps) == 0 {
		return hwcloud.ErrorResult(fmt.Errorf("plan_create: at least one step is required"), false, "")
	}

	entries := make([]Entry, len(params.Steps))
	for i, s := range params.Steps {
		if strings.TrimSpace(s.ID) == "" {
			return hwcloud.ErrorResult(fmt.Errorf("plan_create: step %d has empty id", i+1), false, "")
		}
		if strings.TrimSpace(s.Content) == "" {
			return hwcloud.ErrorResult(fmt.Errorf("plan_create: step %d has empty content", i+1), false, "")
		}
		p := PriorityMedium
		switch s.Priority {
		case "high":
			p = PriorityHigh
		case "medium":
			p = PriorityMedium
		case "low":
			p = PriorityLow
		}
		entries[i] = Entry{ID: s.ID, Content: s.Content, Priority: p, Status: StatusPending}
	}

	if t.onPlan != nil {
		t.onPlan(copyEntries(entries))
	}

	return &hwcloud.ToolResult{Content: formatPlan(params.Goal, entries)}
}

// formatPlan renders entries as human-readable text for the agent's context.
func formatPlan(goal string, entries []Entry) string {
	var b strings.Builder
	b.WriteString("## Plan\n\n**Goal:** ")
	b.WriteString(goal)
	b.WriteString("\n\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "- [%s] [%s] `%s` %s\n", e.Priority, e.Status, e.ID, e.Content)
	}
	b.WriteString("\nWork through each step in order. Use plan_update to mark progress — reference each step by its `id`.")
	return b.String()
}

func copyEntries(src []Entry) []Entry {
	dst := make([]Entry, len(src))
	copy(dst, src)
	return dst
}

// ── plan_update Tool ──

// Update is a single status change for a plan entry.
type Update struct {
	ID     string `json:"id"`     // matches the id field from plan_create steps
	Status string `json:"status"` // "pending","in_progress","completed"
}

// OnUpdate is called when plan_update executes.
type OnUpdate func(updates []Update) ([]Entry, error)

// UpdateTool is an hwcloud.Tool named "plan_update". The agent calls it
// to mark plan entry progress. The OnUpdate callback applies the changes
// and persists them externally — the tool itself is a pure conduit.
type UpdateTool struct {
	onUpdate OnUpdate
}

// NewUpdateTool creates a plan_update tool.
func NewUpdateTool(onUpdate OnUpdate) *UpdateTool {
	return &UpdateTool{onUpdate: onUpdate}
}

// Definition implements hwcloud.Tool.
func (t *UpdateTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "plan_update",
		Description: `Update the status of one or more plan entries. Call this as you start working on a step (in_progress) or after completing it (completed). Reference each step by its id (shown after the status in the plan).`,
		Parameters:  hwcloud.SchemaOf[PlanUpdateParams](),
	}
}

// Execute implements hwcloud.Tool.
func (t *UpdateTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[PlanUpdateParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("plan_update: %w", err), false, "")
	}
	if len(params.Updates) == 0 {
		return hwcloud.ErrorResult(fmt.Errorf("plan_update: at least one update required"), false, "")
	}

	entries, err := t.onUpdate(params.Updates)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("plan_update: %w", err), false, "")
	}

	return &hwcloud.ToolResult{Content: formatPlan("", entries)}
}

// ── enter_plan_mode Tool ──

// EnterTool is an hwcloud.Tool named "enter_plan_mode". The agent calls it
// to enter plan mode when a task requires structured planning. In plan mode,
// execution tools are removed and plan_create / exit_plan_mode become available.
//
// The onEnter callback is called by Execute to transition the session. It is
// wired by the ACP server at OnPrompt time.
type EnterTool struct {
	onEnter func() error
}

// NewEnterTool creates an enter_plan_mode tool.
func NewEnterTool(onEnter func() error) *EnterTool {
	return &EnterTool{onEnter: onEnter}
}

// Definition implements hwcloud.Tool.
func (t *EnterTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "enter_plan_mode",
		Description: `Enter plan mode to create a structured execution plan. Use this when the task is complex, involves multiple steps, spans multiple files, or requires careful sequencing.

After entering plan mode, your execution tools (shell, file writes, terminal) are temporarily removed. Your read-only tools (read, ls, grep, webfetch, websearch, recall, load_skill, reload_skills) remain available, and you gain access to plan_create, plan_update, and exit_plan_mode.

Workflow: enter_plan_mode → plan_create → exit_plan_mode → execute`,
		Parameters: hwcloud.SchemaOf[struct{}](),
	}
}

// Execute implements hwcloud.Tool. It calls the onEnter callback to
// transition the session mode, then returns confirmation text.
func (t *EnterTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	if t.onEnter == nil {
		return &hwcloud.ToolResult{Content: "enter_plan_mode: no mode transition callback configured.\n"}
	}
	if err := t.onEnter(); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("enter_plan_mode: %w", err), false, "")
	}
	return &hwcloud.ToolResult{Content: "Entered plan mode. Execution tools are disabled. You now have access to read-only tools (read, ls, grep, webfetch, websearch, recall, load_skill, reload_skills) and plan tools (plan_create, plan_update, exit_plan_mode). Create a plan with plan_create, then call exit_plan_mode when ready to execute.\n"}
}

// ── exit_plan_mode Tool ──

// ExitTool is an hwcloud.Tool named "exit_plan_mode". The agent calls it
// to leave plan mode and gain execution tools (shell, write, delete, terminal,
// etc.). The session returns to the mode that was active before entering plan
// mode (auto or manual).
//
// The onExit callback is called by Execute to transition the session. It is
// wired by the ACP server at OnPrompt time.
type ExitTool struct {
	onExit func() error
}

// NewExitTool creates an exit_plan_mode tool.
func NewExitTool(onExit func() error) *ExitTool {
	return &ExitTool{onExit: onExit}
}

// Definition implements hwcloud.Tool.
func (t *ExitTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "exit_plan_mode",
		Description: `Exit plan mode and return to the previous mode (auto or manual) to begin executing the plan. Call this once you have created a complete plan with plan_create. You will regain access to execution tools (shell, write, terminal, etc.).

Only call this once, and only when you are ready to start executing the plan steps.`,
		Parameters: hwcloud.SchemaOf[struct{}](),
	}
}

// Execute implements hwcloud.Tool. It calls the onExit callback to
// transition the session mode, then returns confirmation text.
func (t *ExitTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	if t.onExit == nil {
		return &hwcloud.ToolResult{Content: "exit_plan_mode: no mode transition callback configured.\n"}
	}
	if err := t.onExit(); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("exit_plan_mode: %w", err), false, "")
	}
	return &hwcloud.ToolResult{Content: "Exited plan mode. You now have access to execution tools (shell, write, terminal, etc.). Use plan_update to track progress as you work through the plan steps.\n"}
}

// PlanStep is one entry of a plan_create steps array. Fields mirror the
// JSON shape the model emits; requiredness follows the pre-refactor schema
// (id/content/priority all required).
type PlanStep struct {
	ID       string `json:"id" jsonschema:"description=Stable identifier for this step (e.g. 'step-1', 'explore-auth'). plan_update references this id."`
	Content  string `json:"content" jsonschema:"description=What this step should accomplish. Be specific — name files, functions, or operations."`
	Priority string `json:"priority" jsonschema:"description=high=critical dependencies, medium=main work, low=cleanup/docs."`
}

type PlanCreateParams struct {
	Goal  string     `json:"goal" jsonschema:"description=The objective to accomplish — a clear one-line summary."`
	Steps []PlanStep `json:"steps" jsonschema:"description=Ordered execution steps. Each must be concrete, self-contained, and actionable by an AI coding agent with file/code/tools access. Start with exploration/analysis, end with validation."`
}

type PlanUpdateParams struct {
	Updates []Update `json:"updates"`
}
