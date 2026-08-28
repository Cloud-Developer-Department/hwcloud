// Team example: multi-agent software development workflow.
//
//	go run ./examples/team/
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/model/openai"
)

func main() {
	apiKey := os.Getenv("HWCLOUD_API_KEY")
	modelID := os.Getenv("HWCLOUD_MODEL")
	baseURL := os.Getenv("HWCLOUD_BASE_URL")

	sharedModel := openai.New(apiKey, modelID, baseURL).
		WithContextWindow(128_000)

	// ── Analyst: understands requirements, produces a spec ──
	analyst := agent.New("analyst",
		agent.WithModel(sharedModel),
		agent.WithSystemPrompts(`You are a requirements analyst. Your job:
1. Understand the user's request
2. Break it down into clear, testable requirements
3. Hand off to the designer with a structured specification
Be specific — include constraints, edge cases, and acceptance criteria.`),
		agent.WithMaxTurns(2),
	)

	// ── Designer: architecture and component design ──
	designer := agent.New("designer",
		agent.WithModel(sharedModel),
		agent.WithSystemPrompts(`You are a software designer. Your job:
1. Take the analyst's specification and design the architecture
2. Define components, interfaces, and data flow
3. Hand off to the coder with a clear design document
Be specific about types, function signatures, and module boundaries.`),
		agent.WithMaxTurns(2),
	)

	// ── Coder: writes production code ──
	coder := agent.New("coder",
		agent.WithModel(sharedModel),
		agent.WithSystemPrompts(`You are a software developer. Your job:
1. Take the designer's spec and write clean, idiomatic Go code
2. Include error handling, comments, and tests
3. Hand off the complete implementation to the tester
Output ONLY code with brief inline comments.`),
		agent.WithMaxTurns(3),
	)

	// ── Tester: writes and runs tests ──
	tester := agent.New("tester",
		agent.WithModel(sharedModel),
		agent.WithSystemPrompts(`You are a QA engineer. Your job:
1. Review the coder's implementation
2. Identify edge cases and write tests for them
3. If all tests pass, hand off to the reviewer with your test report
4. If tests fail, report the failures clearly — do NOT fix the code
Be thorough. List what you tested and why.`),
		agent.WithMaxTurns(2),
	)

	// ── Reviewer: final quality gate ──
	reviewer := agent.New("reviewer",
		agent.WithModel(sharedModel),
		agent.WithSystemPrompts(`You are a code reviewer. Your job:
1. Review the complete implementation and test results
2. Check for correctness, style, performance, and security
3. Produce a final review summary: approved, changes requested, or rejected
4. If approved, present the complete deliverable to the user
Do NOT hand off — you are the final gate.`),
		agent.WithMaxTurns(1),
	)

	// Binder: build a fresh Runtime per agent turn from the pure config.
	// Two things MUST come from the TeamContext:
	//  1. tc.HandoffTools — the transfer_to_* tools (without them the
	//     model cannot hand off)
	//  2. tc.TeamPrompt — the "## Team Context" block (members, handoff
	//     rules, history); without it the model doesn't know the team.
	bind := func(cfg *agent.Agent) agent.AgentBinder {
		return func(tc agent.TeamContext) (agent.AgentRunner, error) {
			sub := cfg.Clone()
			sub.SystemPrompts = append(sub.SystemPrompts, tc.TeamPrompt)

			// 辅助打印:确认 team 上下文进了系统提示词
			fmt.Printf("\n── %s system prompts ──\n%s\n", cfg.Name, strings.Join(sub.SystemPrompts, "\n\n"))
			fmt.Printf("   handoff tools: %d\n", len(tc.HandoffTools))

			return kernel.New(sub, kernel.Deps{Tools: tc.HandoffTools}), nil
		}
	}

	// ── Build team ──
	team := agent.NewTeam(
		agent.WithTeamAgent("analyst", "Understands requirements and produces specifications", analyst, bind(analyst)),
		agent.WithTeamAgent("designer", "Designs architecture, components, and data flow", designer, bind(designer)),
		agent.WithTeamAgent("coder", "Writes clean, idiomatic Go code with error handling", coder, bind(coder)),
		agent.WithTeamAgent("tester", "Writes tests, identifies edge cases, reports results", tester, bind(tester)),
		agent.WithTeamAgent("reviewer", "Reviews code for correctness, style, and security", reviewer, bind(reviewer)),
		agent.WithTeamMaxHandoffs(5),
	)

	ctx := context.Background()
	session := hwcloud.Session{
		ID:     "team-session-1",
		UserID: "user-1",

		ModelID:   modelID,
		CreatedAt: time.Now(),
	}

	fmt.Println("=== Team: analyst → designer → coder → tester → reviewer ===")
	fmt.Printf("User: Write a function that validates email addresses\n\n")

	var result *agent.TeamResult
	for evt := range team.RunStream(ctx, session, hwcloud.UserMessage("Write a function that validates email addresses")) {
		switch evt.Type {
		case agent.TeamAgentStart:
			fmt.Printf("\n── %s ──\n", evt.Agent)
		case agent.TeamAgentEnd:
			// done.
		case agent.TeamHandoff:
			fmt.Printf("\n  → %s: %s\n", evt.Target, truncate(evt.Message, 120))
		case agent.TeamTextDelta:
			fmt.Print(evt.Text)
		case agent.TeamToolCall:
			if evt.ToolCall != nil {
				fmt.Printf("\n  🔧 %s(%s)\n", evt.ToolCall.Function.Name, truncate(evt.ToolCall.Function.Arguments, 120))
			}
		case agent.TeamToolResult:
			fmt.Printf("  → %s\n", truncate(evt.Text, 200))
		case agent.TeamDone:
			result = evt.Result
		case agent.TeamError:
			fmt.Fprintf(os.Stderr, "\nERROR: %v\n", evt.Error)
			os.Exit(1)
		}
	}
	fmt.Println()

	if result == nil {
		fmt.Fprintln(os.Stderr, "Team returned no result")
		os.Exit(1)
	}

	fmt.Printf("\nHandoffs: %d\n", len(result.HandoffChain))
	for i, h := range result.HandoffChain {
		fmt.Printf("  %d. %s → %s: %s\n", i+1, h.From, h.To, truncate(h.Message, 120))
	}
	fmt.Printf("Total turns: %d, Tokens: prompt=%d completion=%d total=%d\n",
		result.TotalTurns, result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
