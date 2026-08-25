package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
	openaiembed "github.com/Cloud-Developer-Department/hwcloud/embedder/openai"
	"github.com/Cloud-Developer-Department/hwcloud/guard/llm"
	"go.opentelemetry.io/otel/trace"
	otelhooks "github.com/Cloud-Developer-Department/hwcloud/hooks/otel"
	redacthook "github.com/Cloud-Developer-Department/hwcloud/hooks/redact"
	sloghooks "github.com/Cloud-Developer-Department/hwcloud/hooks/slog"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/model/openai"
	memorysqlite "github.com/Cloud-Developer-Department/hwcloud/provider/memory/sqlite"
	openviking "github.com/Cloud-Developer-Department/hwcloud/provider/openviking"
	"github.com/Cloud-Developer-Department/hwcloud/provider/skill"
	"github.com/Cloud-Developer-Department/hwcloud/sandbox/native"
	"github.com/Cloud-Developer-Department/hwcloud/session"
	sessionsqlite "github.com/Cloud-Developer-Department/hwcloud/session/sqlite"
	"github.com/Cloud-Developer-Department/hwcloud/skill/fs"
	builtinskills "github.com/Cloud-Developer-Department/hwcloud/skills"
	opentool "github.com/Cloud-Developer-Department/hwcloud/tool"
	"github.com/Cloud-Developer-Department/hwcloud/version"

	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
)

// ── Shared agent setup ──

// buildMemory opens the SQLite session store and knowledge provider under
// config.Dir()/memory/. The conversation (SessionStore/Compressor) and the
// knowledge provider (MemoryProvider) share one database file via separate
// connections (WAL); the metadata Store shares the conversation connection.
func buildMemory(emb config.EmbeddingConfig, embedder bool) (*sessionsqlite.MessageStore, ctxpkg.MemoryProvider, session.Store, func(), error) {
	memDir := filepath.Join(configDir(), "memory")
	_ = os.MkdirAll(memDir, 0755)
	path := filepath.Join(memDir, "memory.db")
	ms, err := sessionsqlite.NewMessageStore(path)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("message store: %w", err)
	}
	var knowledge ctxpkg.MemoryProvider
	if embedder {
		kmem, err := memorysqlite.New(path)
		if err != nil {
			ms.Close()
			return nil, nil, nil, nil, fmt.Errorf("knowledge store: %w", err)
		}
		if emb.Provider != "" {
			// External embedding backend (OpenAI-compatible /embeddings:
			// OpenAI, Ollama, Jina, Cohere, local proxies).
			kmem.WithEmbedder(openaiembed.New(emb.BaseURL, emb.APIKey, emb.Model))
		} else {
			// No embedding backend configured: semantic vector recall stays
			// disabled (Memory falls back to keyword search). The built-in
			// BGE embedder was removed to keep the default build cgo-free;
			// configure embedding.provider in settings to enable semantic
			// recall. WithEmbedder is intentionally NOT called — nil embedder.
		}
		knowledge = kmem
	}
	store, err := sessionsqlite.New(ms.DB())
	if err != nil {
		ms.Close()
		return nil, nil, nil, nil, fmt.Errorf("session store: %w", err)
	}
	return ms, knowledge, store, func() {
		store.Close()
		if c, ok := knowledge.(interface{ Close() error }); ok {
			c.Close()
		}
		ms.Close()
	}, nil
}

// buildModels creates OpenAI model instances from config providers.
func buildModels(providers map[string]config.ProviderConfig) ([]hwcloud.Model, []modelReg) {
	var models []hwcloud.Model
	var infos []modelReg
	for pid, p := range providers {
		for _, mc := range p.Models {
			apiKey := p.APIKey
			if apiKey == "" {
				apiKey = os.Getenv(strings.ToUpper(pid) + "_API_KEY")
			}
			m := openai.New(apiKey, mc.ID, p.BaseURL)
			// Explicit max_input_tokens overrides the built-in vendor
			// lookup (quantized/shrunk custom models may declare 1M but
			// serve 128K).
			if mc.MaxInputTokens > 0 {
				m = m.WithContextWindow(mc.MaxInputTokens)
			}
			models = append(models, m)
			infos = append(infos, modelReg{
				ID:                     mc.ID,
				Provider:               pid,
				Model:                  m,
				APIKey:                 apiKey,
				BaseURL:                p.BaseURL,
				MaxOutputTokens:        mc.MaxOutputTokens,
				InputCostPerToken:      mc.InputCostPerToken,
				InputCacheCostPerToken: mc.InputCacheCostPerToken,
				OutputCostPerToken:     mc.OutputCostPerToken,
			})
		}
	}
	return models, infos
}

