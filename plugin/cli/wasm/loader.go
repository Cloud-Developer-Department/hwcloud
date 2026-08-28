package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/Cloud-Developer-Department/hwcloud/plugin/wasmhost"
)

var commandModules sync.Map

type Module struct {
	Mod  api.Module
	Meta CLIPluginMeta
}

func (r *Runtime) Instantiate(ctx context.Context, wasmBytes []byte, name string) (*Module, CLIPluginMeta, error) {
	mod, err := r.rt.InstantiateWithConfig(ctx, wasmBytes,
		wazero.NewModuleConfig().WithName(name).WithSysNanosleep().WithSysNanotime())
	if err != nil {
		return nil, CLIPluginMeta{}, fmt.Errorf("instantiate: %w", err)
	}
	meta, err := readCLIMeta(ctx, mod)
	if err != nil {
		return nil, CLIPluginMeta{}, fmt.Errorf("metadata: %w", err)
	}
	if !strings.HasPrefix(meta.Type, PluginCLIPrefix) {
		return nil, CLIPluginMeta{}, fmt.Errorf("plugin type %q does not start with %q", meta.Type, PluginCLIPrefix)
	}
	return &Module{Mod: mod, Meta: meta}, meta, nil
}

func (m *Module) CallInit(ctx context.Context, settingsJSON []byte) ([]byte, error) {
	if m.Mod.ExportedFunction("init") == nil {
		return settingsJSON, nil
	}
	// Shared ABI helper (previously duplicated here and in the agent loader).
	return wasmhost.CallWithInput(ctx, m.Mod, "init", settingsJSON)
}

func (m *Module) ReadCommands(ctx context.Context) ([]CommandDef, error) {
	if m.Mod.ExportedFunction("commands") == nil {
		return nil, nil
	}
	raw, err := callExport(ctx, m.Mod, "commands")
	if err != nil {
		return nil, fmt.Errorf("commands: %w", err)
	}
	var cmds []CommandDef
	if err := json.Unmarshal(raw, &cmds); err != nil {
		return nil, fmt.Errorf("parse commands: %w", err)
	}
	for _, cd := range cmds {
		if _, loaded := commandModules.LoadOrStore(cd.Name, m.Mod); loaded {
			slog.Warn("plugin command already registered, overwriting", "plugin", m.Meta.Name, "command", cd.Name)
		}
	}
	return cmds, nil
}

func RunCommand(ctx context.Context, cmdName string, argsJSON string) (string, error) {
	v, ok := commandModules.Load(cmdName)
	if !ok {
		return "", fmt.Errorf("command %q not found", cmdName)
	}
	mod := v.(api.Module)
	if mod.ExportedFunction("run_"+cmdName) == nil {
		return "", fmt.Errorf("command %q has no export run_%s", cmdName, cmdName)
	}
	data, err := wasmhost.CallWithInput(ctx, mod, "run_"+cmdName, []byte(argsJSON))
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", fmt.Errorf("run_%s: empty result", cmdName)
	}
	return string(data), nil
}

func readCLIMeta(ctx context.Context, mod api.Module) (CLIPluginMeta, error) {
	raw, err := callExport(ctx, mod, "metadata")
	if err != nil {
		return CLIPluginMeta{}, err
	}
	var meta CLIPluginMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return CLIPluginMeta{}, fmt.Errorf("parse metadata: %w", err)
	}
	return meta, nil
}

func callExport(ctx context.Context, mod api.Module, name string) ([]byte, error) {
	fn := mod.ExportedFunction(name)
	if fn == nil {
		return nil, fmt.Errorf("export %q not found", name)
	}
	results, err := fn.Call(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("%s: no result", name)
	}
	data := wasmhost.ReadPacked(mod, results[0])
	// Return the result buffer to the guest heap (no-op for older
	// binaries without a dealloc export).
	wasmhost.FreePacked(ctx, mod, results[0])
	return data, nil
}
