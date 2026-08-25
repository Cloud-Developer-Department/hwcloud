package openviking

import (
	"context"

	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
)

// Default recall quotas — match the OpenViking server's DEFAULT_QUOTAS
// (openviking/retrieve/type_quota_recall.py). Without quotas the recall
// endpoint can return empty results.
var defaultRecallQuotas = map[string]int{
	"events":      10,
	"entities":    10,
	"preferences": 3,
	"experiences": 0,
}

const (
	defaultRecallMaxChars = 6500
	defaultRecallMinScore = 0.1
)

// RecallConfig is the per-provider recall tuning. Zero values fall back
// to the defaults above. Mirrors cmd/cli/config.RecallConfig but lives
// here so the provider is usable without the CLI config package.
type RecallConfig struct {
	Quotas   map[string]int
	MaxChars int
	MinScore float64
}

// Memory implements context.MemoryProvider backed by OpenViking's
// type-quota recall endpoint (POST /api/v1/search/recall).
type Memory struct {
	client    *Client
	recallCfg RecallConfig
}

// NewMemory creates the memory provider with default recall quotas.
func NewMemory(client *Client) *Memory {
	return &Memory{
		client:    client,
		recallCfg: RecallConfig{Quotas: cloneQuotas(defaultRecallQuotas), MaxChars: defaultRecallMaxChars, MinScore: defaultRecallMinScore},
	}
}

// NewMemoryWithRecall creates the memory provider with a custom recall
// config. Zero-value fields fall back to the defaults.
func NewMemoryWithRecall(client *Client, cfg RecallConfig) *Memory {
	m := NewMemory(client)
	if cfg.MaxChars > 0 {
		m.recallCfg.MaxChars = cfg.MaxChars
	}
	if cfg.MinScore > 0 {
		m.recallCfg.MinScore = cfg.MinScore
	}
	if cfg.Quotas != nil {
		m.recallCfg.Quotas = cloneQuotas(cfg.Quotas)
	}
	return m
}

// RecallConfig returns the provider's effective recall configuration.
// The returned map is a copy so callers cannot mutate the live config.
func (m *Memory) RecallConfig() RecallConfig {
	return RecallConfig{
		Quotas:   cloneQuotas(m.recallCfg.Quotas),
		MaxChars: m.recallCfg.MaxChars,
		MinScore: m.recallCfg.MinScore,
	}
}

func cloneQuotas(q map[string]int) map[string]int {
	out := make(map[string]int, len(q))
	for k, v := range q {
		out[k] = v
	}
	return out
}

// Recall implements context.MemoryProvider. The limit parameter is
// ignored — quotas control per-type limits. Content falls back through
// summary → abstract → uri so a truncated-mode hit still carries text.
func (m *Memory) Recall(ctx context.Context, scope ctxpkg.ContextScope, query string, limit int) ([]ctxpkg.MemoryEntry, error) {
	res, err := m.client.Recall(ctx, query, m.recallCfg)
	if err != nil {
		return nil, err
	}
	entries := make([]ctxpkg.MemoryEntry, 0, len(res.Entries))
	for _, e := range res.Entries {
		content := e.Content
		if content == "" {
			content = e.Summary
		}
		if content == "" {
			content = e.Abstract
		}
		if content == "" {
			content = e.URI
		}
		entries = append(entries, ctxpkg.MemoryEntry{
			Kind:    ctxpkg.MemoryKind(e.Type),
			Content: content,
			Score:   e.Score,
		})
	}
	return entries, nil
}

// Store implements context.MemoryProvider. OpenViking's memory is a
// shared long-term knowledge base scoped by the server's own identity
// (account/user), not by ContextScope — the scope is not applied on the
// wire. Deployments needing per-user isolation scope the server side.
func (m *Memory) Store(ctx context.Context, _ ctxpkg.ContextScope, item ctxpkg.MemoryItem) error {
	_, err := m.client.Remember(ctx, item.Content)
	return err
}
