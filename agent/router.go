package agent

import (
	"context"
	"fmt"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// AgentInfo is a summary of an agent exposed to the Router.
type AgentInfo struct {
	Name        string
	Description string
	Type        AgentType // internal (in-process) or external (ACP/MCP)
}

// RouteResult is the output of Router.Route.
type RouteResult struct {
	Agent    string // selected agent name
	Fallback bool   // true if the router fell back (error, fuzzy match, or default)
	Reason   string // why fallback occurred (empty for confident routing)
}

// Router decides message routing in a Team.
//
// Two responsibilities:
//   - Route: pick the first agent for a new user message
//   - CanHandoff: policy gate on handoffs (nil return = allowed)
//
// nil Router = all agents are valid targets, first agent handles all input.
type Router interface {
	// Route picks the initial agent for a user message.
	// The returned RouteResult.Agent must match an agent in the Team.
	// Fallback indicates the router was not confident (model error, fuzzy match).
	Route(ctx context.Context, input hwcloud.Message, agents []AgentInfo) (RouteResult, error)

	// CanHandoff checks whether a handoff should proceed.
	// Return nil if allowed. Return an error to veto — the error message
	// is injected as a hint into the next agent's prompt.
	// Called before each handoff (not for initial routing).
	CanHandoff(ctx context.Context, entry HandoffEntry, chain []HandoffEntry, session hwcloud.Session) error
}

// ── Default Router: first-agent-wins ──

// FirstAgentRouter routes every message to the first agent in the Team.
// It never vetoes handoffs. Suitable for single-agent or fully model-driven
// handoff scenarios.
type FirstAgentRouter struct{}

func (FirstAgentRouter) Route(_ context.Context, _ hwcloud.Message, agents []AgentInfo) (RouteResult, error) {
	if len(agents) == 0 {
		return RouteResult{}, fmt.Errorf("team has no agents")
	}
	return RouteResult{Agent: agents[0].Name}, nil
}

func (FirstAgentRouter) CanHandoff(_ context.Context, _ HandoffEntry, _ []HandoffEntry, _ hwcloud.Session) error {
	return nil // never veto
}

// ── LLM Router ──

// LLMRouter uses a Model to decide routing. For Route, it asks the model
// which agent best matches the user's intent. CanHandoff always allows.
type LLMRouter struct {
	model hwcloud.Model
}

// NewLLMRouter creates a Router backed by a judge model.
func NewLLMRouter(model hwcloud.Model) *LLMRouter {
	return &LLMRouter{model: model}
}

func (r *LLMRouter) Route(ctx context.Context, input hwcloud.Message, agents []AgentInfo) (RouteResult, error) {
	if len(agents) == 0 {
		return RouteResult{}, fmt.Errorf("team has no agents")
	}
	if len(agents) == 1 {
		return RouteResult{Agent: agents[0].Name}, nil
	}

	prompt := buildRouterPrompt(agents)
	resp, err := r.model.ChatCompletion(ctx, hwcloud.ChatCompletionRequest{
		Messages: []hwcloud.Message{
			{Role: hwcloud.RoleSystem, Content: prompt},
			{Role: hwcloud.RoleUser, Content: input.Content},
		},
		MaxTokens: 64,
	})
	if err != nil {
		return RouteResult{Agent: agents[0].Name, Fallback: true, Reason: fmt.Sprintf("model error: %v", err)}, nil
	}
	if len(resp.Choices) == 0 {
		return RouteResult{Agent: agents[0].Name, Fallback: true, Reason: "model returned no choices"}, nil
	}

	// The model should return just the agent name
	chosen := resp.Choices[0].Message.Content
	for _, a := range agents {
		if a.Name == chosen {
			return RouteResult{Agent: a.Name}, nil
		}
	}
	// Fuzzy match: check if any agent name appears in the response
	for _, a := range agents {
		if containsWord(chosen, a.Name) {
			return RouteResult{Agent: a.Name, Fallback: true, Reason: fmt.Sprintf("fuzzy matched %q from response %q", a.Name, chosen)}, nil
		}
	}
	return RouteResult{Agent: agents[0].Name, Fallback: true, Reason: fmt.Sprintf("unrecognized response %q", chosen)}, nil
}

func (r *LLMRouter) CanHandoff(_ context.Context, _ HandoffEntry, _ []HandoffEntry, _ hwcloud.Session) error {
	return nil
}

func buildRouterPrompt(agents []AgentInfo) string {
	p := "You are a router. Given a user message, pick the best agent.\n\nAgents:\n"
	for _, a := range agents {
		p += fmt.Sprintf("- %s: %s\n", a.Name, a.Description)
	}
	p += "\nReply with ONLY the agent name (one word)."
	return p
}

func containsWord(s, word string) bool {
	return strings.Contains(s, word)
}
