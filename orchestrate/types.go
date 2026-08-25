// Package plan provides goal decomposition, DAG-based dependency graphs,
// concurrent step execution, and automatic replanning on failure.
//
// Plan is the second multi-agent orchestration mode (alongside Team):
//   - Team — agents decide routing at runtime via handoffs (transfer_to_*)
//   - Plan — a Planner generates a DAG before execution; an Executor
//     runs steps in topological order with parallel batches
//
// Both modes share the AgentRunner interface — a Step can be an Agent,
// a Team, or an external ACP agent.
package orchestrate

import (
	"context"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
)

// ── Status types ──

// PlanStatus is the lifecycle state of a plan.
type PlanStatus string

const (
	PlanStatusPlanning  PlanStatus = "planning"
	PlanStatusApproved  PlanStatus = "approved"
	PlanStatusRunning   PlanStatus = "running"
	PlanStatusDone      PlanStatus = "done"
	PlanStatusFailed    PlanStatus = "failed"
	PlanStatusCancelled PlanStatus = "cancelled"
)

// StepStatus is the execution state of a single step.
type StepStatus string

const (
	StepStatusPending StepStatus = "pending"
	StepStatusRunning StepStatus = "running"
	StepStatusDone    StepStatus = "done"
	StepStatusFailed  StepStatus = "failed"
	StepStatusSkipped StepStatus = "skipped" // dependency failed
)

// ── Plan definition (Planner output) ──

// StepDef defines one step in a plan DAG.
type StepDef struct {
	ID         string   `json:"id"`                    // unique within the plan
	Agent      string   `json:"agent"`                 // agent name (must exist in the plan's agent set)
	Task       string   `json:"task"`                  // what this step should accomplish
	DependsOn  []string `json:"depends_on,omitempty"`  // step IDs this step depends on
	Final      bool     `json:"final,omitempty"`       // true if this step's output is the plan's final answer
	Gate       bool     `json:"gate,omitempty"`        // true if executor pauses after this step for human approval
	MaxRetries int      `json:"max_retries,omitempty"` // per-step retry limit (0 = use plan default of 3)
}

// PlanDef is the static plan produced by a [Planner].
// It is a pure data struct — serialisable, user-editable, and replayable.
type PlanDef struct {
	Goal  string    `json:"goal"`
	Steps []StepDef `json:"steps"`
}

// ── Runtime state ──

// StepResult holds the outcome of executing one step.
type StepResult struct {
	Status      StepStatus      `json:"status"`
	Summary     string          `json:"summary"`         // LLM-generated concise summary
	FinalOutput string          `json:"final_output"`    // raw agent output (may be truncated)
	Error       string          `json:"error,omitempty"` // error message if failed
	Retries     int             `json:"retries"`         // actual retry count
	Usage       hwcloud.Usage `json:"usage"`           // token usage from this step
	StartTime   time.Time       `json:"start_time"`
	EndTime     time.Time       `json:"end_time"`
	// RunID is the child agent's run ID for this step (#1). Joins the
	// step to its agent trajectory via RunResult.RunID; the child's
	// ParentRunID points back to the plan's PlanRunID. Blank when the
	// step failed before producing a RunResult.
	RunID string `json:"run_id,omitempty"`
}

