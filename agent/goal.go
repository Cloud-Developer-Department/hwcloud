package agent

import (
	"fmt"
	"strings"
)

// WithGoalInstructions clones the agent with goal-mode instructions
// injected into the system prompts (used by RunGoal / RunGoalStream).
func (a *Agent) WithGoalInstructions(goal string) *Agent {
	clone := a.Clone()
	if len(goal) <= 0 {
		return clone
	}
	clone.SystemPrompts = buildGoalPrompts(clone.SystemPrompts, goal, clone.MaxTurns)
	return clone
}

// buildGoalPrompts appends the goal-mode system prompt to the agent's
// prompts, and caps MaxTurns to the goal budget.
func buildGoalPrompts(prompts []string, goal string, maxTurns int) []string {
	goalPrompt := fmt.Sprintf(`You are operating autonomously to achieve the following goal:

<goal>
%s
</goal>

You have up to %d turns to complete the goal. Work step by step, verify your
progress, and use tools when they help. When the goal is achieved — or you
are certain it cannot be — stop and report what you did.`, goal, maxTurns)
	out := make([]string, 0, len(prompts)+1)
	out = append(out, prompts...)
	out = append(out, goalPrompt)
	return out
}

// IsGoalPrompted reports whether the agent's system prompts carry a
// goal-mode instruction block.
func IsGoalPrompted(prompts []string) bool {
	for _, p := range prompts {
		if strings.Contains(p, "<goal>") {
			return true
		}
	}
	return false
}
