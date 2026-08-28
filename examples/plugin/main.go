// Plugin example: demonstrates the WASM plugin system.
//
// Build plugins first (requires Rust + wasm32-unknown-unknown target):
//
//	make -C examples/plugin
//
// Then run:
//
//	go run ./examples/plugin/
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
	"github.com/Cloud-Developer-Department/hwcloud/plugin/agent/wasm"
	"github.com/Cloud-Developer-Department/hwcloud/plugin/wasmhost"
	"github.com/Cloud-Developer-Department/hwcloud/scheduler"
)

type stdLogger struct{}

func (l *stdLogger) Info(msg string)  { fmt.Println("[plugin]", msg) }
func (l *stdLogger) Warn(msg string)  { fmt.Println("[plugin] WARN:", msg) }
func (l *stdLogger) Error(msg string) { fmt.Println("[plugin] ERROR:", msg) }

func main() {
	apiKey := os.Getenv("HWCLOUD_API_KEY")
	modelID := os.Getenv("HWCLOUD_MODEL")
	baseURL := os.Getenv("HWCLOUD_BASE_URL")

	model := openai.New(apiKey, modelID, baseURL).
		WithContextWindow(128_000)

	// Plugin manager with host API so plugins can use log_* and keyring_*.
	// Scheduled jobs declared by plugins fire on a process-local scheduler.
	hostAPI := &wasmhost.HostAPI{Logger: &stdLogger{}}
	sch := scheduler.New()
	go sch.Run(context.Background())
	mgr := wasm.NewManager("./plugins").WithHostAPI(hostAPI).WithScheduler(sch)
	if err := mgr.Discover(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "Plugin discover error: %v\n", err)
		os.Exit(1)
	}
	defer mgr.Close()

	// Report any scheduled jobs the plugins declared (the scheduler fires
	// them in the background; this is just the registration view).
	if jobs := sch.Jobs(); len(jobs) > 0 {
		for _, j := range jobs {
			fmt.Printf("Scheduled job %q: %s (next %s)\n", j.ID, j.Cron, j.NextRun.Format(time.RFC3339))
		}
	}

	// Loaded tools (from .wasm plugins + built-in)
	tools := []hwcloud.Tool{&calculatorTool{}}
	tools = append(tools, mgr.Tools()...)

	fmt.Printf("Loaded %d tool plugin(s)\n", len(mgr.Tools()))

	// Observer: wrap stage plugin with a logger so we can see it fire.
	var observer hwcloud.RunObserver
	if raw := mgr.Observer(); raw != nil {
		fmt.Println("Stage plugins loaded")
		observer = raw
	} else {
		fmt.Println("No stage plugins")
	}

	cfg := agent.New("assistant",
		agent.WithModel(model),
		agent.WithSystemPrompts("You are a precise assistant. Use echo for testing tool calls, calculator for math. Be concise."),
	)
	rt := kernel.New(cfg, kernel.Deps{
		Tools:    tools,
		Observer: observer,
	})

	ctx := context.Background()
	session := hwcloud.Session{
		ID:     "plugin-session-1",
		UserID: "user-1",

		ModelID:   modelID,
		CreatedAt: time.Now(),
	}

	fmt.Println("\n=== Running agent ===")

	// Inject AgentRuntime so runtime_* host APIs work in observers.
	wrt := wasm.BuildAgentRuntime(rt, &session, nil, nil)
	ctx = wasmhost.WithAgentRuntime(ctx, wrt)
	result, err := rt.Run(ctx, session, hwcloud.UserMessage("Use the echo tool to echo 'hello plugin', then calculate 15+27"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Output: %s\n", result.FinalOutput)
	fmt.Printf("Turns: %d, Tokens: %d\n", result.TurnCount, result.Usage.TotalTokens)
}

// Calculator Tool (built-in, always available)

type calculatorTool struct{}

func (t *calculatorTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "calculator",
		Description: "Evaluate a math expression like '15+27' or '100/3'.",
		Parameters:  hwcloud.SchemaOf[CalcParams](),
	}
}

func (t *calculatorTool) Execute(_ context.Context, args json.RawMessage) *hwcloud.ToolResult {
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
