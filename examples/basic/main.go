// Basic example: non-streaming Agent with a real Model and one Tool.
// Verifies the full 8-node mainline loop against an actual LLM.
//
// Environment variables:
//
//	HWCLOUD_BASE_URL   — API base URL
//	HWCLOUD_MODEL      — model ID
//	HWCLOUD_API_KEY    — API key
//
//	go run ./examples/basic/
package main

import (
	"context"
	"encoding/json"
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

	model := openai.New(apiKey, modelID, baseURL).
		WithContextWindow(128_000)

	cfg := agent.New("calculator",
		agent.WithModel(model),
		agent.WithSystemPrompts("You are a precise calculator. Use the calculator tool for arithmetic. Answer concisely."),
	)
	deps := kernel.Deps{
		Tools: []hwcloud.Tool{&calculatorTool{}},
	}

	session := hwcloud.Session{
		ID:     "basic-session-1",
		UserID: "user-1",

		ModelID:   modelID,
		CreatedAt: time.Now(),
	}

	ctx := context.Background()
	result, err := kernel.New(cfg, deps).Run(ctx, session, hwcloud.UserMessage("what is 12 + 34?"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("=== Run Result ===")
	fmt.Printf("Final output: %s\n", result.FinalOutput)
	fmt.Printf("Turns: %d\n", result.TurnCount)
	fmt.Printf("Tokens: prompt=%d completion=%d total=%d\n",
		result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens)
	fmt.Println("\n=== Messages ===")
	for i, msg := range result.Messages {
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				fmt.Printf("[%d] %s → tool_call: %s(%s)\n", i, msg.Role, tc.Function.Name, tc.Function.Arguments)
			}
		} else {
			fmt.Printf("[%d] %s: %s\n", i, msg.Role, truncate(msg.Content, 80))
		}
	}
}

// ── Calculator Tool ──

type calculatorTool struct{}

func (t *calculatorTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "calculator",
		Description: "Evaluate a mathematical expression. Input is a valid arithmetic expression like '12+34' or '100/3'.",
		Parameters:  hwcloud.SchemaOf[CalcParams](),
	}
}

func (t *calculatorTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[CalcParams](args)
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
	// Remove spaces — model may produce "12 + 34" or "12+34"
	expr := strings.ReplaceAll(params.Expression, " ", "")
	var a, b int
	var op rune
	fmt.Sscanf(expr, "%d%c%d", &a, &op, &b)
	switch op {
	case '+':
		return &hwcloud.ToolResult{Content: fmt.Sprintf("%d", a+b)}
	case '-':
		return &hwcloud.ToolResult{Content: fmt.Sprintf("%d", a-b)}
	case '*':
		return &hwcloud.ToolResult{Content: fmt.Sprintf("%d", a*b)}
	case '/':
		if b == 0 {
			return hwcloud.ErrorResult(fmt.Errorf("division by zero"), false, "")
		}
		return &hwcloud.ToolResult{Content: fmt.Sprintf("%d", a/b)}
	default:
		return hwcloud.ErrorResult(fmt.Errorf("unsupported operator: %c", op), false, "")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type CalcParams struct {
	Expression string `json:"expression" jsonschema:"description=The math expression to evaluate"`
}
