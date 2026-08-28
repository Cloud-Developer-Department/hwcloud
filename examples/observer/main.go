// Observer example: demonstrates RunObserver for stage-level observability.
//
//	go run ./examples/observer/
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

	observer := &stageObserver{}

	cfg := agent.New("calculator",
		agent.WithModel(model),
		agent.WithSystemPrompts("You are a precise calculator. Use the calculator tool for arithmetic. Answer concisely."),
	)
	deps := kernel.Deps{
		Tools:    []hwcloud.Tool{&calculatorTool{}},
		Observer: observer,
	}

	session := hwcloud.Session{
		ID:     "observer-session-1",
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
}

// ── StageObserver ──

type stageObserver struct{}

func (o *stageObserver) ObserveStage(ctx context.Context, event hwcloud.StageEvent) {
	switch event.Phase {
	case "enter":
		fmt.Printf("[%s] → entering\n", event.Name)
	case "leave":
		status := "ok"
		if event.Err != nil {
			status = event.Err.Error()
		}
		fmt.Printf("[%s] ← leaving (%v) %s\n", event.Name, event.Duration.Round(time.Microsecond), status)
	}
}

// ── Calculator Tool ──

type calculatorTool struct{}

func (t *calculatorTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "calculator",
		Description: "Evaluate a mathematical expression like '12+34' or '100/3'.",
		Parameters:  hwcloud.SchemaOf[CalcParams](),
	}
}

func (t *calculatorTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[CalcParams](args)
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
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

type CalcParams struct {
	Expression string `json:"expression" jsonschema:"description=The math expression to evaluate"`
}
