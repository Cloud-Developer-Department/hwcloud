// Delegate example: demonstrates parallel delegation with Agent.AsTool().
//
// Unlike Team (serial handoff chain), AsTool lets a coordinator agent
// call multiple sub-agents in parallel. Each sub-agent runs with isolated
// context — it sees only its own instructions and the task, not the
// coordinator's conversation history or other sub-agents' work.
//
//	go run ./examples/delegate/
package main

import (
	"context"
	"fmt"
	"os"
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

	// ── Sub-agent 1: researcher — analyzes facts ──
	researcher := agent.New("researcher",
		agent.WithModel(sharedModel),
		agent.WithDescription("Analyzes data and finds key insights."),
		agent.WithSystemPrompts(`You are a researcher. Analyze the given task and provide:
1. Key facts and data points
2. Important trends or patterns
3. A concise 2-3 sentence summary

Be thorough but concise. Only respond with your analysis — do not ask follow-up questions.`),
		agent.WithMaxTurns(1),
	)

	// ── Sub-agent 2: writer — writes reports ──
	writer := agent.New("writer",
		agent.WithModel(sharedModel),
		agent.WithDescription("Writes clear, well-structured reports."),
		agent.WithSystemPrompts(`You are a technical writer. Based on the task you receive, write:
1. A clear title
2. 2-3 paragraphs of well-structured content
3. A brief conclusion

Write in a professional but accessible tone. Only respond with your writing — do not ask follow-up questions.`),
		agent.WithMaxTurns(1),
	)

	// ── Coordinator: delegates to researcher + writer in parallel ──
	coordinator := agent.New("coordinator",
		agent.WithModel(sharedModel),
		agent.WithSystemPrompts(`You are a coordinator. When given a topic to report on:
1. Call BOTH researcher and writer tools IN THE SAME TURN
   - researcher: ask it to analyze the topic
   - writer: ask it to write a brief report on the topic
2. After receiving both results, synthesize them into a final response.
   Mention what each sub-agent contributed.

IMPORTANT: Call both tools in a single response so they run in parallel.`),
		agent.WithMaxTurns(3),
	)

	// Runtime deps: sub-agents run isolated (no tools/memory/hooks of their own).
	coordDeps := kernel.Deps{
		Tools: []hwcloud.Tool{
			kernel.AsTool(researcher, kernel.Deps{}),
			kernel.AsTool(writer, kernel.Deps{}),
		},
	}

	ctx := context.Background()
	session := hwcloud.Session{
		ID:     "delegate-session-1",
		UserID: "user-1",

		ModelID:   modelID,
		CreatedAt: time.Now(),
	}

	// ── Run ──
	fmt.Println("=== Parallel Delegate: coordinator + researcher + writer ===")
	fmt.Println("User: Research and write a report about the Go programming language")
	fmt.Println()

	result, err := kernel.New(coordinator, coordDeps).Run(ctx, session,
		hwcloud.UserMessage("Research and write a brief report about the Go programming language. Cover its origins, key features, and why it's popular for backend development."))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Final output:\n%s\n\n", result.FinalOutput)
	fmt.Printf("Turns: %d\n", result.TurnCount)
	fmt.Printf("Messages: %d\n", len(result.Messages))
	for i, msg := range result.Messages {
		role := msg.Role
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				fmt.Printf("  [%d] %s → tool_call: %s(%s)\n", i, role, tc.Function.Name, trunc(tc.Function.Arguments, 80))
			}
		} else if msg.ToolCallID != "" {
			fmt.Printf("  [%d] %s → tool_result(%s): %s\n", i, role, msg.ToolCallID, trunc(msg.Content, 80))
		} else {
			fmt.Printf("  [%d] %s: %s\n", i, role, trunc(msg.Content, 120))
		}
	}
	fmt.Printf("Tokens: prompt=%d completion=%d total=%d\n",
		result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
