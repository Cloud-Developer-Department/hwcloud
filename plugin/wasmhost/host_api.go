// Package wasmhost provides shared WASM infrastructure for plugin hosts.
// Both the CLI plugin system (plugin/cli/wasm) and the Agent plugin system
// (plugin/agent/wasm) use this package for:
//
//   - Host exports (keyring_get/set/delete, http_request, fs_read/write/readdir, log_*, runtime_*)
//   - ABI helpers (Pack, Unpack, ReadPacked, ReadString, WriteString)
//   - Host API interfaces (Keyring, HTTPClient, FS, Logger)
package wasmhost

import (
	"context"
	"os"
)

// agentRuntimeKey is the context key for AgentRuntime.
type agentRuntimeKeyType struct{}

var agentRuntimeKey agentRuntimeKeyType

// WithAgentRuntime attaches an AgentRuntime to the context.
func WithAgentRuntime(ctx context.Context, rt *AgentRuntime) context.Context {
	return context.WithValue(ctx, agentRuntimeKey, rt)
}

// AgentRuntimeFromContext extracts the AgentRuntime from ctx, or nil.
func AgentRuntimeFromContext(ctx context.Context) *AgentRuntime {
	rt, _ := ctx.Value(agentRuntimeKey).(*AgentRuntime)
	return rt
}

// Keyring abstracts the system keychain for WASM plugins.
type Keyring interface {
	Get(service, key string) (string, error)
	Set(service, key, value string) error
	Delete(service, key string) error
}

// HTTPClient abstracts HTTP outbound calls for WASM plugins.
type HTTPClient interface {
	Do(method, url string, headers map[string]string, body []byte) (status int, respBody []byte, err error)
}

// FS abstracts filesystem access for WASM plugins.
// Paths are relative or absolute; the implementation decides the sandbox boundary.
type FS interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadDir(path string) ([]os.DirEntry, error)
	FileMD5(path string) (string, error)
	DirectoryMD5(path string) (string, error)
}

// Executor abstracts command execution for WASM plugins. The default
// implementation (stdExecutor) runs the command as a child process; a
// deployment can substitute a sandboxed executor (container, seccomp, ...)
// without touching the plugin ABI.
type Executor interface {
	Exec(ctx context.Context, req ExecRequest) ExecResult
}

// ExecRequest describes one command invocation.
type ExecRequest struct {
	// Cmd is the program name (host PATH lookup) or an explicit path.
	Cmd string
	// Args is argv after the program.
	Args []string
	// Cwd is the working directory; empty = inherit the host process cwd.
	Cwd string
	// Env overrides/adds environment variables (inherited from the host
	// process environment). Empty/nil = pure inheritance.
	Env map[string]string
	// EnvReplace, when true, makes Env the *complete* environment for the
	// child process — os.Environ() is NOT merged in. This is the correct
	// mode for passing a minimal allowlist that excludes host secrets
	// (API keys, tokens). When false (default), Env overlays os.Environ().
	EnvReplace bool
	// TimeoutMS bounds the invocation; 0 = default (ExecDefaultTimeout).
	// Values above ExecMaxTimeout are clamped.
	TimeoutMS int
}

// ExecResult is the outcome of one command invocation.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// Err is non-nil only when the command could not run at all (not
	// found, cwd invalid), timed out, or exceeded the output cap. A
	// non-zero exit code is a business result, NOT an error.
	Err error
}

// Logger abstracts structured logging for WASM plugins.
type Logger interface {
	Info(msg string)
	Warn(msg string)
	Error(msg string)
}

// AgentRuntime provides plugins with read/write access to the current
// Agent and Session during a run. Get reads a named value; Set writes one;
// SetModel replaces a model in the global registry; SetEmbedding refreshes
// the configured embedder's credentials in place.
type AgentRuntime struct {
	Get          func(key string) (string, bool)
	Set          func(key string, value string) error
	SetModel     func(provider, modelID, apiKey, baseURL string, maxInputTokens, maxOutputTokens int)
	SetEmbedding func(baseURL, apiKey, model string)
}

// Runtime key constants used by AgentRuntime.Get/Set.
const (
	RuntimeKeySessionID      = "session_id"
	RuntimeKeyUserID         = "user_id"
	RuntimeKeyTurnCount      = "turn_count"
	RuntimeKeyModelID        = "model_id"
	RuntimeKeyProvider       = "provider"
	RuntimeKeyMetadataPrefix = "metadata." // Get("metadata.foo") / Set("metadata.foo", "bar")
)

// HostAPI bundles host-provided capabilities available to WASM plugins
// via the "host" module.  Runtime capabilities are accessed via
// WithAgentRuntime / AgentRuntimeFromContext (context-based, not a field
// on HostAPI) to avoid shared mutable state across goroutines.
//
// By default every export is available to every plugin — the host trusts
// its plugins. Restricted deployments should set Deny to return an error
// for sensitive exports (keyring_*, http_request, fs_*, env_*,
// exec_command, runtime_set_*).
type HostAPI struct {
	Keyring Keyring
	HTTP    HTTPClient
	FS      FS
	Logger  Logger
	// Executor runs commands for exec_command. nil = the export reports
	// "exec not available".
	Executor Executor
	// Deny, when non-nil, is consulted for every sensitive export by
	// name; returning true makes the export fail with "export disabled".
	// nil = all exports allowed.
	Deny func(export string) bool
}

// WithFS enables the fs_* exports (path boundary is the FS
// implementation's responsibility). nil FS (default) makes fs_* return
// "filesystem not available".
func (h *HostAPI) WithFS(fs FS) *HostAPI {
	h.FS = fs
	return h
}

// denied reports whether an export is disabled by the Deny hook.
func (h *HostAPI) denied(name string) bool {
	return h.Deny != nil && h.Deny(name)
}