// PlanState is the runtime state of a plan execution.
// It is serialisable so that interrupted runs can be resumed
// (resume is a follow-up feature; the design leaves the door open).
type PlanState struct {
	ID          string                 `json:"id"`
	Goal        string                 `json:"goal"`
	Status      PlanStatus             `json:"status"`
	Steps       []StepDef              `json:"steps"`
	Results     map[string]*StepResult `json:"results"`
	ReplanCount int                    `json:"replan_count"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
	// PlanRunID is the plan-level run ID (#1): stable across replan
	// (all replan steps join the same planRunID) and resume
	// (ExecuteWithState reads it back so a resumed run joins the original
	// plan trajectory instead of starting a fresh ID). Set once by
	// Plan.execute at first entry; left blank by ExecuteWithState when the
	// caller passes a freshly-constructed state (a resumed run then stamps
	// a new planRunID, consistent with "resume = new run of the same plan").
	PlanRunID string `json:"plan_run_id,omitempty"`
}

// PlanResult is the final output of a plan execution.
type PlanResult struct {
	Goal        string          `json:"goal"`
	FinalOutput string          `json:"final_output"`
	Steps       []StepResult    `json:"steps"` // in execution order
	Usage       hwcloud.Usage `json:"usage"`
	ReplanCount int             `json:"replan_count"`
	// PlanRunID is the plan-level run ID (#1): joins all step RunIDs via
	// their ParentRunID and survives replan. Echoed from executor.planRunID
	// by buildResult. A consumer reassembles the full plan trajectory by
	// filtering DecisionEvents/StageEvents on ParentRunID == PlanRunID.
	PlanRunID string `json:"plan_run_id,omitempty"`
}

// ── Step context (data passed between steps) ──

// DepResult is the summarised result of a completed dependency step.
// It is assembled into [StepContext.Dependencies] for downstream steps.
type DepResult struct {
	StepID    string `json:"step_id"`
	AgentName string `json:"agent_name"`
	Task      string `json:"task"`
	Summary   string `json:"summary"`          // always present (LLM-generated or truncated output)
	Output    string `json:"output,omitempty"` // raw output, included when short enough
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
}

// StepContext is the input assembled for a step from its dependency results.
// The executor formats it as a system message and passes it via
// [agent.AgentRunner.RunWithPrefix].
type StepContext struct {
	Goal         string      `json:"goal"`
	Task         string      `json:"task"`
	Dependencies []DepResult `json:"dependencies"`
}

// ── Configuration ──

// PlanConfig holds execution tuning parameters.
type PlanConfig struct {
	// StepTimeout is the maximum duration for a single step.
	// 0 means no timeout. Default 5 minutes.
	StepTimeout time.Duration

	// MaxReplans is the maximum number of replanning attempts.
	// Default 3.
	MaxReplans int

	// MaxSteps is the maximum number of steps allowed in a plan.
	// Plans exceeding this are rejected. Default 20.
	MaxSteps int

	// MaxConcurrency is the maximum number of steps that can run
	// concurrently within a single topological batch.
	// Default 8.
	MaxConcurrency int

	// AutoReplan controls whether the executor automatically replans on step failure.
	// When true (default), a failed step triggers automatic replanning (up to MaxReplans times).
	// When false, the executor pauses on failure, sends [PlanEventWaitingRetry], and waits
	// for the caller to resume via [Plan.ExecuteWithState] — enabling manual retry or
	// replan-with-feedback workflows.
	AutoReplan bool
}

// DefaultPlanConfig returns sensible defaults.
func DefaultPlanConfig() PlanConfig {
	return PlanConfig{
		StepTimeout:    5 * time.Minute,
		MaxReplans:     3,
		MaxSteps:       20,
		MaxConcurrency: 8,
		AutoReplan:     true, // default: auto-replan; set false for manual retry/replan
	}
}

// ── Plan events (for streaming progress) ──

// PlanEventType categorises events emitted during plan execution.
type PlanEventType string

const (
	PlanEventGenerated    PlanEventType = "plan_generated"     // Planner produced a PlanDef
	PlanEventApproved     PlanEventType = "plan_approved"      // user approved the plan
	PlanEventStepStart    PlanEventType = "step_start"         // step began execution
	PlanEventTextDelta    PlanEventType = "step_text_delta"    // text token from a running step
	PlanEventToolCall     PlanEventType = "step_tool_call"     // tool call from a step
	PlanEventToolProgress PlanEventType = "step_tool_progress" // streaming tool output chunk
	PlanEventToolResult   PlanEventType = "step_tool_result"   // tool result (final)
	PlanEventStepDone     PlanEventType = "step_done"          // step completed successfully
	PlanEventStepFailed   PlanEventType = "step_failed"        // step failed
	PlanEventReplanning   PlanEventType = "replanning"         // replan in progress
	PlanEventWaitingRetry PlanEventType = "plan_waiting_retry" // paused, waiting for manual retry/replan
	PlanEventDone         PlanEventType = "plan_done"          // plan execution complete
	PlanEventError        PlanEventType = "plan_error"         // fatal error
)

// PlanEvent is emitted during plan execution for streaming progress.
type PlanEvent struct {
	Type    PlanEventType `json:"type"`
	StepID  string        `json:"step_id,omitempty"`
	Agent   string        `json:"agent,omitempty"`
	Text    string        `json:"text,omitempty"`   // text_delta, tool_result content, plan_done summary
	Result  *StepResult   `json:"result,omitempty"` // step_done, step_failed
	Goal    string        `json:"goal,omitempty"`   // plan_generated
	Def     *PlanDef      `json:"def,omitempty"`    // plan_generated
	ErrText string        `json:"error,omitempty"`  // plan_error, step_failed, waiting_retry
	Error   error         `json:"-"`                // internal, not serialised

	// Tool call detail (PlanEventToolCall).
	ToolName string `json:"tool_name,omitempty"` // function name
	ToolArgs string `json:"tool_args,omitempty"` // JSON arguments
	ToolID   string `json:"tool_id,omitempty"`   // tool call ID from model
}

// ── Replanning ──

// ReplanDoneStep summarises a completed step for the Planner.
// It is the minimal information the Planner needs to understand what
// has been accomplished, without exposing internal executor state.
type ReplanDoneStep struct {
	ID      string
	Agent   string
	Summary string
}

// ReplanInput is the context the Planner needs to regenerate a failed plan.
// The executor builds this from its internal state; the Planner consumes it
// without knowing anything about the executor.
type ReplanInput struct {
	Goal         string           // original plan goal
	FailedStepID string           // which step failed
	FailureError string           // why it failed
	Feedback     string           // user's natural language suggestions
	DoneSteps    []ReplanDoneStep // completed steps (context, do not regenerate)
	Affected     []StepDef        // steps that need replacing
	Agents       []agent.AgentInfo
	SurvivingIDs []string // step IDs that must NOT be reused
}

// ── Manual intervention (retry / replan-with-feedback) ──

// RetryAction is a command from the caller to resume a paused plan execution.
// It is sent to the channel returned by [Plan.ResumeChan] when the executor
// pauses with [PlanEventWaitingRetry].
type RetryAction struct {
	Action   string   // "retry" or "replan"
	StepID   string   // step to retry (for "retry")
	Feedback string   // user feedback for replan (for "replan")
	NewDef   *PlanDef // replacement plan def (for "replan", pre-computed by caller)
}

// ── Plan approver (user confirmation gate) ──

// PlanApprover allows the user to review and approve/reject a generated plan
// before execution starts. nil means auto-approve (Run runs Plan+Execute in one step).
type PlanApprover interface {
	ApprovePlan(ctx context.Context, def *PlanDef) (approved bool, reason string)
}