type modelReg struct {
	ID                     string
	Provider               string
	Model                  hwcloud.Model
	APIKey                 string
	BaseURL                string
	MaxOutputTokens        int
	InputCostPerToken      float64
	InputCacheCostPerToken float64
	OutputCostPerToken     float64
}

func firstModel(models []hwcloud.Model) hwcloud.Model {
	for _, m := range models {
		if m != nil {
			return m
		}
	}
	return nil
}

// applyContextProviders selects the provider backend per capability.
// OpenViking is a whole-context service (one endpoint, server-managed
// memory/resource/skill indexes): a configured endpoint switches ALL
// three domains to it by default — one address is enough. context_providers
// remains as an opt-out escape hatch: an explicit "builtin" for a domain
// keeps the local backend. No endpoint = fully local, no server required.
func applyContextProviders(cfg *config.Config, deps *kernel.Deps) error {
	cp := cfg.ContextProviders
	if cfg.OpenViking.Endpoint == "" {
		return nil
	}
	client, err := openviking.NewClient(cfg.OpenViking.Endpoint, cfg.OpenViking.APIKey)
	if err != nil {
		return fmt.Errorf("openviking: %w", err)
	}
	if cp.Memory != "builtin" {
		deps.MemoryProvider = openviking.NewMemoryWithRecall(client, openviking.RecallConfig{
			Quotas:   cfg.OpenViking.Recall.Quotas,
			MaxChars: cfg.OpenViking.Recall.MaxChars,
			MinScore: cfg.OpenViking.Recall.MinScore,
		})
	}
	if cp.Skill != "builtin" {
		deps.SkillProvider = openviking.NewSkill(client, nil)
	}
	if cp.Resource != "builtin" {
		deps.ResourceProvider = openviking.NewResource(client)
	}
	return nil
}

// sandboxPolicy translates the config-layer SandboxConfig into a
// native.Policy. Empty Network is treated as "host" (matches the
// sandbox package's zero-value default), so missing config yields
// network access for the agent — required for shell tools that
// reach LLM providers, package managers, cloud CLIs, etc.
func sandboxPolicy(cfg config.SandboxConfig) native.Policy {
	return native.Policy{
		Enabled:       cfg.Enabled,
		Network:       cfg.Network,
		WritablePaths: cfg.WritablePaths,
		ReadablePaths: cfg.ReadablePaths,
	}
}

// buildTools creates the standard file/shell tool set using the sandbox.
// workDir is the workspace root; the tool list selects which tools to create.
func buildTools(sandbox *native.Sandbox, workDir string, toolList []string) []hwcloud.Tool {
	enabled := make(map[string]bool)
	for _, name := range toolList {
		enabled[name] = true
	}
	var tools []hwcloud.Tool
	if enabled["shell"] {
		tools = append(tools, opentool.NewShell(sandbox))
	}
	if enabled["read"] {
		tools = append(tools, opentool.NewReadFile(workDir))
	}
	if enabled["write"] {
		tools = append(tools, opentool.NewWriteFile(workDir))
	}
	if enabled["ls"] {
		tools = append(tools, opentool.NewListDir(workDir))
	}
	if enabled["grep"] {
		tools = append(tools, opentool.NewGrep(workDir))
	}
	if enabled["edit"] {
		tools = append(tools, opentool.NewEditFile(workDir))
	}
	if enabled["websearch"] {
		tools = append(tools, opentool.NewWebSearch())
	}
	if enabled["webfetch"] {
		tools = append(tools, opentool.NewWebFetch())
	}
	if enabled["browser"] {
		// One-shot headless tools (browser_navigate/screenshot/evaluate/click)
		// + persistent-session tools (browser_use_open/snapshot/click/type/
		// press/play_media/tabs/switch_tab/close_tab/close).
		tools = append(tools, opentool.NewBrowserTools()...)
		tools = append(tools, opentool.NewBrowserUseTools()...)
	}
	if enabled["office"] {
		// PPT tools: pptx_read (pure Go) + pptx_template_analyze/fill (pure
		// Go) + pptx_write (Node.js PptxGenJS, embedded worker bundle).
		tools = append(tools, opentool.NewOfficeTools(workDir)...)
	}
	return tools
}

