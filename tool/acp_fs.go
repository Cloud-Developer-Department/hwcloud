package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	openacp "github.com/Cloud-Developer-Department/hwcloud/acp/sdk"
)

// ACPReadFile reads a file from the client's filesystem via fs/read_text_file.
// This is an Agent→Client RPC — the agent asks the client to read a file.
type ACPReadFile struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPReadFile(client openacp.ClientRequester, sid openacp.SessionId) *ACPReadFile {
	return &ACPReadFile{client: client, sessionID: sid}
}

func (t *ACPReadFile) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "read_client_file",
		Description: "Read a file from the client's filesystem. Use this when the file is on the user's machine rather than the agent's workspace. Path must be absolute.",
		Parameters:  hwcloud.SchemaOf[AcpFs1Params](),
	}
}

func (t *ACPReadFile) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[AcpFs1Params](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("read_client_file: %w", err), false, "")
	}
	if strings.TrimSpace(params.Path) == "" {
		return hwcloud.ErrorResult(fmt.Errorf("read_client_file: path is required"), false, "")
	}

	resp, err := t.client.ReadTextFile(ctx, openacp.ReadTextFileRequest{
		SessionID: t.sessionID,
		Path:      params.Path,
		Line:      params.Line,
		Limit:     params.Limit,
	})
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("read_client_file: %w", err), false, "")
	}
	return &hwcloud.ToolResult{Content: resp.Content}
}

// ACPWriteFile writes content to a file on the client's filesystem via
// fs/write_text_file. This is an Agent→Client RPC.
type ACPWriteFile struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPWriteFile(client openacp.ClientRequester, sid openacp.SessionId) *ACPWriteFile {
	return &ACPWriteFile{client: client, sessionID: sid}
}

func (t *ACPWriteFile) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "write_client_file",
		Description: "Write content to a file on the client's filesystem. Use this when the file needs to be written to the user's machine rather than the agent's workspace. Path must be absolute.",
		Parameters:  hwcloud.SchemaOf[AcpFs2Params](),
	}
}

func (t *ACPWriteFile) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[AcpFs2Params](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("write_client_file: %w", err), false, "")
	}
	if strings.TrimSpace(params.Path) == "" {
		return hwcloud.ErrorResult(fmt.Errorf("write_client_file: path is required"), false, "")
	}

	_, err = t.client.WriteTextFile(ctx, openacp.WriteTextFileRequest{
		SessionID: t.sessionID,
		Path:      params.Path,
		Content:   params.Content,
	})
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("write_client_file: %w", err), false, "")
	}
	return &hwcloud.ToolResult{Content: "File written successfully."}
}

type AcpFs1Params struct {
	Path  string `json:"path" jsonschema:"description=Absolute path to the file to read."`
	Line  *int   `json:"line,omitempty" jsonschema:"description=Optional 1-based line number to start reading from."`
	Limit *int   `json:"limit,omitempty" jsonschema:"description=Optional maximum number of lines to read."`
}

type AcpFs2Params struct {
	Path    string `json:"path" jsonschema:"description=Absolute path to the file to write."`
	Content string `json:"content" jsonschema:"description=Text content to write to the file."`
}
