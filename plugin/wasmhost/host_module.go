package wasmhost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// RegisterHostModule instantiates the "host" wazero module with the
// following exports visible to WASM plugins:
//
//	keyring_get(service_ptr, service_len, key_ptr, key_len) → packed(json)
//	keyring_set(service_ptr, service_len, key_ptr, key_len, val_ptr, val_len) → packed(json)
//	keyring_delete(service_ptr, service_len, key_ptr, key_len) → packed(json)
//	exec_command(json_ptr, json_len) → packed(json) // {"stdout","stderr","exit_code","error"}
//	env_get(key_ptr, key_len) → packed(json)   // {"value":"...","error":""}
//	env_set(key_ptr, key_len, val_ptr, val_len) → packed(json)
//	env_unset(key_ptr, key_len) → packed(json)
//	env_list() → packed(json)                  // {"env":[{"key":"...","value":"..."},...],"error":""}
//	http_request(method_ptr, method_len, url_ptr, url_len,
//	             headers_ptr, headers_len, body_ptr, body_len) → packed(json)
//	fs_read(path_ptr, path_len) → packed(json)   // {"data":"<base64>","error":""}
//	fs_write(path_ptr, path_len, data_ptr, data_len) → packed(json)
//	fs_readdir(path_ptr, path_len) → packed(json) // {"entries":[{"name":"...","is_dir":true},...],"error":""}
//	log_info / log_warn / log_error(msg_ptr, msg_len) → void
//	utc_now() → uint64 (nanoseconds)
//
// The env_* exports read and write the HOST PROCESS environment — not a
// per-plugin or per-session view. A plugin's env_set is visible to every
// other plugin and to the host itself, and env_list exposes the full
// process environment (API keys included). Restricted deployments should
// deny env_* via the Deny hook.
//
// All functions that can fail return packed JSON with an "error" field.
// Empty error string = success.

