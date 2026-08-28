package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/acp"
	openacpsdk "github.com/Cloud-Developer-Department/hwcloud/acp/sdk"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/keyring"
	"github.com/Cloud-Developer-Department/hwcloud/sandbox/native"
	"github.com/Cloud-Developer-Department/hwcloud/summarizer"
	"github.com/Cloud-Developer-Department/hwcloud/version"

	wasm "github.com/Cloud-Developer-Department/hwcloud/plugin/agent/wasm"
	"github.com/Cloud-Developer-Department/hwcloud/plugin/wasmhost"
	"github.com/Cloud-Developer-Department/hwcloud/scheduler"

	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
)

// RunACP starts the agent in ACP mode over stdio.
//
// Lifecycle:
//  1. Open memory + session store (SQLite).
//  2. Build models from config.
//  3. Create sandbox + standard tools.
//  4. Wire summarizer for long-conversation compression.
//  5. Construct the agent.
//  6. Wrap in AgentServer, launch ACP protocol mux on stdin/stdout.
func RunACP(ctx context.Context, cfg *config.Config, caps config.Capabilities) error {
	ms, knowledge, sessionStore, cleanup, err := buildMemory(cfg.Embedding, caps.OnEmbedder())
	if err != nil {
		return err
	}
	defer cleanup()

	_, modelInfos := buildModels(cfg.Provider)
	if len(modelInfos) == 0 {
		slog.Warn("no models configured, ACP server will start but prompt turns will fail")
	}

	modelMap := make(map[string]hwcloud.Model, len(modelInfos))
	for _, mi := range modelInfos {
		key := mi.ID
		if mi.Provider != "" {
			key = mi.Provider + "/" + mi.ID
		}
		modelMap[key] = mi.Model
	}

	// Summarizer and Memory are enabled by default; allow --summarizer=off
	// and --memory=off to disable them.
	var firstM hwcloud.Model
	if len(modelInfos) > 0 {
		firstM = modelInfos[0].Model
	}

	// Tools and sandbox are created once per session (buildRuntimeForSession)
	// scoped to the session's cwd. Agent configuration is pure (model,
	// prompts, limits, guards, skills); runtime capabilities live in
	// kernel.Deps.
	opts := []agent.Option{
		agent.WithSystemPrompts(resolveProfiles("")...),
		agent.WithMaxTurns(100),
	}
	opts, skillProvider := buildOpts(opts, caps, firstM)
	agentCfg := agent.New(version.Name, opts...)

	tracer, telemetryShutdown, err := setupTelemetry(ctx, *cfg)
	if err != nil {
		return fmt.Errorf("telemetry init: %w", err)
	}
	defer telemetryShutdown()

	deps := buildRuntimeDeps(caps, cfg.Sensitive, tracer)
	deps.SkillProvider = skillProvider
	// Pass nil Mem when --memory=off so the AgentServer skips history
	// replay and memory cleanup (all s.Mem uses are nil-guarded). The
	// sessionStore (session metadata) is separate and unaffected.
	if caps.OnMemory() {
		deps.SessionStore = ms
		deps.Compressor = ms
		deps.MemoryProvider = knowledge
	}

	var sumz *summarizer.Compressor
	if caps.OnMemory() && caps.OnSummarizer() && firstM != nil {
		sumz = summarizer.New(firstM).WithMaxTokens(agentCfg.MaxCompressedTokens)
		ms.WithSummarizer(sumz)
	}

	// Plugin manager — loads agent:tools and agent:observers plugins.
	// Discover before constructing the server so a plugin observer can be
	// merged into deps.Observer before the AgentServer snapshots it.
	var pluginMgr *wasm.Manager
	pluginDir := resolvePluginsDir()
	sch := scheduler.New()
	go sch.Run(ctx)
	mgr := wasm.NewManager(pluginDir).
		WithHostAPI(wasmhost.NewHostAPI(keyring.NewKeyring())).
		WithScheduler(sch)
	if err := mgr.Discover(ctx); err != nil {
		slog.Warn("plugin discover failed", "error", err)
	} else {
		pluginMgr = mgr
		if obs := mgr.Observer(); obs != nil {
			if deps.Observer != nil {
				deps.Observer = hwcloud.MultiObserver(deps.Observer, obs)
			} else {
				deps.Observer = obs
			}
			slog.Info("plugin observer wired", "source", "wasm")
		}
	}

	if err := applyContextProviders(cfg, &deps); err != nil {
		return err
	}
	// The extractor captures the MemoryProvider it writes to — build it
	// AFTER applyContextProviders so the effective provider is used.
	// Building it earlier would fork writes to the local sqlite store
	// while Recall reads the OpenViking index (silent knowledge loss).
	var extractor *ctxpkg.AsyncExtractor
	if caps.OnMemory() && firstM != nil && deps.MemoryProvider != nil {
		extractor = ctxpkg.NewAsyncExtractor(ctxpkg.NewLLMExtractor(firstM, deps.MemoryProvider))
		deps.Extractor = extractor
	}
	srv := acp.NewAgentServer(agentCfg, deps, sessionStore, modelMap)
	srv.AgentName = version.Name
	srv.AgentVersion = version.Version
	srv.MCPEnabled = caps.OnMCP()
	srv.DefaultMode = cfg.DefaultMode
	srv.PluginMgr = pluginMgr
	srv.Summarizer = sumz
	srv.Extractor = extractor
	srv.ProfileResolver = func(cwd string) []string {
		return resolveProfiles(cwd)
	}

	// Register model configs for runtime_set_model_config.
	for _, mi := range modelInfos {
		key := mi.ID
		if mi.Provider != "" {
			key = mi.Provider + "/" + mi.ID
		}
		srv.RegisterModel(key, mi.Provider, mi.ID, mi.APIKey, mi.BaseURL, acp.ModelPricing{
			MaxOutputTokens:        mi.MaxOutputTokens,
			InputCostPerToken:      mi.InputCostPerToken,
			InputCacheCostPerToken: mi.InputCacheCostPerToken,
			OutputCostPerToken:     mi.OutputCostPerToken,
		})
	}

	// settings "model" ("<provider>/<modelID>") wins as the default;
	// fall back to the first registered model.
	if cfg.Model != "" {
		if !srv.SetDefaultModelID(cfg.Model) {
			slog.Warn("settings model not in provider list, using first registered", "model", cfg.Model)
		}
	}

	policy := sandboxPolicy(cfg.Sandbox)
	baseToolList := []string{"shell", "read", "write", "ls", "grep", "websearch", "webfetch"}
	if caps.OnBrowser() {
		baseToolList = append(baseToolList, "browser")
	}
	if caps.OnOffice() {
		baseToolList = append(baseToolList, "office")
	}
	srv.ToolFactory = func(cwd string) []hwcloud.Tool {
		sb, err := native.NewWithPolicy(cwd, policy)
		if err != nil {
			slog.Warn("tool factory: sandbox creation failed; execution tools disabled", "cwd", cwd, "error", err)
			return nil
		}
		return buildTools(sb, cwd, baseToolList)
	}
	server := openacpsdk.NewServer(version.Name, version.Version, srv)
	server.SetLogger(slog.Default())

	// Channel agent: clone the template and inject a default Model + Tools
	// so the IM bot can run standalone (the ACP path resolves the model per
	// session, but channels call kernel.New(...).RunStream directly).
	channelCfg := agentCfg.Clone()
	for _, mi := range modelInfos {
		if mi.Model != nil {
			channelCfg.Model = mi.Model
			break
		}
	}
	channelDeps := deps
	cwd, _ := os.Getwd()
	if sb, err := native.NewWithPolicy(cwd, policy); err == nil {
		channelDeps.Tools = buildTools(sb, cwd, baseToolList)
	}

	if _, _, _, err := RunChannels(ChannelEnv{
		Ctx:         ctx,
		Cfg:         channelCfg,
		Deps:        channelDeps,
		DefaultMode: cfg.DefaultMode,
		WorkDir:     cwd,
		MetaStore:   sessionStore,
	}, cfg.Channels); err != nil {
		slog.Warn("channel error", "error", err)
	}

	slog.Info("ACP server starting on stdio")
	return server.Run(ctx)
}
