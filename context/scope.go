package context

// ContextScope describes the ownership domain of context: which user,
// which project, which session (and, for teams, which partition) a piece
// of context belongs to.
//
// Providers (memory/skill/resource) use the scope to isolate knowledge:
// a user's preferences, a project's facts, a session's working state.
// Partition distinguishes team-shared ("") from agent-private
// ("private:<agentName>") storage within the same session.
type ContextScope struct {
	UserID    string `json:"user_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Partition string `json:"partition,omitempty"`
}
