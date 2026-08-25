package iac

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-exec/tfexec"
)

// Config controls how a Client is created: binary management, provider
// mirrors, and execution environment.
type Config struct {
	// ── Binary ──
	// Priority: BinaryPath > Detect(PATH) > BinaryMirrors > hc-install official
	BinaryPath    string   // Use this binary directly; skip auto-install
	Version       string   // Required version, e.g. "1.9.5"; empty = latest
	BinaryMirrors []string // Binary download mirror base URLs, tried in order

	// ── Provider ──
	// Generates .terraformrc and sets TF_CLI_CONFIG_FILE.
	// URL → network_mirror, local path → filesystem_mirror.
	ProviderMirrors []string // Provider mirror URLs or local paths, tried in order

	// PluginCacheDir sets TF_PLUGIN_CACHE_DIR — providers are downloaded
	// once into this shared directory and reused by every deployment,
	// instead of re-downloading into each workspace's .terraform/.
	// Empty disables the cache.
	PluginCacheDir string

	// ── Execution ──
	Env    map[string]string // Extra env vars (cloud credentials, etc.)
	DryRun bool              // Simulate without calling terraform binary
}

// Client wraps a terraform working directory and binary.
// Each Client is bound to one workDir and is safe for sequential use
// (concurrent calls to the same Client are not synchronized).
type Client struct {
	workDir    string
	binaryPath string
	dryRun     bool
	tf         *tfexec.Terraform // lazy-init on first real call
	env        map[string]string // merged env for terraform subprocess
	tfOnce     sync.Once
	tfErr      error // captured init error from Once
}

// NewClient creates a Client bound to workDir.
// It ensures the terraform binary is available, generates provider mirror
// config if needed, and prepares the execution environment.
// The workDir is created if it does not exist.
func NewClient(ctx context.Context, workDir string, cfg Config) (*Client, error) {
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}

	// Ensure binary (skip in dry-run — we won't call it).
	binaryPath := ""
	if !cfg.DryRun {
		path, err := EnsureTerraform(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("ensure terraform: %w", err)
		}
		binaryPath = path
	}

	// Build merged env: the process environment is the base, cloud
	// credentials override it. tfexec.SetEnv replaces the subprocess env
	// rather than merging (despite its doc comment), so a cfg.Env without
	// PATH would leave terraform unable to find system commands like
	// getent (used for provider mirror host resolution).
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range cfg.Env {
		env[k] = v
	}

	// Provider mirror config.
	if len(cfg.ProviderMirrors) > 0 {
		rcPath, err := writeProviderMirrorConfig(workDir, cfg.ProviderMirrors)
		if err != nil {
			return nil, fmt.Errorf("provider mirror config: %w", err)
		}
		if rcPath != "" {
			env["TF_CLI_CONFIG_FILE"] = rcPath
		}
	}

	// Shared provider plugin cache — one download, reused by all
	// deployments. Terraform falls back to downloading when the cache
	// misses (it is a cache, not a mirror).
	if cfg.PluginCacheDir != "" {
		env["TF_PLUGIN_CACHE_DIR"] = cfg.PluginCacheDir
	}

	return &Client{
		workDir:    workDir,
		binaryPath: binaryPath,
		dryRun:     cfg.DryRun,
		env:        env,
	}, nil
}

// WorkDir returns the working directory path.
func (c *Client) WorkDir() string { return c.workDir }

// ensureTF lazily initializes the tfexec.Terraform instance.
// Safe for concurrent use; initialization happens exactly once.
func (c *Client) ensureTF() error {
	c.tfOnce.Do(func() {
		if c.binaryPath == "" {
			c.tfErr = fmt.Errorf("no terraform binary configured")
			return
		}
		tf, err := tfexec.NewTerraform(c.workDir, c.binaryPath)
		if err != nil {
			c.tfErr = fmt.Errorf("init tfexec: %w", err)
			return
		}
		// tfexec.SetEnv merges these with os.Environ(); our values take
		// precedence over process-level env.
		tf.SetEnv(c.env)
		c.tf = tf
	})
	return c.tfErr
}
