// Package plan provides plan_create / plan_update Tools that agents can
// invoke to decompose complex goals into structured plans and track progress.
//
// The agent outputs plan entries directly via function-calling arguments —
// the tool does no internal model calls. It validates, persists, and
// notifies the client. All types follow ACP v1 PlanEntry schema.
package plan

// Priority indicates the relative importance of a plan entry.
// Per ACP spec: "high", "medium", "low".
type Priority string

const (
	PriorityHigh   Priority = "high"
	PriorityMedium Priority = "medium"
	PriorityLow    Priority = "low"
)

// Status is the execution state of a single plan entry.
// Per ACP spec: "pending", "in_progress", "completed".
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

// Entry is a single task in an execution plan. Matches ACP PlanEntry schema
// plus an internal ID for stable cross-turn references (not part of ACP).
type Entry struct {
	ID       string   `json:"id"`
	Content  string   `json:"content"`
	Priority Priority `json:"priority"`
	Status   Status   `json:"status"`
}
