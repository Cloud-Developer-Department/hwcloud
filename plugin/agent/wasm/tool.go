package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// wasmTool adapts a WASM tool plugin to hwcloud.Tool.
type wasmTool struct {
	mod  *module
	meta PluginMeta
}

var _ hwcloud.Tool = (*wasmTool)(nil)

func (t *wasmTool) Definition() hwcloud.FunctionDefinition {
	var schemaMap map[string]any
	if len(t.meta.Parameters) > 0 {
		if err := json.Unmarshal(t.meta.Parameters, &schemaMap); err != nil {
			slog.Warn("wasm tool invalid parameters schema", "tool", t.meta.Name, "error", err)
		}
	}
	return hwcloud.FunctionDefinition{
		Name:        t.meta.Name,
		Description: t.meta.Description,
		Parameters:  hwcloud.ParametersFromMap(schemaMap),
	}
}

func (t *wasmTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	input, err := json.Marshal(ToolInput{Args: args})
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("wasm tool %q: marshal input: %w", t.meta.Name, err), false, "")
	}

	output, err := t.mod.invoke(ctx, "execute", input)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("wasm tool %q: %w", t.meta.Name, err), false, "")
	}

	var out ToolOutput
	if err := json.Unmarshal(output, &out); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("wasm tool %q: parse output: %w", t.meta.Name, err), false, "")
	}

	if out.Error != "" {
		return hwcloud.ErrorResult(fmt.Errorf("wasm tool %q: %s", t.meta.Name, out.Error), false, "")
	}
	return &hwcloud.ToolResult{Content: out.Result}
}
