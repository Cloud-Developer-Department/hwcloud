package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// Server exposes hwcloud.Tool instances as an MCP server.
//
// Create with [NewServer], add tools with [Server.AddTool], then serve:
//
//	server := mcp.NewServer("my-agent", "1.0.0")
//	server.AddTool(myTool)
//	server.Run(ctx, &mcpsdk.StdioTransport{})          // stdio
//	http.ListenAndServe(":9090", server.HTTPHandler())  // HTTP/SSE
type Server struct {
	inner *mcpsdk.Server
	opts  ServerOptions
}

// ServerOptions configures a [Server].
type ServerOptions struct {
	// Logger is an optional slog.Logger for MCP protocol logging.
	Logger *slog.Logger
}

// NewServer creates an MCP [Server] with the given implementation identity.
// name and version are reported to MCP clients during initialization.
func NewServer(name, version string, opts *ServerOptions) *Server {
	var o ServerOptions
	if opts != nil {
		o = *opts
	}
	s := &Server{opts: o}
	s.inner = mcpsdk.NewServer(&mcpsdk.Implementation{
		Name: name, Version: version,
	}, &mcpsdk.ServerOptions{
		Logger: o.Logger,
	})
	return s
}

// AddTool registers an hwcloud.Tool as an MCP tool on this server.
// The tool's FunctionDefinition and Execute are adapted to MCP's
// ToolHandler interface.
func (s *Server) AddTool(tool hwcloud.Tool) error {
	def := tool.Definition()
	if def.Name == "" {
		return fmt.Errorf("mcp: tool name is required")
	}

	mcpTool := ToMCPTool(def)

	// Adapter: MCP ToolHandler → hwcloud Tool.Execute.
	// The handler receives raw JSON arguments, passes them to Execute,
	// and wraps the result in MCP TextContent.
	//
	// If the client supplied a progressToken, a [ProgressFunc] is built from
	// the server session and injected into the context so the tool can stream
	// progress notifications back to the client during long-running calls.
	// The session ID is also injected so tools can distinguish same-client
	// retries from cross-client conflicts on shared resources.
	handler := func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		ctx = withProgress(ctx, req)
		ctx = withSessionID(ctx, req)
		output := tool.Execute(ctx, req.Params.Arguments)
		if output.Error != nil {
			return &mcpsdk.CallToolResult{
				IsError: true,
				Content: []mcpsdk.Content{
					&mcpsdk.TextContent{Text: output.Error.Message},
				},
			}, nil
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{
				&mcpsdk.TextContent{Text: output.Content},
			},
		}, nil
	}

	s.inner.AddTool(mcpTool, handler)
	return nil
}

// withProgress builds a [hwcloud.ProgressFunc] from the MCP server session
// and the client-supplied progress token, then injects it into ctx.
// If no token is present, ctx is returned unchanged (ProgressFunc will be nil).
func withProgress(ctx context.Context, req *mcpsdk.CallToolRequest) context.Context {
	token := req.Params.GetProgressToken()
	if token == nil {
		return ctx
	}
	ss := req.Session
	return hwcloud.WithProgress(ctx, func(message string, progress, total float64) {
		_ = ss.NotifyProgress(ctx, &mcpsdk.ProgressNotificationParams{
			ProgressToken: token,
			Message:       message,
			Progress:      progress,
			Total:         total,
		})
	})
}

// AddTools registers multiple hwcloud.Tool instances.
func (s *Server) AddTools(tools []hwcloud.Tool) error {
	for _, t := range tools {
		if err := s.AddTool(t); err != nil {
			return err
		}
	}
	return nil
}

// AddToolWithSchema registers an hwcloud.Tool with an explicit JSON Schema
// override. Use this when the tool's Definition().Parameters is not a valid
// JSON Schema or you want to provide a different schema.
func (s *Server) AddToolWithSchema(tool hwcloud.Tool, inputSchema json.RawMessage) error {
	def := tool.Definition()
	mcpTool := &mcpsdk.Tool{
		Name:        def.Name,
		Description: def.Description,
		InputSchema: inputSchema,
	}

	handler := func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		ctx = withProgress(ctx, req)
		ctx = withSessionID(ctx, req)
		output := tool.Execute(ctx, req.Params.Arguments)
		if output.Error != nil {
			return &mcpsdk.CallToolResult{
				IsError: true,
				Content: []mcpsdk.Content{
					&mcpsdk.TextContent{Text: output.Error.Message},
				},
			}, nil
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{
				&mcpsdk.TextContent{Text: output.Content},
			},
		}, nil
	}

	s.inner.AddTool(mcpTool, handler)
	return nil
}

// withSessionID injects the MCP server session ID into ctx so tools can
// distinguish same-client retries from cross-client conflicts. The session
// ID is empty for stdio (single client) and unique per HTTP connection.
func withSessionID(ctx context.Context, req *mcpsdk.CallToolRequest) context.Context {
	if req.Session != nil {
		return WithSessionID(ctx, req.Session.ID())
	}
	return ctx
}

// sessionIDKey is the context key for the MCP session ID (Mcp-Session-Id
// header). Empty for stdio (single client), unique per HTTP connection.
// withSessionID injects it; SessionIDFromContext extracts it.
type sessionIDKey struct{}

// WithSessionID returns a context that carries the MCP session ID.
// An empty sessionID is valid (stdio transport) and means "single client".
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// SessionIDFromContext extracts the MCP session ID from ctx, or "" if none
// was set (e.g. stdio transport, or a call outside an MCP server).
func SessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey{}).(string)
	return id
}

// Run starts the MCP server on the given transport. It blocks until the
// transport closes or ctx is cancelled.
//
// Common transports:
//   - &mcpsdk.StdioTransport{} — stdin/stdout (for subprocess spawning)
//   - in-memory transports (see mcpsdk.NewInMemoryTransports for testing)
func (s *Server) Run(ctx context.Context, transport mcpsdk.Transport) error {
	return s.inner.Run(ctx, transport)
}

// HTTPHandler returns an http.Handler that serves MCP over SSE (Server-Sent
// Events). Use this to expose the server over HTTP:
//
//	http.ListenAndServe(":9090", server.HTTPHandler())
func (s *Server) HTTPHandler() http.Handler {
	return mcpsdk.NewStreamableHTTPHandler(
		func(r *http.Request) *mcpsdk.Server { return s.inner },
		&mcpsdk.StreamableHTTPOptions{},
	)
}

// Inner returns the underlying mcpsdk.Server for advanced use cases.
func (s *Server) Inner() *mcpsdk.Server {
	return s.inner
}
