package wasm

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero"

	"github.com/Cloud-Developer-Department/hwcloud/keyring"
	"github.com/Cloud-Developer-Department/hwcloud/plugin/wasmhost"
)

// Runtime wraps a wazero runtime with the host module pre-registered.
// All CLI plugins (.wasm files) share this runtime.
type Runtime struct {
	rt   wazero.Runtime
	host *wasmhost.HostAPI
}

// NewRuntime creates a wazero runtime and registers the "host" module
// with a standard HostAPI: system keyring (in-memory fallback), net/http
// client, and slog-backed logger. Callers that need a custom HostAPI can
// construct one via wasmhost.NewHostAPI and register it directly.
func NewRuntime(ctx context.Context) (*Runtime, error) {
	// Same 512 MiB per-plugin linear-memory cap as the agent plugin
	// loader (see plugin/agent/wasm/loader.go). PDK plugins use dlmalloc
	// backed by memory.grow, so this cap bounds how far a plugin can
	// grow its linear memory.
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithMemoryLimitPages(8192))

	// Unrestricted host filesystem for fs_* — the cli:http trust model
	// (whoever can drop a .wasm here has the process's capabilities).
	// Substitute via HostAPI.WithFS for a sandboxed deployment.
	h := wasmhost.NewHostAPI(keyring.NewKeyring()).WithFS(wasmhost.NewOSFS())
	if err := h.RegisterHostModule(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("register host module: %w", err)
	}

	return &Runtime{rt: rt, host: h}, nil
}

// Close releases the wazero runtime.
func (r *Runtime) Close(ctx context.Context) error {
	return r.rt.Close(ctx)
}
