// Skill example: demonstrates automatic skill discovery and on-demand loading.
//
// Environment variables:
//
//	HWCLOUD_BASE_URL   — API base URL
//	HWCLOUD_MODEL      — model ID
//	HWCLOUD_API_KEY    — API key
//
//	go run ./examples/skill/
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/model/openai"
	"github.com/Cloud-Developer-Department/hwcloud/provider/skill"
	"github.com/Cloud-Developer-Department/hwcloud/skill/fs"
)

func main() {
	apiKey := os.Getenv("HWCLOUD_API_KEY")
	modelID := os.Getenv("HWCLOUD_MODEL")
	baseURL := os.Getenv("HWCLOUD_BASE_URL")

	model := openai.New(apiKey, modelID, baseURL).
		WithContextWindow(128_000)

	// Skill provider: skills directory is next to the binary
	skillRoot, _ := filepath.Abs("examples/skill/skills")
	loader := skill.NewFSBridge(fs.New(skillRoot))

	cfg := agent.New("skill-demo",
		agent.WithModel(model),
		agent.WithSystemPrompts("You are a helpful assistant. Skills are available for loading — use load_skill to load one when you need detailed instructions."),
	)

	session := hwcloud.Session{
		ID:     "skill-demo-1",
		UserID: "user-1",

		ModelID:   modelID,
		CreatedAt: time.Now(),
	}

	ctx := context.Background()
	result, err := kernel.New(cfg, kernel.Deps{SkillProvider: loader}).Run(ctx, session, hwcloud.UserMessage("请加载 example-skill 并按它的要求执行"))
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
		} else if msg.Role == hwcloud.RoleSystem {
			fmt.Printf("[%d] %s: %s\n", i, msg.Role, truncate(msg.Content, 120))
		} else {
			fmt.Printf("[%d] %s: %s\n", i, msg.Role, truncate(msg.Content, 200))
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
