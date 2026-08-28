package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// Client connects to an external MCP server and imports its tools.
//
// Create with [NewClient], connect to a server, then use [Session.Tools]
// to get hwcloud.Tool wrappers:
//
//	client := mcp.NewClient("my-agent", "1.0.0")
//	session, _ := client.ConnectStdio(ctx, "my-mcp-server")
//	tools, _ := session.Tools(ctx)
//	agent := hwcloud.NewAgent("bot", hwcloud.WithTools(tools...))
type Client struct {
	inner *mcpsdk.Client
}

// progress tracking: maps a progress token (generated per CallTool) to the
// tool name so the ProgressNotificationHandler can log which tool is reporting.
var (
	progressMu  sync.Mutex
	progressLog = make(map[string]string) // token → tool name
	progressCtr int64                     // monotonic token counter
)

// NewClient creates an MCP [Client] with the given implementation identity.
// MCP protocol logging is written to slog.Default().
//
// A ProgressNotificationHandler is registered so progress notifications from
// the server are logged via slog at Info level. Each [Session.Tools] adapter
// generates a unique progressToken per CallTool so the handler can attribute
// progress to the originating tool.
func NewClient(name, version string) *Client {
	return &Client{
		inner: mcpsdk.NewClient(&mcpsdk.Implementation{
			Name: name, Version: version,
		}, &mcpsdk.ClientOptions{
			Logger: slog.Default(),
			ProgressNotificationHandler: func(_ context.Context, req *mcpsdk.ProgressNotificationClientRequest) {
				token, _ := req.Params.ProgressToken.(string)
				progressMu.Lock()
				toolName := progressLog[token]
				progressMu.Unlock()
				if toolName == "" {
					return // token unknown — stale or from another client
				}
				slog.Info("mcp progress",
					"tool", toolName,
					"message", req.Params.Message,
					"progress", req.Params.Progress,
					"total", req.Params.Total,
				)
			},
		}),
	}
}

// ConnectStdio connects to an MCP server by spawning it as a subprocess and
// communicating over stdin/stdout. command is the executable; args are its
// arguments. The process inherits the parent environment.
//
// The context is used to start the process. Call [Session.Close] to terminate
// the process and clean up.
func (c *Client) ConnectStdio(ctx context.Context, command string, args ...string) (*Session, error) {
	return c.connectStdioInternal(ctx, command, args, nil)
}

// ConnectStdioWithEnv is like ConnectStdio but appends additional
// environment variables (name=value pairs) to the spawned process.
func (c *Client) ConnectStdioWithEnv(ctx context.Context, command string, args []string, env []string) (*Session, error) {
	return c.connectStdioInternal(ctx, command, args, env)
}

func (c *Client) connectStdioInternal(ctx context.Context, command string, args []string, env []string) (*Session, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp start %q: %w", command, err)
	}

	transport := &mcpsdk.IOTransport{Reader: stdout, Writer: stdin}
	sess, err := c.inner.Connect(ctx, transport, nil)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		if stderr := stderrBuf.String(); stderr != "" {
			return nil, fmt.Errorf("mcp connect stdio: %w\nstderr:\n%s", err, stderr)
		}
		return nil, fmt.Errorf("mcp connect stdio: %w", err)
	}

	return &Session{inner: sess, cmd: cmd, stdin: stdin, stderrBuf: &stderrBuf}, nil
}

// ConnectHTTP connects to an MCP server over HTTP/SSE.
func (c *Client) ConnectHTTP(ctx context.Context, endpoint string) (*Session, error) {
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint: endpoint,
	}
	sess, err := c.inner.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp connect http: %w", err)
	}
	return &Session{inner: sess}, nil
}

// ConnectTransport connects using a custom MCP transport.
// The transport can connect to any MCP server (stdio subprocess,
// HTTP endpoint, in-memory, etc.).
func (c *Client) ConnectTransport(ctx context.Context, transport mcpsdk.Transport) (*Session, error) {
	sess, err := c.inner.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp connect: %w", err)
	}
	return &Session{inner: sess}, nil
}

// Session is an active MCP connection. Use [Session.Tools] to import tools.
type Session struct {
	inner *mcpsdk.ClientSession
	cmd   *exec.Cmd // non-nil for stdio connections

	// stdin is the write end of the process's stdin pipe. Closed in Close()
	// to signal EOF before killing the process.
	stdin io.WriteCloser

	// stderrBuf captures the process's stderr output for diagnostics.
	stderrBuf *bytes.Buffer

	// name is the server name. When set, tools returned by Tools() are
	// named "mcp__<name>__<tool>" — unique
	// across servers and self-describing to the model. Empty = tools
	// keep the server's original names (backward compatible).
	name string
}