func (h *HostAPI) RegisterHostModule(ctx context.Context, rt wazero.Runtime) error {
	read := func(mod api.Module, ptr, length uint32) string {
		return ReadString(mod, ptr, length)
	}
	write := func(ctx context.Context, mod api.Module, data []byte) uint64 {
		return WriteString(ctx, mod, data)
	}
	writeJSON := func(ctx context.Context, mod api.Module, v any) uint64 {
		b, _ := json.Marshal(v)
		return write(ctx, mod, b)
	}

	_, err := rt.NewHostModuleBuilder("host").

		// ── keyring_get → {"value": "...", "error": ""} ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, svcPtr, svcLen, keyPtr, keyLen uint32) uint64 {
			if h.denied("keyring_get") {
				return writeJSON(ctx, mod, map[string]string{"error": "export keyring_get disabled"})
			}
			if h.Keyring == nil {
				return writeJSON(ctx, mod, map[string]string{"error": "keyring not available"})
			}
			svc := read(mod, svcPtr, svcLen)
			key := read(mod, keyPtr, keyLen)
			v, err := h.Keyring.Get(svc, key)
			if err != nil {
				return writeJSON(ctx, mod, map[string]string{"error": err.Error()})
			}
			return writeJSON(ctx, mod, map[string]string{"value": v})
		}).
		Export("keyring_get").

		// ── keyring_set → {"error": ""} ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, svcPtr, svcLen, keyPtr, keyLen, valPtr, valLen uint32) uint64 {
			if h.denied("keyring_set") {
				return writeJSON(ctx, mod, map[string]string{"error": "export keyring_set disabled"})
			}
			if h.Keyring == nil {
				return writeJSON(ctx, mod, map[string]string{"error": "keyring not available"})
			}
			svc := read(mod, svcPtr, svcLen)
			key := read(mod, keyPtr, keyLen)
			val := read(mod, valPtr, valLen)
			if err := h.Keyring.Set(svc, key, val); err != nil {
				return writeJSON(ctx, mod, map[string]string{"error": err.Error()})
			}
			return writeJSON(ctx, mod, map[string]string{})
		}).
		Export("keyring_set").

		// ── keyring_delete → {"error": ""} ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, svcPtr, svcLen, keyPtr, keyLen uint32) uint64 {
			if h.denied("keyring_delete") {
				return writeJSON(ctx, mod, map[string]string{"error": "export keyring_delete disabled"})
			}
			if h.Keyring == nil {
				return writeJSON(ctx, mod, map[string]string{"error": "keyring not available"})
			}
			svc := read(mod, svcPtr, svcLen)
			key := read(mod, keyPtr, keyLen)
			if err := h.Keyring.Delete(svc, key); err != nil {
				return writeJSON(ctx, mod, map[string]string{"error": err.Error()})
			}
			return writeJSON(ctx, mod, map[string]string{})
		}).
		Export("keyring_delete").

		// ── exec_command → {"stdout":"...","stderr":"...","exit_code":0,"error":""} ──
		// Input JSON: {"cmd":"ls","args":["-la"],"cwd":"/tmp","env":{...},"timeout_ms":120000}
		// cmd is required; args/cwd/env/timeout_ms are optional. env merges
		// over the host process environment (inherited unless overridden).
		// timeout_ms defaults to 120s and is clamped to 10min. The command
		// runs as a child process — platform is opaque to the guest (the
		// host resolves the program via PATH). exit_code != 0 is a business
		// result, not an error; "error" is set only when the command could
		// not run, timed out, or exceeded the output cap.
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, jsonPtr, jsonLen uint32) uint64 {
			if h.denied("exec_command") {
				return writeJSON(ctx, mod, map[string]string{"error": "export exec_command disabled"})
			}
			if h.Executor == nil {
				return writeJSON(ctx, mod, map[string]string{"error": "exec not available"})
			}
			raw := read(mod, jsonPtr, jsonLen)
			var req struct {
				Cmd       string            `json:"cmd"`
				Args      []string          `json:"args"`
				Cwd       string            `json:"cwd"`
				Env       map[string]string `json:"env"`
				EnvReplace bool             `json:"env_replace"`
				TimeoutMS int               `json:"timeout_ms"`
			}
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				return writeJSON(ctx, mod, map[string]string{"error": fmt.Sprintf("invalid exec request: %v", err)})
			}
			if req.Cmd == "" {
				return writeJSON(ctx, mod, map[string]string{"error": "cmd is required"})
			}
			res := h.Executor.Exec(ctx, ExecRequest{
				Cmd:        req.Cmd,
				Args:       req.Args,
				Cwd:        req.Cwd,
				Env:        req.Env,
				EnvReplace: req.EnvReplace,
				TimeoutMS:  req.TimeoutMS,
			})
			// The guest heap bounds every host response (the guest must
			// allocate the full JSON to deserialize it). An output near
			// the heap size would make the guest panic on alloc failure
			// — reject with a specific error instead.
			if len(res.Stdout)+len(res.Stderr) > maxExecGuestResponse {
				return writeJSON(ctx, mod, map[string]any{
					"stdout": "", "stderr": "", "exit_code": 0,
					"error": fmt.Sprintf("exec: output too large for guest (%d bytes)", len(res.Stdout)+len(res.Stderr)),
				})
			}
			result := struct {
				Stdout   string `json:"stdout"`
				Stderr   string `json:"stderr"`
				ExitCode int    `json:"exit_code"`
				Error    string `json:"error,omitempty"`
			}{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}
			if res.Err != nil {
				result.Error = res.Err.Error()
			}
			return writeJSON(ctx, mod, result)
		}).
		Export("exec_command").

		// ── env_get → {"value": "...", "error": ""} ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, keyPtr, keyLen uint32) uint64 {
			if h.denied("env_get") {
				return writeJSON(ctx, mod, map[string]string{"error": "export env_get disabled"})
			}
			return writeJSON(ctx, mod, map[string]string{"value": os.Getenv(read(mod, keyPtr, keyLen))})
		}).
		Export("env_get").

		// ── env_set → {"error": ""} (host process environment) ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, keyPtr, keyLen, valPtr, valLen uint32) uint64 {
			if h.denied("env_set") {
				return writeJSON(ctx, mod, map[string]string{"error": "export env_set disabled"})
			}
			if err := os.Setenv(read(mod, keyPtr, keyLen), read(mod, valPtr, valLen)); err != nil {
				return writeJSON(ctx, mod, map[string]string{"error": err.Error()})
			}
			return writeJSON(ctx, mod, map[string]string{})
		}).
		Export("env_set").

		// ── env_unset → {"error": ""} ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, keyPtr, keyLen uint32) uint64 {
			if h.denied("env_unset") {
				return writeJSON(ctx, mod, map[string]string{"error": "export env_unset disabled"})
			}
			if err := os.Unsetenv(read(mod, keyPtr, keyLen)); err != nil {
				return writeJSON(ctx, mod, map[string]string{"error": err.Error()})
			}
			return writeJSON(ctx, mod, map[string]string{})
		}).
		Export("env_unset").

		// ── env_list → {"env": [{"key":"...","value":"..."},...], "error": ""} ──
		// The full host process environment — secrets included — so this is
		// gated by the same Deny hook as the other env_* exports.
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module) uint64 {
			if h.denied("env_list") {
				return writeJSON(ctx, mod, map[string]string{"error": "export env_list disabled"})
			}
			env := os.Environ()
			entries := make([]map[string]string, 0, len(env))
			for _, kv := range env {
				k, v, _ := strings.Cut(kv, "=")
				entries = append(entries, map[string]string{"key": k, "value": v})
			}
			return writeJSON(ctx, mod, map[string]any{"env": entries})
		}).
		Export("env_list").

		// ── http_request → {"status": 200, "body": "...", "error": ""} ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module,
			methodPtr, methodLen uint32,
			urlPtr, urlLen uint32,
			headersPtr, headersLen uint32,
			bodyPtr, bodyLen uint32) uint64 {
			if h.denied("http_request") {
				return writeJSON(ctx, mod, map[string]string{"error": "export http_request disabled"})
			}

			if h.HTTP == nil {
				return writeJSON(ctx, mod, map[string]string{"error": "http not available"})
			}

			method := read(mod, methodPtr, methodLen)
			url := read(mod, urlPtr, urlLen)
			headersRaw := read(mod, headersPtr, headersLen)
			bodyRaw := read(mod, bodyPtr, bodyLen)

			var headers map[string]string
			if headersRaw != "" {
				json.Unmarshal([]byte(headersRaw), &headers)
			}
			status, respBody, err := h.HTTP.Do(method, url, headers, []byte(bodyRaw))

			result := struct {
				Status int    `json:"status"`
				Body   string `json:"body"`
				Error  string `json:"error,omitempty"`
			}{Status: status, Body: string(respBody)}
			if err != nil {
				result.Error = err.Error()
			}
			return writeJSON(ctx, mod, result)
		}).
		Export("http_request").

		// ── fs_read → {"data": "<base64>", "error": ""} ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, pathPtr, pathLen uint32) uint64 {
			if h.denied("fs_read") {
				return writeJSON(ctx, mod, map[string]string{"error": "export fs_read disabled"})
			}
			if h.FS == nil {
				return writeJSON(ctx, mod, map[string]string{"error": "filesystem not available"})
			}
			path := read(mod, pathPtr, pathLen)
			data, err := h.FS.ReadFile(path)
			if err != nil {
				return writeJSON(ctx, mod, map[string]string{"error": err.Error()})
			}
			return writeJSON(ctx, mod, map[string]string{"data": base64.StdEncoding.EncodeToString(data)})
		}).
		Export("fs_read").

		// ── fs_write → {"error": ""} ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, pathPtr, pathLen, dataPtr, dataLen uint32) uint64 {
			if h.denied("fs_write") {
				return writeJSON(ctx, mod, map[string]string{"error": "export fs_write disabled"})
			}
			if h.FS == nil {
				return writeJSON(ctx, mod, map[string]string{"error": "filesystem not available"})
			}
			path := read(mod, pathPtr, pathLen)
			raw, ok := mod.Memory().Read(dataPtr, dataLen)
			if !ok {
				return writeJSON(ctx, mod, map[string]string{"error": "read guest memory out of bounds"})
			}
			if err := h.FS.WriteFile(path, raw); err != nil {
				return writeJSON(ctx, mod, map[string]string{"error": err.Error()})
			}
			return writeJSON(ctx, mod, map[string]string{})
		}).
		Export("fs_write").

		// ── fs_readdir → {"entries": [{"name":"...","is_dir":true},...], "error": ""} ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, pathPtr, pathLen uint32) uint64 {
			if h.denied("fs_readdir") {
				return writeJSON(ctx, mod, map[string]string{"error": "export fs_readdir disabled"})
			}
			if h.FS == nil {
				return writeJSON(ctx, mod, map[string]string{"error": "filesystem not available"})
			}
			path := read(mod, pathPtr, pathLen)
			entries, err := h.FS.ReadDir(path)
			if err != nil {
				return writeJSON(ctx, mod, map[string]string{"error": err.Error()})
			}
			type dirEntry struct {
				Name  string `json:"name"`
				IsDir bool   `json:"is_dir"`
			}
			out := make([]dirEntry, len(entries))
			for i, e := range entries {
				out[i] = dirEntry{Name: e.Name(), IsDir: e.IsDir()}
			}
			return writeJSON(ctx, mod, map[string]any{"entries": out})
		}).
		Export("fs_readdir").

		// ── file_md5 → {"md5": "...", "error": ""} ──
		// Computes the MD5 of a single file. Reserved for plugins that
		// need per-file hashing; the skill-manager plugin currently uses
		// directory_md5 for aggregate change detection.
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, pathPtr, pathLen uint32) uint64 {
			if h.denied("file_md5") {
				return writeJSON(ctx, mod, map[string]string{"error": "export file_md5 disabled"})
			}
			if h.FS == nil {
				return writeJSON(ctx, mod, map[string]string{"error": "filesystem not available"})
			}
			path := read(mod, pathPtr, pathLen)
			md5val, err := h.FS.FileMD5(path)
			if err != nil {
				return writeJSON(ctx, mod, map[string]string{"error": err.Error()})
			}
			return writeJSON(ctx, mod, map[string]string{"md5": md5val})
		}).
		Export("file_md5").

		// ── directory_md5 → {"md5": "...", "error": ""} ──
		// Computes an aggregate MD5 of a directory (dirname + all files
		// sorted by relative path). Used by plugins to detect content
		// changes without downloading the whole tree for comparison.
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, pathPtr, pathLen uint32) uint64 {
			if h.denied("directory_md5") {
				return writeJSON(ctx, mod, map[string]string{"error": "export directory_md5 disabled"})
			}
			if h.FS == nil {
				return writeJSON(ctx, mod, map[string]string{"error": "filesystem not available"})
			}
			path := read(mod, pathPtr, pathLen)
			md5val, err := h.FS.DirectoryMD5(path)
			if err != nil {
				return writeJSON(ctx, mod, map[string]string{"error": err.Error()})
			}
			return writeJSON(ctx, mod, map[string]string{"md5": md5val})
		}).
		Export("directory_md5").

		// ── log_info / log_warn / log_error → void ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, msgPtr uint32, msgLen uint32) {
			msg := read(mod, msgPtr, msgLen)
			if h.Logger != nil {
				h.Logger.Info(msg)
			}
		}).
		Export("log_info").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, msgPtr uint32, msgLen uint32) {
			msg := read(mod, msgPtr, msgLen)
			if h.Logger != nil {
				h.Logger.Warn(msg)
			}
		}).
		Export("log_warn").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, msgPtr uint32, msgLen uint32) {
			msg := read(mod, msgPtr, msgLen)
			if h.Logger != nil {
				h.Logger.Error(msg)
			}
		}).
		Export("log_error").

		// ── utc_now → uint64 ──
		NewFunctionBuilder().
		WithFunc(func(_ context.Context, _ api.Module) uint64 {
			return uint64(time.Now().UnixNano())
		}).
		Export("utc_now").

		// ── runtime_* → {"value":"...","error":""} ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module) uint64 {
			return h.runtimeGet(ctx, mod, RuntimeKeySessionID)
		}).
		Export("runtime_session_id").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module) uint64 {
			return h.runtimeGet(ctx, mod, RuntimeKeyUserID)
		}).
		Export("runtime_user_id").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module) uint64 {
			return h.runtimeGet(ctx, mod, RuntimeKeyTurnCount)
		}).
		Export("runtime_turn_count").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module) uint64 {
			return h.runtimeGet(ctx, mod, RuntimeKeyModelID)
		}).
		Export("runtime_model_id").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module) uint64 {
			return h.runtimeGet(ctx, mod, RuntimeKeyProvider)
		}).
		Export("runtime_provider").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, keyPtr, keyLen uint32) uint64 {
			key := read(mod, keyPtr, keyLen)
			return h.runtimeGet(ctx, mod, RuntimeKeyMetadataPrefix+key)
		}).
		Export("runtime_get_metadata").

		// ── runtime_set_* → {"error":""} ──
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, keyPtr, keyLen, valPtr, valLen uint32) uint64 {
			if h.denied("runtime_set_metadata") {
				return writeJSON(ctx, mod, map[string]string{"error": "export runtime_set_metadata disabled"})
			}
			key := read(mod, keyPtr, keyLen)
			val := read(mod, valPtr, valLen)
			return h.runtimeSet(ctx, mod, RuntimeKeyMetadataPrefix+key, val)
		}).
		Export("runtime_set_metadata").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, valPtr, valLen uint32) uint64 {
			if h.denied("runtime_set_model_config") {
				return writeJSON(ctx, mod, map[string]string{"error": "export runtime_set_model_config disabled"})
			}
			return h.runtimeSetModelConfig(ctx, mod, read(mod, valPtr, valLen))
		}).
		Export("runtime_set_model_config").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, valPtr, valLen uint32) uint64 {
			if h.denied("runtime_set_system_prompts") {
				return writeJSON(ctx, mod, map[string]string{"error": "export runtime_set_system_prompts disabled"})
			}
			return h.runtimeSet(ctx, mod, "system_prompts", read(mod, valPtr, valLen))
		}).
		Export("runtime_set_system_prompts").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, valPtr, valLen uint32) uint64 {
			if h.denied("runtime_set_max_turns") {
				return writeJSON(ctx, mod, map[string]string{"error": "export runtime_set_max_turns disabled"})
			}
			return h.runtimeSet(ctx, mod, "max_turns", read(mod, valPtr, valLen))
		}).
		Export("runtime_set_max_turns").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, valPtr, valLen uint32) uint64 {
			if h.denied("runtime_set_embedding_config") {
				return writeJSON(ctx, mod, map[string]string{"error": "export runtime_set_embedding_config disabled"})
			}
			return h.runtimeSetEmbeddingConfig(ctx, mod, read(mod, valPtr, valLen))
		}).
		Export("runtime_set_embedding_config").
		Instantiate(ctx)
	return err
}

