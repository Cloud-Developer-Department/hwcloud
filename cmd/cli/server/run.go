package server

import (
	"context"
	"fmt"
	"os"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/sandbox/native"
	"github.com/Cloud-Developer-Department/hwcloud/summarizer"
	"github.com/Cloud-Developer-Department/hwcloud/version"
)

// RunCLI runs a one-shot chat turn with streaming output to stdout.
// Same memory wiring as the servers: the conversation is persisted under a
// fresh session id (each run is its own session), and durable knowledge is
// extracted after the run and recalled across runs (user-level scope), so
// "run" participates in the knowledge closed loop.
func RunCLI(ctx context.Context, cfg *config.Config, message string) error {
	// 1. Build model from config (unexported: buildModels, firstModel)
	models, _ := buildModels(cfg.Provider)
	m := firstModel(models)
	if m == nil {
		return fmt.Errorf("no models configured. Please add a provider in %s", config.Path())
	}

	// 2. Memory + knowledge (same wiring as RunACP/RunREST).
	// Capabilities come from settings.json like every other mode — run is
	// not special-cased to defaults-on.
	caps := cfg.Capabilities
	ms, knowledge, _, cleanup, err := buildMemory(cfg.Embedding, caps.OnEmbedder())
	if err != nil {
		return err
	}
	defer cleanup()

	// 3. System prompts (unexported: resolveProfiles)
	prompts := resolveProfiles("")

	// 4. Sandbox + standard tools (unexported: sandboxPolicy, buildTools)
	workDir, _ := os.Getwd()
	policy := sandboxPolicy(cfg.Sandbox)
	sb, err := native.NewWithPolicy(workDir, policy)
	runToolList := []string{"shell", "read", "write", "ls", "grep", "websearch", "webfetch"}
	if caps.OnBrowser() {
		runToolList = append(runToolList, "browser")
	}
	if caps.OnOffice() {
		runToolList = append(runToolList, "office")
	}
	var tools []hwcloud.Tool
	if err == nil {
		tools = buildTools(sb, workDir, runToolList)
	} else {
		fmt.Fprintf(os.Stderr, "sandbox unavailable, tools disabled: %v\n", err)
	}

	// 5. Construct agent config (pure) + runtime deps.
	opts := []agent.Option{
		agent.WithModel(m),
		agent.WithSystemPrompts(prompts...),
		agent.WithMaxTurns(50),
	}
	opts, skillProvider := buildOpts(opts, caps, m)
	agentCfg := agent.New(version.Name, opts...)

	tracer, telemetryShutdown, err := setupTelemetry(ctx, *cfg)
	if err != nil {
		return fmt.Errorf("telemetry init: %w", err)
	}
	defer telemetryShutdown()

	deps := buildRuntimeDeps(caps, cfg.Sensitive, tracer)
	deps.Tools = tools
	deps.SkillProvider = skillProvider
	if caps.OnMemory() {
		deps.SessionStore = ms
		deps.Compressor = ms
		deps.MemoryProvider = knowledge
		// One shared background extractor: knowledge from this run is
		// stored and recalled by later runs (and by the servers sharing
		// this db).
		deps.Extractor = ctxpkg.NewAsyncExtractor(ctxpkg.NewLLMExtractor(m, knowledge))
	}
	if caps.OnMemory() && caps.OnSummarizer() && m != nil {
		ms.WithSummarizer(summarizer.New(m).WithMaxTokens(agentCfg.MaxCompressedTokens))
	}

	// 6. Fresh session per run (no cross-run conversation history, but
	// durable knowledge is user-level and carries across runs).
	session := hwcloud.Session{
		ID:        fmt.Sprintf("cli-%d", time.Now().UnixNano()),
		CreatedAt: time.Now(),
	}

	// 7. Run and stream events to terminal
	ch := kernel.New(agentCfg, deps).RunStream(ctx, session, hwcloud.UserMessage(message))
	for evt := range ch {
		switch evt.Type {
		case hwcloud.StreamTextDelta:
			fmt.Print(evt.Text)

		case hwcloud.StreamDone:
			fmt.Println()
			if evt.Result != nil {
				u := evt.Result.Usage
				fmt.Fprintf(os.Stderr, "─── %d prompt + %d completion = %d tokens, %d turns\n",
					u.PromptTokens, u.CompletionTokens, u.TotalTokens,
					evt.Result.TurnCount)
			}

		case hwcloud.StreamError:
			return evt.Error

		case hwcloud.StreamAborted:
			if evt.Error != nil {
				return evt.Error
			}
			return ctx.Err()
		}
	}
	return nil
}