// ── Static context (AGENTS.md / SOUL.md) ──

// methodologyAndRulesPrompt is the built-in default for AGENTS.md.
// It defines working methodology and behavioral rules.
const methodologyAndRulesPrompt = `# Methodology & Rules
CRITICAL: Do not present uncertain conclusions as facts.
CRITICAL: Do not include secrets or credential values in user-facing output.
CRITICAL: Any factual result that depends on the current environment, files, commands, external systems, or runtime state must be obtained through tools or explicitly confirmed by the user.
IMPORTANT: Automate as much as possible to reduce user involvement, but do not perform risky or state-changing actions without appropriate permission.
IMPORTANT: Explain important actions briefly before taking them.
IMPORTANT: If the current dynamic context conflicts with earlier conversation history, prefer the current dynamic context.
- When receiving a large or complex task, decompose it into structured steps before starting work.
- Read existing context before making changes — understand, then act.
- After each tool execution, verify the result before proceeding to the next step.
- Use recall to search conversation history for exact details — commands, file names, dates — not covered by the summary.
- When uncertain about requirements, ask clarifying questions rather than guessing.
`

// personaAndLimitsPrompt is the built-in default for SOUL.md.
// It defines personality, tone, and behavioral boundaries.
const personaAndLimitsPrompt = `# Persona & Limits
IMPORTANT: Always use the same language as the user. If the user asks in Chinese, reasoning and response in Chinese.
IMPORTANT: Help the user complete tasks by using available tools when appropriate. Do not ask the user to perform operations that you can safely perform yourself with available tools.
- Be concise and direct. Do not flatter, apologize excessively, or hedge.
- Never delete, move, or overwrite files without explicit user confirmation.
- When asked to do something impossible or unsafe, explain why and suggest alternatives.
- Respect user time — surface the most relevant information first. Avoid verbose preambles.
- Use clear, imperative language for actions; use structured formatting for complex output.
`

// systemContextPrompt is the built-in default for SYSTEM.md.
// It is a system-level prompt slot for environment-wide instructions that
// sit between persona (SOUL.md) and methodology (AGENTS.md). Override by
// placing SYSTEM.md in the profile directory.
const systemContextPrompt = `# System Instructions
CRITICAL: Do not claim completion unless the relevant work has actually been performed or verified.
IMPORTANT: Be concise, practical, and action-oriented.
IMPORTANT: Keep user-facing text focused on progress, decisions, results, and next actions.

- Prefer direct answers and concrete next actions.
- Avoid long hidden-style reasoning in user-facing text.
- Do not narrate every internal consideration.
- Summarize tool results only as much as needed to continue the task.
- If something fails, explain the failure briefly and choose the next best action.
- Avoid repeating the same status update unless new information was learned.
`

// profileDir returns the prompt content directory: config.Dir()/
// profile — a fixed subdirectory of the configuration directory, so
// HWCLOUD_CLI_CONFIG is the single root for every persistent path.
func profileDir() string {
	return filepath.Join(configDir(), "profile")
}

// resolvePluginsDir returns the agent plugin directory: config.Dir()/
// plugins — the same root as the CLI plugin default (config.DefaultPluginsDir),
// so all plugins live in one place regardless of loader.
func resolvePluginsDir() string {
	return filepath.Join(configDir(), "plugins")
}