// runtimeGet reads a value from the runtime. Returns {"value":"...","error":""}.
func (h *HostAPI) runtimeGet(ctx context.Context, mod api.Module, key string) uint64 {
	rt := AgentRuntimeFromContext(ctx)
	if rt == nil {
		b, _ := json.Marshal(map[string]string{"error": "runtime not available"})
		return WriteString(ctx, mod, b)
	}
	v, ok := rt.Get(key)
	if !ok {
		b, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("key %q not found", key)})
		return WriteString(ctx, mod, b)
	}
	b, _ := json.Marshal(map[string]string{"value": v})
	return WriteString(ctx, mod, b)
}

// runtimeSet writes a value to the runtime. Returns {"error":""}.
func (h *HostAPI) runtimeSet(ctx context.Context, mod api.Module, key, value string) uint64 {
	rt := AgentRuntimeFromContext(ctx)
	if rt == nil {
		b, _ := json.Marshal(map[string]string{"error": "runtime not available"})
		return WriteString(ctx, mod, b)
	}
	if err := rt.Set(key, value); err != nil {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return WriteString(ctx, mod, b)
	}
	b, _ := json.Marshal(map[string]string{})
	return WriteString(ctx, mod, b)
}

// runtimeSetModelConfig parses a JSON model_config and calls rt.SetModel.
// Input: {"provider":"deepseek","model_id":"v4","api_key":"sk-...","base_url":"https://..."}
// api_key and base_url are optional — empty values leave the existing ones unchanged.
func (h *HostAPI) runtimeSetModelConfig(ctx context.Context, mod api.Module, raw string) uint64 {
	rt := AgentRuntimeFromContext(ctx)
	if rt == nil {
		b, _ := json.Marshal(map[string]string{"error": "runtime not available"})
		return WriteString(ctx, mod, b)
	}
	if rt.SetModel == nil {
		b, _ := json.Marshal(map[string]string{"error": "SetModel not configured"})
		return WriteString(ctx, mod, b)
	}
	var mc struct {
		Provider       string `json:"provider"`
		ModelID        string `json:"model_id"`
		APIKey         string `json:"api_key"`
		BaseURL        string `json:"base_url"`
		MaxInputTokens int    `json:"max_input_tokens"`
		MaxOutputTokens int   `json:"max_output_tokens"`
	}
	if err := json.Unmarshal([]byte(raw), &mc); err != nil {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return WriteString(ctx, mod, b)
	}
	if mc.Provider == "" || mc.ModelID == "" {
		b, _ := json.Marshal(map[string]string{"error": "provider and model_id are required"})
		return WriteString(ctx, mod, b)
	}
	rt.SetModel(mc.Provider, mc.ModelID, mc.APIKey, mc.BaseURL, mc.MaxInputTokens, mc.MaxOutputTokens)
	b, _ := json.Marshal(map[string]string{})
	return WriteString(ctx, mod, b)
}

// runtimeSetEmbeddingConfig parses a JSON embedding config and calls
// rt.SetEmbedding to refresh the embedder's credentials in place.
// Input: {"base_url":"https://...","api_key":"sk-...","model":"text-embedding-3-small"}
func (h *HostAPI) runtimeSetEmbeddingConfig(ctx context.Context, mod api.Module, raw string) uint64 {
	rt := AgentRuntimeFromContext(ctx)
	if rt == nil {
		b, _ := json.Marshal(map[string]string{"error": "runtime not available"})
		return WriteString(ctx, mod, b)
	}
	if rt.SetEmbedding == nil {
		b, _ := json.Marshal(map[string]string{"error": "SetEmbedding not configured"})
		return WriteString(ctx, mod, b)
	}
	var ec struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal([]byte(raw), &ec); err != nil {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return WriteString(ctx, mod, b)
	}
	if ec.Model == "" {
		b, _ := json.Marshal(map[string]string{"error": "model is required"})
		return WriteString(ctx, mod, b)
	}
	rt.SetEmbedding(ec.BaseURL, ec.APIKey, ec.Model)
	b, _ := json.Marshal(map[string]string{})
	return WriteString(ctx, mod, b)
}