// Named sets the server name on the session. Tools returned by Tools()
// are then named "mcp__<name>__<tool>". The name is sanitized for use
// in a tool name ([A-Za-z0-9_-] only — model APIs reject other
// characters). Returns the session for chaining.
func (s *Session) Named(name string) *Session {
	s.name = sanitizeName(name)
	return s
}

// sanitizeName keeps a server name safe for use inside a tool name:
// model APIs require tool names to match [A-Za-z0-9_-]+.
func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "mcp"
	}
	return b.String()
}

// Tools lists tools available on the MCP server and returns them as
// hwcloud.Tool wrappers. Each tool's Execute method calls the MCP
// server via CallTool.
func (s *Session) Tools(ctx context.Context) ([]hwcloud.Tool, error) {
	resp, err := s.inner.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		return nil, fmt.Errorf("mcp list tools: %w", err)
	}

	tools := make([]hwcloud.Tool, 0, len(resp.Tools))
	for _, t := range resp.Tools {
		def, err := ToFunctionDefinition(*t)
		if err != nil {
			return nil, err
		}
		callName := def.Name // the server's registered name
		if s.name != "" {
			def.Name = "mcp__" + s.name + "__" + def.Name
		}
		tools = append(tools, &mcpToolAdapter{
			session:    s.inner,
			def:        def,
			callName:   callName,
			serverName: s.name,
		})
	}
	return tools, nil
}

// ListTools returns the raw MCP tool definitions (without wrapping as
// hwcloud.Tool). Use this if you only need metadata.
func (s *Session) ListTools(ctx context.Context) ([]mcpsdk.Tool, error) {
	resp, err := s.inner.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	tools := make([]mcpsdk.Tool, len(resp.Tools))
	for i, t := range resp.Tools {
		tools[i] = *t
	}
	return tools, nil
}

// Close terminates the MCP session. For stdio connections, stdin is closed
// first to signal EOF, then the subprocess is terminated.
func (s *Session) Close() error {
	// Close stdin to signal EOF before killing the process.
	if s.stdin != nil {
		_ = s.stdin.Close()
	}

	err := s.inner.Close()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	return err
}

// Stderr returns the captured stderr output of the subprocess.
// Empty string if nothing was written to stderr. Use this for
// diagnostics when the process fails or behaves unexpectedly.
func (s *Session) Stderr() string {
	if s.stderrBuf == nil {
		return ""
	}
	return s.stderrBuf.String()
}

// ── mcpToolAdapter ──

// mcpToolAdapter wraps an MCP tool as an hwcloud.Tool.
type mcpToolAdapter struct {
	session *mcpsdk.ClientSession
	// def is the DISPLAY definition — when the session is named, def.Name
	// carries the "mcp__<server>__<tool>" prefix the model sees.
	def hwcloud.FunctionDefinition
	// callName is the ORIGINAL tool name as the MCP server registered it.
	// CallTool MUST send this, not def.Name: the server has no knowledge
	// of the client-side prefix and rejects the prefixed name ("unknown
	// tool").
	callName   string
	serverName string // "" = unnamed session (tools keep original names)
}

func (a *mcpToolAdapter) Definition() hwcloud.FunctionDefinition {
	return a.def
}

func (a *mcpToolAdapter) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	// Unmarshal arguments to map[string]any (MCP CallToolParams expects this).
	var v map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &v); err != nil {
			return hwcloud.ErrorResult(fmt.Errorf("mcp: unmarshal args: %w", err), false, "")
		}
	}

	// Generate a progress token so the server can stream progress back.
	// The token is mapped to the tool name for the ProgressNotificationHandler.
	// Use a string token — JSON round-trips preserve strings exactly, unlike
	// integers which become float64 on the wire and break map lookups.
	token := strconv.FormatInt(atomic.AddInt64(&progressCtr, 1), 10)
	progressMu.Lock()
	progressLog[token] = a.def.Name
	progressMu.Unlock()
	defer func() {
		progressMu.Lock()
		delete(progressLog, token)
		progressMu.Unlock()
	}()

	result, err := a.session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      a.callName, // the server-s registered name, NOT the display name
		Arguments: v,
		Meta:      mcpsdk.Meta{"progressToken": token},
	})
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("mcp call tool %q: %w", a.def.Name, err), false, "")
	}

	// Extract text content from the result.
	text := extractText(result.Content)
	if result.IsError {
		return &hwcloud.ToolResult{
			Content: text,
			Error:   &hwcloud.ToolError{Message: text, Code: "mcp_error"},
		}
	}
	return &hwcloud.ToolResult{Content: text}
}

// extractText concatenates all TextContent from an MCP result.
func extractText(contents []mcpsdk.Content) string {
	var out string
	for _, c := range contents {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			if out != "" {
				out += "\n"
			}
			out += tc.Text
		}
	}
	return out
}