// configDir returns the configuration directory (config.Dir), with a
// home fallback when it cannot be resolved.
func configDir() string {
	return config.Dir()
}

// resolveProfiles reads SOUL.md, SYSTEM.md, and AGENTS.md: project-level
// directory. Falls back to built-in defaults when the files are missing.
//
// cwd is the working directory to search for project-level prompts; if empty,
// os.Getwd() is used.
//
// Resolution order (per file):
//  1. $(cwd)/FILE.md
//  2. <config-dir>/profile/FILE.md
//  3. built-in default
//
// The prompts are returned in injection order: SOUL → SYSTEM → AGENTS.
func resolveProfiles(cwd string) []string {
	return []string{
		resolveProfileFile(cwd, "SOUL.md", personaAndLimitsPrompt),
		resolveProfileFile(cwd, "SYSTEM.md", systemContextPrompt),
		resolveProfileFile(cwd, "AGENTS.md", methodologyAndRulesPrompt),
	}
}

func resolveProfileFile(cwd, filename, defaultText string) string {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// 1.  Project-level: $(cwd)/FILE.md (AGENTS.md convention — the
	// project's own prompt lives with the project).
	if cwd != "" {
		p := filepath.Join(cwd, filename)
		if data, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(data))
		}
	}

	// 2.  User-level: <config-dir>/profile/FILE.md — a custom
	// directory derives from the configuration directory, so a custom
	// HWCLOUD_CLI_CONFIG relocates the prompts with it.
	p := filepath.Join(profileDir(), filename)
	if data, err := os.ReadFile(p); err == nil {
		return strings.TrimSpace(string(data))
	}

	// 3.  Built-in default
	return defaultText
}

// ── Optional capability builders ──

// openSkillProvider creates a file-system skill provider spanning the skill
// directories that exist plus the embedded built-in skills (when built with
// -tags embed). Roots are passed to fs.New in override order (user-level first,
// project-level last), so skills in a later root override same-name skills
// from an earlier root:
//
//  1. ~/.agents/skills            (user-level)
//  2. <workspace>/.agents/skills  (project-level, overrides user-level)
//  3. embedded built-in skills    (lowest priority, -tags embed only)
//
// Directories that do not exist are skipped. Returns nil only when there are
// no disk roots AND no embedded skills.
func openSkillProvider() skill.Provider {
	var roots []string
	for _, dir := range skillDirs() {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			roots = append(roots, dir)
		}
	}
	embedFS := builtinskills.BuiltinFS()
	if len(roots) == 0 && embedFS == nil {
		return nil
	}
	loader := fs.New(roots...)
	if embedFS != nil {
		loader = loader.WithEmbedFS(embedFS)
	}
	return skill.NewFSBridge(loader)
}

