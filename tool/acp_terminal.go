package tool

import (
	"context"
	"encoding/json"
	"fmt"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	openacp "github.com/Cloud-Developer-Department/hwcloud/acp/sdk"
)

// ── ACPTerminalCreate ──

// ACPTerminalCreate spawns a command in a new terminal on the client side.
type ACPTerminalCreate struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPTerminalCreate(client openacp.ClientRequester, sid openacp.SessionId) *ACPTerminalCreate {
	return &ACPTerminalCreate{client: client, sessionID: sid}
}

func (t *ACPTerminalCreate) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "terminal_create",
		Description: "Create a new terminal on the client's machine and start a command. Returns a terminal ID for use with terminal_output, terminal_wait, terminal_kill, and terminal_release.",
		Parameters:  hwcloud.SchemaOf[AcpTerminalCreateParams](),
	}
}

func (t *ACPTerminalCreate) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[AcpTerminalCreateParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("terminal_create: %w", err), false, "")
	}

	resp, err := t.client.CreateTerminal(ctx, openacp.CreateTerminalRequest{
		SessionID:       t.sessionID,
		Command:         params.Command,
		Args:            params.Args,
		Cwd:             params.Cwd,
		OutputByteLimit: params.OutputByteLimit,
	})
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("terminal_create: %w", err), false, "")
	}
	return &hwcloud.ToolResult{Content: "Terminal created. ID: " + resp.TerminalID}
}

// ── ACPTerminalOutput ──

// ACPTerminalOutput polls the current output of a terminal.
type ACPTerminalOutput struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPTerminalOutput(client openacp.ClientRequester, sid openacp.SessionId) *ACPTerminalOutput {
	return &ACPTerminalOutput{client: client, sessionID: sid}
}

func (t *ACPTerminalOutput) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "terminal_output",
		Description: "Get the current output of a running terminal.",
		Parameters:  hwcloud.SchemaOf[AcpTerminalOutputParams](),
	}
}

func (t *ACPTerminalOutput) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[AcpTerminalOutputParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("terminal_output: %w", err), false, "")
	}

	resp, err := t.client.TerminalOutput(ctx, openacp.TerminalOutputRequest{
		SessionID:  t.sessionID,
		TerminalID: params.TerminalID,
	})
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("terminal_output: %w", err), false, "")
	}
	if resp.Truncated {
		return &hwcloud.ToolResult{Content: resp.Output + "\n[output truncated]"}
	}
	return &hwcloud.ToolResult{Content: resp.Output}
}

// ── ACPTerminalWait ──

// ACPTerminalWait blocks until a terminal command finishes.
type ACPTerminalWait struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPTerminalWait(client openacp.ClientRequester, sid openacp.SessionId) *ACPTerminalWait {
	return &ACPTerminalWait{client: client, sessionID: sid}
}

func (t *ACPTerminalWait) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "terminal_wait",
		Description: "Wait for a terminal command to finish and return its exit status.",
		Parameters:  hwcloud.SchemaOf[AcpTerminalWaitParams](),
	}
}

func (t *ACPTerminalWait) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[AcpTerminalWaitParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("terminal_wait: %w", err), false, "")
	}

	resp, err := t.client.WaitForTerminalExit(ctx, openacp.WaitForTerminalExitRequest{
		SessionID:  t.sessionID,
		TerminalID: params.TerminalID,
	})
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("terminal_wait: %w", err), false, "")
	}
	if resp.ExitCode != nil {
		return &hwcloud.ToolResult{Content: fmt.Sprintf("Command exited with code %d.", *resp.ExitCode)}
	}
	if resp.Signal != nil {
		return &hwcloud.ToolResult{Content: fmt.Sprintf("Command terminated by signal: %s.", *resp.Signal)}
	}
	return &hwcloud.ToolResult{Content: "Command finished."}
}

// ── ACPTerminalKill ──

// ACPTerminalKill terminates a running terminal command without releasing it.
type ACPTerminalKill struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPTerminalKill(client openacp.ClientRequester, sid openacp.SessionId) *ACPTerminalKill {
	return &ACPTerminalKill{client: client, sessionID: sid}
}

func (t *ACPTerminalKill) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "terminal_kill",
		Description: "Kill a running terminal command without releasing the terminal. Use terminal_output to get final output, then terminal_release to free resources.",
		Parameters:  hwcloud.SchemaOf[AcpTerminalKillParams](),
	}
}

func (t *ACPTerminalKill) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[AcpTerminalKillParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("terminal_kill: %w", err), false, "")
	}

	_, err = t.client.KillTerminal(ctx, openacp.KillTerminalRequest{
		SessionID:  t.sessionID,
		TerminalID: params.TerminalID,
	})
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("terminal_kill: %w", err), false, "")
	}
	return &hwcloud.ToolResult{Content: "Command killed."}
}

// ── ACPTerminalRelease ──

// ACPTerminalRelease kills the command (if running) and releases all resources.
type ACPTerminalRelease struct {
	client    openacp.ClientRequester
	sessionID openacp.SessionId
}

func NewACPTerminalRelease(client openacp.ClientRequester, sid openacp.SessionId) *ACPTerminalRelease {
	return &ACPTerminalRelease{client: client, sessionID: sid}
}

func (t *ACPTerminalRelease) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "terminal_release",
		Description: "Kill the terminal command (if still running) and release all resources. The terminal ID becomes invalid after this call.",
		Parameters:  hwcloud.SchemaOf[AcpTerminalReleaseParams](),
	}
}

func (t *ACPTerminalRelease) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[AcpTerminalReleaseParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("terminal_release: %w", err), false, "")
	}

	_, err = t.client.ReleaseTerminal(ctx, openacp.ReleaseTerminalRequest{
		SessionID:  t.sessionID,
		TerminalID: params.TerminalID,
	})
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("terminal_release: %w", err), false, "")
	}
	return &hwcloud.ToolResult{Content: "Terminal released."}
}

type AcpTerminalCreateParams struct {
	Command         string   `json:"command" jsonschema:"description=The command to execute."`
	Args            []string `json:"args,omitempty" jsonschema:"description=Command arguments."`
	Cwd             *string  `json:"cwd,omitempty" jsonschema:"description=Working directory (must be absolute)."`
	OutputByteLimit *int     `json:"outputByteLimit,omitempty" jsonschema:"description=Maximum bytes of output to retain."`
}

type AcpTerminalOutputParams struct {
	TerminalID string `json:"terminalId" jsonschema:"description=The terminal ID returned by terminal_create."`
}

type AcpTerminalWaitParams struct {
	TerminalID string `json:"terminalId" jsonschema:"description=The terminal ID returned by terminal_create."`
}

type AcpTerminalKillParams struct {
	TerminalID string `json:"terminalId" jsonschema:"description=The terminal ID returned by terminal_create."`
}

type AcpTerminalReleaseParams struct {
	TerminalID string `json:"terminalId" jsonschema:"description=The terminal ID returned by terminal_create."`
}
