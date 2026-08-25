// Stream example: streaming Agent with real-time output via RunStream.
// Demonstrates text_delta, tool_call, tool_result, retrying, and done events.
//
// Environment variables:
//
//	HWCLOUD_BASE_URL   — API base URL
//	HWCLOUD_MODEL      — model ID
//	HWCLOUD_API_KEY    — API key
//
//	go run ./examples/stream/
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
		ID:     "stream-session-1",
		UserID: "user-1",

		ModelID:   modelID,
		CreatedAt: time.Now(),
	}

	fmt.Printf("Model: %s | Base URL: %s\n\n", modelID, baseURL)

	ctx := context.Background()
	events := kernel.New(cfg, deps).RunStream(ctx, session, hwcloud.UserMessage("what is 12 + 34?"))

	var inThought bool
	fmt.Print("Assistant: ")
	for event := range events {
		switch event.Type {
		case hwcloud.StreamThought:
			if !inThought {
				fmt.Println()
				fmt.Print("🧠 Thinking: ")
				inThought = true
			}
			fmt.Print(event.Text)

		case hwcloud.StreamTextDelta:
			if inThought {
				fmt.Println()
				fmt.Print("Assistant: ")
				inThought = false
			}
			fmt.Print(event.Text) // real-time character output

		case hwcloud.StreamToolCall:
			for _, tc := range event.Message.ToolCalls {
				fmt.Printf("\n🔧 calling %s(%s)...\n", tc.Function.Name, tc.Function.Arguments)
			}

		case hwcloud.StreamToolResult:
			fmt.Printf("📦 %s\n", truncate(event.Message.Content, 120))
			fmt.Print("Assistant: ")

		case hwcloud.StreamRetrying:
			fmt.Printf("\n⏳ retrying: %v\n", event.Error)
			fmt.Print("Assistant: ")

		case hwcloud.StreamDone:
			r := event.Result
			fmt.Printf("\n\n=== Done ===\n")
			fmt.Printf("Turns: %d | Tokens: prompt=%d completion=%d total=%d\n",
				r.TurnCount, r.Usage.PromptTokens, r.Usage.CompletionTokens, r.Usage.TotalTokens)

		case hwcloud.StreamError:
			fmt.Fprintf(os.Stderr, "\nERROR: %v\n", event.Error)
			os.Exit(1)
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
