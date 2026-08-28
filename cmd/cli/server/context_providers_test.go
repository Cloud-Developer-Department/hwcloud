package server

import (
	"testing"

	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	openviking "github.com/Cloud-Developer-Department/hwcloud/provider/openviking"
)

// TestApplyContextProviders_EndpointSwitchesAll: a configured OpenViking
// endpoint switches memory/skill/resource to OpenViking by default — one
// address is enough.
func TestApplyContextProviders_EndpointSwitchesAll(t *testing.T) {
	cfg := &config.Config{
		OpenViking: config.OpenVikingConfig{Endpoint: "http://127.0.0.1:1933"},
	}
	var deps kernel.Deps
	if err := applyContextProviders(cfg, &deps); err != nil {
		t.Fatal(err)
	}
	if deps.MemoryProvider == nil {
		t.Error("memory provider not switched to openviking")
	}
	if deps.SkillProvider == nil {
		t.Error("skill provider not switched to openviking")
	}
	if deps.ResourceProvider == nil {
		t.Error("resource provider not switched to openviking")
	}
	// A bare endpoint (no explicit recall config) still gets the
	// provider defaults — the provider is the single source of truth.
	if mem, ok := deps.MemoryProvider.(*openviking.Memory); ok {
		rc := mem.RecallConfig()
		if rc.MaxChars != 6500 {
			t.Errorf("recall max_chars = %d, want 6500", rc.MaxChars)
		}
		if rc.MinScore != 0.1 {
			t.Errorf("recall min_score = %v, want 0.1", rc.MinScore)
		}
		if rc.Quotas["events"] != 10 {
			t.Errorf("recall events quota = %d, want 10", rc.Quotas["events"])
		}
		if rc.Quotas["preferences"] != 3 {
			t.Errorf("recall preferences quota = %d, want 3", rc.Quotas["preferences"])
		}
	} else {
		t.Errorf("memory provider is %T, want *openviking.Memory", deps.MemoryProvider)
	}
}

// TestApplyContextProviders_BuiltinOverride: an explicit "builtin" for a
// domain keeps the local backend even with an endpoint configured.
func TestApplyContextProviders_BuiltinOverride(t *testing.T) {
	cfg := &config.Config{
		ContextProviders: config.ContextProviderConfig{
			Memory: "builtin",
		},
		OpenViking: config.OpenVikingConfig{Endpoint: "http://127.0.0.1:1933"},
	}
	var deps kernel.Deps
	if err := applyContextProviders(cfg, &deps); err != nil {
		t.Fatal(err)
	}
	if deps.MemoryProvider != nil {
		t.Error("memory provider should stay builtin (nil) when overridden")
	}
	if deps.SkillProvider == nil {
		t.Error("skill provider should still switch to openviking")
	}
	if deps.ResourceProvider == nil {
		t.Error("resource provider should still switch to openviking")
	}
}

// TestApplyContextProviders_NoEndpoint: no endpoint = fully local, the
// openviking provider is never constructed.
func TestApplyContextProviders_NoEndpoint(t *testing.T) {
	cfg := &config.Config{}
	var deps kernel.Deps
	if err := applyContextProviders(cfg, &deps); err != nil {
		t.Fatal(err)
	}
	if deps.MemoryProvider != nil || deps.SkillProvider != nil || deps.ResourceProvider != nil {
		t.Error("no endpoint must leave all providers nil (local)")
	}
}

// TestApplyContextProviders_CustomRecallConfig: a user-configured recall
// config overrides the defaults and propagates to the provider.
func TestApplyContextProviders_CustomRecallConfig(t *testing.T) {
	cfg := &config.Config{
		OpenViking: config.OpenVikingConfig{
			Endpoint: "http://127.0.0.1:1933",
			Recall: config.RecallConfig{
				Quotas:   map[string]int{"events": 5, "entities": 5, "preferences": 2, "experiences": 1},
				MaxChars: 4000,
				MinScore: 0.25,
			},
		},
	}
	var deps kernel.Deps
	if err := applyContextProviders(cfg, &deps); err != nil {
		t.Fatal(err)
	}
	mem, ok := deps.MemoryProvider.(*openviking.Memory)
	if !ok {
		t.Fatalf("memory provider is %T, want *openviking.Memory", deps.MemoryProvider)
	}
	rc := mem.RecallConfig()
	if rc.MaxChars != 4000 {
		t.Errorf("recall max_chars = %d, want 4000", rc.MaxChars)
	}
	if rc.MinScore != 0.25 {
		t.Errorf("recall min_score = %v, want 0.25", rc.MinScore)
	}
	if rc.Quotas["events"] != 5 {
		t.Errorf("recall events quota = %d, want 5", rc.Quotas["events"])
	}
	if rc.Quotas["experiences"] != 1 {
		t.Errorf("recall experiences quota = %d, want 1", rc.Quotas["experiences"])
	}
}

