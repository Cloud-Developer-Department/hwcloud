package wasm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/Cloud-Developer-Department/hwcloud/plugin/wasmhost"
)

// loader manages a wazero runtime for WASM plugin modules.
type loader struct {
	runtime wazero.Runtime
}

func newLoader(ctx context.Context) (loader, error) {
	// Cap each plugin's linear memory at 512 MiB of virtual address
	// space. PDK plugins use dlmalloc backed by memory.grow, growing
	// on demand and staying well below this limit in practice; the cap
	// bounds the worst case a plugin can reach (physical memory is
	// allocated lazily per page, so untouched pages are harmless).
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithMemoryLimitPages(8192))
	return loader{runtime: rt}, nil
}

func (l loader) Close(ctx context.Context) error {
	return l.runtime.Close(ctx)
}

// loadModule instantiates a .wasm file and returns a module wrapper.
func (l loader) loadModule(ctx context.Context, name string, wasmBytes []byte) (*module, error) {
	cfg := wazero.NewModuleConfig().WithName(name)
	mod, err := l.runtime.InstantiateWithConfig(ctx, wasmBytes, cfg)
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	return &module{
		mod:         mod,
		hasDecision: mod.ExportedFunction("observe_decision") != nil,
	}, nil
}

// ── module wraps a wazero api.Module ──

type module struct {
	mod api.Module
	// hasDecision is true when the guest exports "observe_decision". Older
	// binaries compiled before the DecisionObserver ABI lack this export;
	// the host probes once at load and silently skips decision events for
	// them (graceful degradation — see runDecisionObservers).
	hasDecision bool
}

// metadataJSON calls guest's metadata() → packed(ptr, len) and returns the JSON bytes.
func (m *module) metadataJSON(ctx context.Context) ([]byte, error) {
	fn := m.mod.ExportedFunction("metadata")
	if fn == nil {
		return nil, fmt.Errorf("plugin missing export metadata")
	}
	results, err := fn.Call(ctx)
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	if len(results) < 1 {
		return nil, fmt.Errorf("metadata: no result")
	}
	data := wasmhost.ReadPacked(m.mod, results[0])
	if data == nil {
		return nil, fmt.Errorf("metadata: read out of bounds")
	}
	// The metadata buffer is a per-load sdk_return allocation — return it
	// to the guest heap (no-op for older binaries without dealloc).
	wasmhost.FreePacked(ctx, m.mod, results[0])
	return data, nil
}

// invoke calls a guest export with JSON input and returns JSON output
// (shared ABI helper — see wasmhost.CallWithInput).
func (m *module) invoke(ctx context.Context, fnName string, input []byte) ([]byte, error) {
	return wasmhost.CallWithInput(ctx, m.mod, fnName, input)
}

// parseMeta reads and unmarshals the plugin metadata.
func (m *module) parseMeta(ctx context.Context) (PluginMeta, error) {
	raw, err := m.metadataJSON(ctx)
	if err != nil {
		return PluginMeta{}, err
	}
	var meta PluginMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return PluginMeta{}, fmt.Errorf("parse metadata: %w", err)
	}
	return meta, nil
}
