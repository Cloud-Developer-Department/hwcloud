// Package mcp integrates hwcloud with the Model Context Protocol (MCP).
//
// It provides:
//   - Server: expose hwcloud.Tool instances as MCP tools
//   - Client: import MCP server tools as hwcloud.Tool instances
//
// Import as:
//
//	openmcp "github.com/Cloud-Developer-Department/hwcloud/mcp"
package mcp

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// ToMCPTool converts an hwcloud FunctionDefinition to an MCP Tool.
// The InputSchema is passed through as-is (json.RawMessage is valid JSON Schema).
func ToMCPTool(def hwcloud.FunctionDefinition) *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        def.Name,
		Description: def.Description,
		InputSchema: def.Parameters,
	}
}

// ToFunctionDefinition converts an MCP Tool to an hwcloud FunctionDefinition.
// The MCP InputSchema (a JSON Schema map) is normalized into the neutral
// Parameters model.
func ToFunctionDefinition(t mcpsdk.Tool) (hwcloud.FunctionDefinition, error) {
	return hwcloud.FunctionDefinition{
		Name:        t.Name,
		Description: t.Description,
		Parameters:  hwcloud.ParametersFromMap(toMap(t.InputSchema)),
	}, nil
}

// toMap type-asserts an arbitrary schema value (MCP InputSchema is any)
// to a map; empty map when it isn't one.
func toMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}
