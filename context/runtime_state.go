package context

import (
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
)

// ExecutionState tracks one tool execution job.
type ExecutionState struct {
	CallID    string    `json:"call_id"`
	Tool      string    `json:"tool"`
	Status    string    `json:"status"` // pending | running | done | cancelled | failed
	StartedAt time.Time `json:"started_at,omitempty"`
}

// ApprovalRecord is one policy decision for a tool call.
type ApprovalRecord struct {
	Call   hwcloud.ToolCall        `json:"call"`
	Tool   string                    `json:"tool"`
	Action governance.ApprovalAction `json:"action"`
	Reason string                    `json:"reason"`
}

// RuntimeState is the execution bookkeeping of a run — what is happening
// right now. It is deliberately separate from AgentContext (what the agent
// knows): RuntimeState feeds runtime control (cancel compensation, audit,
// recovery), AgentContext feeds the LLM.
type RuntimeState struct {
	SessionID  string           `json:"session_id"`
	Turn       int              `json:"turn"`
	Executions []ExecutionState `json:"executions,omitempty"`
	Approvals  []ApprovalRecord `json:"approvals,omitempty"`
}

// RecordExecution appends or updates an execution entry by call ID.
func (s *RuntimeState) RecordExecution(callID, tool, status string, startedAt time.Time) {
	for i := range s.Executions {
		if s.Executions[i].CallID == callID {
			s.Executions[i].Status = status
			return
		}
	}
	s.Executions = append(s.Executions, ExecutionState{
		CallID: callID, Tool: tool, Status: status, StartedAt: startedAt,
	})
}

// RecordApproval appends an approval decision.
func (s *RuntimeState) RecordApproval(call hwcloud.ToolCall, tool string, action governance.ApprovalAction, reason string) {
	s.Approvals = append(s.Approvals, ApprovalRecord{
		Call: call, Tool: tool, Action: action, Reason: reason,
	})
}

// InFlight returns the call IDs of executions that have not finished.
func (s *RuntimeState) InFlight() []string {
	var out []string
	for _, e := range s.Executions {
		if e.Status == "pending" || e.Status == "running" {
			out = append(out, e.CallID)
		}
	}
	return out
}
