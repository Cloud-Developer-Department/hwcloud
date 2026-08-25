// iac-server is a cloud IaC MCP server. It exposes the deployment tools over
// MCP so any MCP client (Claude Code, opencode, Cursor, hwcloud) can
// plan, update, estimate cost, apply, troubleshoot, and destroy cloud
// infrastructure, and query existing cloud resources/bills.
//
// Transport is mutually exclusive:
//   - default: stdio (for IDE/agent runtime as parent process)
//   - --port / IAC_PORT: HTTP-only on 127.0.0.1:<port> (for sandbox/container/
//     systemd where stdin is not connected)
//
// Configuration is via environment variables:
//
//	CLOUD              cloud provider: "huaweicloud" (default), "aliyun"
//	IAC_API_KEY        server-side LLM API key
//	IAC_BASE_URL       server-side LLM base URL (OpenAI-compatible)
//	IAC_MODEL          server-side LLM model ID
//	IAC_HOME           iac-server home (default: ~/.<version.Name>/mcp/iac-server)
//	                   skills + deployments live under $IAC_HOME/<cloud>/
//	IAC_DRY_RUN        "true" = simulate, don't call terraform binary
//	IAC_PORT           HTTP listen port — switches to HTTP-only transport
//	TF_BINARY_MIRRORS  comma-separated terraform binary download mirror URLs
//	TF_PROVIDER_MIRRORS comma-separated provider mirror URLs or local paths
//
// Cloud credentials are read from the environment by the selected provider
// (e.g. HW_ACCESS_KEY, HW_SECRET_KEY, HW_REGION, HW_SECURITY_TOKEN for
// huaweicloud). The http_request tool uses AK/SK for SDK-HMAC-SHA256 signing
// — credentials never enter the LLM context.
//
// Skills (deployment guide + references, pricing guide, troubleshoot guide)
// are embedded at compile time (go:embed) and extracted to disk at startup.
// The server-side LLM gets the relevant SKILL.md injected into its system
// prompt and browses reference files with standard read/grep/ls tools.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/mcp/iac-server/agent"
	iacmcp "github.com/Cloud-Developer-Department/hwcloud/cmd/mcp/iac-server/mcp"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/mcp/iac-server/provider"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/mcp/iac-server/provider/aliyun"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/mcp/iac-server/provider/huaweicloud"
	"github.com/Cloud-Developer-Department/hwcloud/iac"
	"github.com/Cloud-Developer-Department/hwcloud/mcp"
	"github.com/Cloud-Developer-Department/hwcloud/model/openai"
	memorysqlite "github.com/Cloud-Developer-Department/hwcloud/provider/memory/sqlite"
	"github.com/Cloud-Developer-Department/hwcloud/provider/skill"
	sessionsqlite "github.com/Cloud-Developer-Department/hwcloud/session/sqlite"
	skillfs "github.com/Cloud-Developer-Department/hwcloud/skill/fs"
	"github.com/Cloud-Developer-Department/hwcloud/summarizer"
	"github.com/Cloud-Developer-Department/hwcloud/version"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ── HTTP port (optional, switches to HTTP-only transport) ──
	// --port or IAC_PORT switches from stdio to HTTP-only on 127.0.0.1:<port>.
	// Neither given = pure stdio (backward compatible).
	fs := flag.NewFlagSet("iac-server", flag.ContinueOnError)
	portFlag := fs.Int("port", 0, "HTTP listen port (switches to HTTP-only transport; stdio is not started)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		fatal(err)
	}
	// portFlagSet is true only if --port was explicitly passed on the
	// command line. fs.Visit visits exactly the flags that were set, so it
	// distinguishes "not given" (→ stdio) from "given as 0" (→ invalid).
	portFlagSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portFlagSet = true
		}
	})
	httpPort := 0
	if portFlagSet {
		if *portFlag <= 0 {
			fatal(fmt.Errorf("--port=%d is not a valid HTTP listen port (omit --port for stdio)", *portFlag))
		}
		httpPort = *portFlag
	}
	if envPort := os.Getenv("IAC_PORT"); envPort != "" && httpPort == 0 {
		p, err := strconv.Atoi(envPort)
		if err != nil {
			fatal(fmt.Errorf("IAC_PORT %q is not a valid port number", envPort))
		}
		if p <= 0 {
			fatal(fmt.Errorf("IAC_PORT %q is not a valid HTTP listen port (unset IAC_PORT for stdio)", envPort))
		}
		httpPort = p
	}

	// ── Logging ──
	// Write logs to a file (stderr is captured by the MCP client and only
	// surfaced on connection failure, so it's not reliable for debugging).
	iacHome := os.Getenv("IAC_HOME")
	if iacHome == "" {
		home, _ := os.UserHomeDir()
		iacHome = filepath.Join(home, version.ConfigDirName(), "mcp", "iac-server")
	}
	// Create the home directory BEFORE opening the log file — O_CREATE does
	// not create parent directories, and a fresh machine has no ~/.<version.Name>
	// tree yet (this used to crash first-run with ENOENT).
	if err := os.MkdirAll(iacHome, 0o755); err != nil {
		fatal(fmt.Errorf("create iac home: %w", err))
	}
	logPath := filepath.Join(iacHome, "iac-server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fatal(fmt.Errorf("open log file: %w", err))
	}
	defer func() { _ = logFile.Close() }()
	slog.SetDefault(slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// ── Select cloud provider ──
	cloud, err := selectProvider(os.Getenv("CLOUD"))
	if err != nil {
		fatal(err)
	}
	slog.Info("selected cloud provider", "cloud", cloud.Name())

	// ── Server-side LLM ──
	apiKey := os.Getenv("IAC_API_KEY")
	modelID := os.Getenv("IAC_MODEL")
	baseURL := os.Getenv("IAC_BASE_URL")
	if apiKey == "" {
		fatal(fmt.Errorf("IAC_API_KEY is required"))
	}
	if modelID == "" {
		fatal(fmt.Errorf("IAC_MODEL is required"))
	}
	model := openai.New(apiKey, modelID, baseURL)
	slog.Info("server LLM configured", "model", modelID, "base_url", baseURL)

	// ── Extract embedded skills to disk ──
	// Skills are embedded via go:embed but the standard skill loader and
	// read/grep/ls tools operate on the OS filesystem. Extract on every
	// startup, overwriting existing files so the disk copy always matches
	// the embedded version.
	cloudHome := filepath.Join(iacHome, cloud.Name())
	skillsDir := filepath.Join(cloudHome, "skills")
	if err := provider.ExtractSkills(cloud.Skills(), skillsDir); err != nil {
		fatal(fmt.Errorf("extract skills: %w", err))
	}
	slog.Info("skills directory", "path", skillsDir)

	// ── Skill provider ──
	loader := skill.NewFSBridge(skillfs.New(skillsDir))

	// ── Deployments ──
	// Each cloud gets its own subtree: $IAC_HOME/<cloud>/d-NNN/.
	deploymentsDir := filepath.Join(cloudHome, "deployments")
	if err := os.MkdirAll(deploymentsDir, 0755); err != nil {
		fatal(fmt.Errorf("create deployments dir: %w", err))
	}
	slog.Info("deployments directory", "path", deploymentsDir)

	// ── Memory ──
	// SQLite-backed conversation memory scoped by deployment_id. This lets
	// estimate_cost see plan_deployment's reasoning, troubleshoot see prior
	// attempts, etc. FTS5 gives fast full-text search across history.
	memoryPath := filepath.Join(cloudHome, "memory.db")
	ms, err := sessionsqlite.NewMessageStore(memoryPath)
	if err != nil {
		fatal(fmt.Errorf("create memory: %w", err))
	}
	defer func() { _ = ms.Close() }()
	knowledge, err := memorysqlite.New(memoryPath)
	if err != nil {
		fatal(fmt.Errorf("create knowledge store: %w", err))
	}
	defer func() { _ = knowledge.Close() }()
	// Enable conversation compaction: long deployment sessions (specify,
	// troubleshoot, ...) are summarized instead of hard-truncated when they
	// outgrow the context window. Same pattern as the CLI.
	ms.WithSummarizer(summarizer.New(model).WithMaxTokens(8192))
	slog.Info("memory database", "path", memoryPath)

	dryRun := os.Getenv("IAC_DRY_RUN") == "true"

	// ── Terraform mirrors (for networks with restricted access) ──
	binaryMirrors := splitCSV(os.Getenv("TF_BINARY_MIRRORS"))
	// The HuaweiCloud provider mirror is the default so init works without
	// user configuration; user-supplied mirrors are appended and tried after
	// it (terraform falls through mirror entries in order).
	providerMirrors := []string{}
	if cloud.Name() == "huaweicloud" {
		providerMirrors = append(providerMirrors, huaweicloud.DefaultProviderMirror)
	}
	providerMirrors = append(providerMirrors, splitCSV(os.Getenv("TF_PROVIDER_MIRRORS"))...)
	if len(binaryMirrors) > 0 || len(providerMirrors) > 0 {
		slog.Info("terraform mirrors configured",
			"binary_mirrors", binaryMirrors, "provider_mirrors", providerMirrors)
	}

	// ── Shared provider plugin cache ──
	// Providers are downloaded once into <iacHome>/plugins and reused by
	// every deployment (TF_PLUGIN_CACHE_DIR), instead of re-downloading
	// into each workspace.
	pluginCacheDir := filepath.Join(iacHome, "plugins")
	if err := os.MkdirAll(pluginCacheDir, 0o755); err != nil {
		fatal(fmt.Errorf("create plugin cache dir: %w", err))
	}
	slog.Info("terraform plugin cache", "path", pluginCacheDir)

	// ── Assemble planner + tools ──
	// Verify the cloud provides every agent role prompt — a missing role
	// silently degrades that agent to contract-only guidance, so fail fast.
	agentPrompts := cloud.Agents()
	for _, role := range provider.AllRoles {
		if cfg, ok := agentPrompts[role]; !ok || cfg.Prompt == "" {
			fatal(fmt.Errorf("cloud %s is missing agent prompt for role %q — implement Agents()", cloud.Name(), role))
		}
	}
	planner := agent.New(model, cloud, loader, ms, knowledge, cloudHome, deploymentsDir, dryRun, binaryMirrors, providerMirrors, pluginCacheDir)

	// ── Prewarm terraform provider ──
	// Download the cloud provider into the shared plugin cache and pin the
	// lock file BEFORE serving requests, so the first deployment init is a
	// cache hit instead of a full download. Failure degrades to a warning —
	// the first generate init falls back to downloading on demand.
	lockPath := filepath.Join(cloudHome, "terraform.lock.hcl")
	prewarmCfg := iac.Config{
		Env:             cloud.Env(),
		DryRun:          dryRun,
		BinaryMirrors:   binaryMirrors,
		ProviderMirrors: providerMirrors,
		PluginCacheDir:  pluginCacheDir,
	}
	// Prewarm runs in the background so startup (and the MCP initialize
	// handshake) is not blocked — first deploy init downloads on demand
	// if the prewarm has not finished yet.
	go func() {
		prewarmStart := time.Now()
		if err := iac.PrewarmProviderCache(ctx, prewarmCfg, cloud.ProviderSource(), lockPath); err != nil {
			slog.Warn("terraform provider prewarm failed (first deploy init will download on demand)", "error", err)
		} else {
			slog.Info("terraform provider prewarmed", "source", cloud.ProviderSource(), "lock", lockPath, "elapsed", time.Since(prewarmStart).Round(time.Millisecond))
		}
	}()
	tools := iacmcp.NewTools(iacmcp.Config{
		Planner:         planner,
		Cloud:           cloud,
		DeploymentsDir:  deploymentsDir,
		DryRun:          dryRun,
		BinaryMirrors:   binaryMirrors,
		ProviderMirrors: providerMirrors,
	})
	slog.Info("registered tools", "count", len(tools))

	mcpServerName := cloud.Name() + "-iac-server"
	// ── Start MCP server ──
	server := mcp.NewServer(mcpServerName, "0.0.1", &mcp.ServerOptions{
		Logger: slog.Default(),
	})
	if err := server.AddTools(tools); err != nil {
		fatal(err)
	}

	// ── Transport: HTTP and stdio are mutually exclusive ──
	// --port > 0: HTTP-only (for sandbox/container/systemd where stdin is
	// not connected and stdio would immediately EOF, killing the process).
	// --port = 0: stdio-only (for IDE/agent runtime as parent process).
	if httpPort > 0 {
		addr := fmt.Sprintf("127.0.0.1:%d", httpPort)
		handler := mcpsdk.NewStreamableHTTPHandler(
			func(r *http.Request) *mcpsdk.Server { return server.Inner() },
			&mcpsdk.StreamableHTTPOptions{
				DisableLocalhostProtection: true,
			},
		)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			fatal(fmt.Errorf("http listen %s: %w", addr, err))
		}
		slog.Info("starting HTTP transport", "addr", addr)
		srv := &http.Server{Handler: handler}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fatal(fmt.Errorf("http serve: %w", err))
			}
		}()
		<-ctx.Done()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fatal(fmt.Errorf("http shutdown: %w", err))
		}
	} else {
		slog.Info("starting iac-server on stdio")
		if err := server.Run(ctx, &mcpsdk.StdioTransport{}); err != nil {
			fatal(err)
		}
	}
}

// selectProvider chooses a CloudProvider by name.
func selectProvider(name string) (provider.CloudProvider, error) {
	switch name {
	case "", "huaweicloud":
		return huaweicloud.New(os.Getenv("HW_REGION")), nil
	case "aliyun":
		return aliyun.New(os.Getenv("ALIYUN_REGION")), nil
	default:
		return nil, fmt.Errorf("unknown cloud provider: %s (supported: huaweicloud, aliyun)", name)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "iac-server: %v\n", err)
	os.Exit(1)
}

// splitCSV splits a comma-separated string into a slice, trimming whitespace
// and dropping empty entries.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