// skillDirs returns the skill directory candidates in override order:
// user-level first (~/.agents/skills), project-level last
// (<cwd>/.agents/skills, overrides user-level). When home equals cwd the
// two resolve to the same path and only one entry is returned.
func skillDirs() []string {
	var dirs []string
	seen := make(map[string]struct{})

	home, err := os.UserHomeDir()
	if err == nil {
		d := filepath.Join(home, ".agents", "skills")
		if _, ok := seen[d]; !ok {
			seen[d] = struct{}{}
			dirs = append(dirs, d)
		}
	}

	cwd, err := os.Getwd()
	if err == nil {
		d := filepath.Join(cwd, ".agents", "skills")
		if _, ok := seen[d]; !ok {
			seen[d] = struct{}{}
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// buildGuard creates an LLM guard using the given model as judge.
func buildGuard(model hwcloud.Model) *llm.Guard {
	return llm.New(model)
}

// buildSlogHooks creates slog-based RunHooks (the lifecycle axis).
func buildSlogHooks() hwcloud.RunHooks {
	return sloghooks.New(slog.Default())
}

// buildSlogObserver creates the observation-axis slog Observer (stage
// boundaries + decision events) backed by slog.Default(). The lifecycle-axis
// Hooks are wired separately by buildSlogHooks; both share slog.Default() so
// a single handler config (rotated file / discard / never stderr — stderr is
// the ACP control pipe) governs both axes. Returning *sloghooks.Observer
// (which implements DecisionObserver) lets the kernel's decision dispatch
// type-assert succeed, so DecisionEvents reach the slog sink in HTTP/run mode
// — the old private slogObserver implemented only ObserveStage and silently
// dropped every DecisionEvent.
func buildSlogObserver() hwcloud.RunObserver {
	return sloghooks.NewObserver(slog.Default())
}

// buildOpts appends capability-gated agent options (skills, guard) to opts
// and returns the skill provider for the runtime deps. model is used by the
// guard; it may be nil if no models are configured, in which case the guard
// is skipped regardless of caps.
func buildOpts(opts []agent.Option, caps config.Capabilities, model hwcloud.Model) ([]agent.Option, skill.Provider) {
	var sp skill.Provider
	if caps.OnSkills() {
		sp = openSkillProvider()
	}
	if caps.OnGuard() && model != nil {
		g := buildGuard(model)
		opts = append(opts, agent.WithInputGuard(g))
		opts = append(opts, agent.WithOutputGuard(g.Output()))
	}
	return opts, sp
}

// setupTelemetry initializes the OpenTelemetry TracerProvider from config.
// Returns the tracer (nil if telemetry is disabled) and a shutdown function
// the caller must defer. When cfg.Telemetry.Endpoint is empty, a no-op
// provider is returned — spans are created but never exported, so the otel
// hook can be wired unconditionally.
func setupTelemetry(ctx context.Context, cfg config.Config) (trace.Tracer, func(), error) {
	insecure := true
	if cfg.Telemetry.Insecure != nil {
		insecure = *cfg.Telemetry.Insecure
	}
	// ServiceName defaults to version.Name (the binary identity set via
	// ldflags in build.sh) so the trace's service.name matches the actual
	// binary name, not a hardcoded string.
	serviceName := strings.TrimSpace(cfg.Telemetry.ServiceName)
	if serviceName == "" {
		serviceName = version.Name
	}
	result, err := otelhooks.SetupTracer(ctx, otelhooks.Config{
		Endpoint:    cfg.Telemetry.Endpoint,
		Protocol:    cfg.Telemetry.Protocol,
		ServiceName: serviceName,
		Insecure:    insecure,
	})
	if err != nil {
		return nil, func() {}, err
	}
	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := result.Shutdown(shutdownCtx); err != nil {
			slog.Warn("telemetry shutdown failed", "error", err)
		}
	}
	return result.Tracer, shutdown, nil
}

// buildRuntimeDeps returns the always-on runtime dependencies shared by all
// modes: the RunHooks pipeline and the stage observer. Mode-specific
// capabilities (Tools, Memory, Approver) are added by the caller.
//
// sensitive carries the user-configured sensitive env-var names; it is
// honored only when caps.OnHooks() is true (redact rides the hooks pipeline).
// tracer, when non-nil, adds an OTel hook that emits agent.run and tool.<name>
// spans. Hook order is redact → otel → slog: redact first (secrets masked
// before any other hook sees the data), otel before slog (spans carry the
// redacted args).
func buildRuntimeDeps(caps config.Capabilities, sensitive config.SensitiveConfig, tracer trace.Tracer) kernel.Deps {
	hooks := []hwcloud.RunHooks{
		redacthook.NewHook(sensitive.Env),
	}
	observers := []hwcloud.RunObserver{buildSlogObserver()}
	if tracer != nil {
		hooks = append(hooks, otelhooks.New(tracer))
		observers = append(observers, otelhooks.NewObserver(tracer))
	}
	hooks = append(hooks, buildSlogHooks())
	return kernel.Deps{
		Hooks:    hwcloud.MultiHooks(hooks...),
		Observer: hwcloud.MultiObserver(observers...),
	}
}

// channelDir returns the per-channel state directory: config.Dir()/
// channel/<name> — channel locks, credentials, QR caches, and media
// live beside memory and profile, not inside either.
func channelDir(name string) string {
	return filepath.Join(configDir(), "channel", name)
}
