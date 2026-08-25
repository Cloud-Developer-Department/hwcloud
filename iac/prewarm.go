package iac

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// PrewarmProviderCache downloads the cloud's terraform provider into the
// plugin cache and persists the dependency lock file, so the first real
// deployment init is a cache hit (seconds, offline-capable) instead of a
// full download.
//
// It works by running terraform init in a throwaway workspace with a
// minimal providers.tf, then copying the generated .terraform.lock.hcl to
// lockPath. Subsequent runs are idempotent: when lockPath already exists
// it is copied into the workspace first, so init reuses the cached
// provider (terraform resolves versions from the lock file, which is
// exactly what makes TF_PLUGIN_CACHE_DIR hits work).
//
// The lock file pins one provider version for all deployments — the
// generated .tf files must NOT specify a provider version (the lock
// governs it), otherwise init fails with a version conflict.
func PrewarmProviderCache(ctx context.Context, cfg Config, providerSource, lockPath string) error {
	if cfg.DryRun {
		return nil // no terraform calls in dry-run
	}
	dir, err := os.MkdirTemp("", "iac-prewarm")
	if err != nil {
		return fmt.Errorf("prewarm: create temp workspace: %w", err)
	}
	defer os.RemoveAll(dir)

	providersTF := fmt.Sprintf(`terraform {
  required_providers {
    %[1]s = { source = "%[2]s" }
  }
}
`, providerName(providerSource), providerSource)
	if err := os.WriteFile(filepath.Join(dir, "providers.tf"), []byte(providersTF), 0644); err != nil {
		return fmt.Errorf("prewarm: write providers.tf: %w", err)
	}

	// Idempotent re-prewarm: reuse the pinned lock so init hits the cache
	// instead of resolving a (possibly newer) latest version.
	if lock, err := os.ReadFile(lockPath); err == nil {
		if err := os.WriteFile(filepath.Join(dir, ".terraform.lock.hcl"), lock, 0644); err != nil {
			return fmt.Errorf("prewarm: copy lock: %w", err)
		}
	}

	client, err := NewClient(ctx, dir, cfg)
	if err != nil {
		return fmt.Errorf("prewarm: %w", err)
	}
	if err := client.Init(ctx); err != nil {
		return fmt.Errorf("prewarm: terraform init: %w", err)
	}

	// Persist the lock so every deployment init pins the same version and
	// hits the plugin cache.
	lock, err := os.ReadFile(filepath.Join(dir, ".terraform.lock.hcl"))
	if err != nil {
		return fmt.Errorf("prewarm: read generated lock: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("prewarm: mkdir lock dir: %w", err)
	}
	if err := os.WriteFile(lockPath, lock, 0644); err != nil {
		return fmt.Errorf("prewarm: write lock: %w", err)
	}
	return nil
}

// providerName returns the local name part of a provider source
// ("huaweicloud/huaweicloud" -> "huaweicloud"), used as the key in
// required_providers blocks.
func providerName(source string) string {
	for i := len(source) - 1; i >= 0; i-- {
		if source[i] == '/' {
			return source[i+1:]
		}
	}
	return source
}
