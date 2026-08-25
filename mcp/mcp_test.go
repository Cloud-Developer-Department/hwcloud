package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// echoTool is a simple hwcloud.Tool for testing.
type echoTool struct{}

func (t *echoTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "echo",
		Description: "Echoes the input message back.",
		Parameters:  hwcloud.SchemaOf[Test1Params](),
	}
}

func (t *echoTool) Execute(_ context.Context, args json.RawMessage) *hwcloud.ToolResult {
	var p struct{ Message string }
	json.Unmarshal(args, &p)
	return &hwcloud.ToolResult{Content: "echo: " + p.Message}
}

func TestServerAddTool(t *testing.T) {
	s := NewServer("test", "1.0.0", nil)
	if err := s.AddTool(&echoTool{}); err != nil {
		t.Fatalf("AddTool: %v", err)
	}
	// Adding a tool with empty name should fail.
	if err := s.AddTool(&emptyNameTool{}); err == nil {
		t.Fatal("expected error for empty tool name")
	}
}

type emptyNameTool struct{}

func (t *emptyNameTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{Name: ""}
}
func (t *emptyNameTool) Execute(_ context.Context, args json.RawMessage) *hwcloud.ToolResult {
	return &hwcloud.ToolResult{}
}

func TestServerClientRoundTrip(t *testing.T) {
	ctx := context.Background()

	// Create server and register a tool.
	server := NewServer("test-server", "1.0.0", nil)
	if err := server.AddTool(&echoTool{}); err != nil {
		t.Fatalf("AddTool: %v", err)
	}

	// Create in-memory transports (bidirectional).
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()

	// Start server in background.
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(ctx, serverTransport)
	}()

	// Connect client.
	client := NewClient("test-client", "1.0.0")
	session, err := client.ConnectTransport(ctx, clientTransport)
	if err != nil {
		t.Fatalf("ConnectTransport: %v", err)
	}
	defer session.Close()

	// List tools.
	tools, err := session.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Definition().Name != "echo" {
		t.Fatalf("expected tool name 'echo', got %q", tools[0].Definition().Name)
	}

	// Call the tool.
	args, _ := json.Marshal(map[string]string{"message": "hello"})
	result := tools[0].Execute(ctx, args)
	if result.Error != nil {
		t.Fatalf("Execute: %v", result.Error)
	}
	if result.Content != "echo: hello" {
		t.Fatalf("expected 'echo: hello', got %q", result.Content)
	}

	// Close client session.
	session.Close()

	// Server should exit (transport closed).
	if err := <-serverDone; err != nil {
		t.Logf("server exited with: %v", err)
	}
}

func TestSchemaConversion(t *testing.T) {
	def := hwcloud.FunctionDefinition{
		Name:        "test",
		Description: "a test tool",
		Parameters:  hwcloud.SchemaOf[Test2Params](),
	}

	mcpTool := ToMCPTool(def)
	if mcpTool.Name != "test" {
		t.Fatalf("name: got %q, want %q", mcpTool.Name, "test")
	}
	if mcpTool.Description != "a test tool" {
		t.Fatalf("desc: got %q, want %q", mcpTool.Description, "a test tool")
	}

	// Round-trip back.
	converted, err := ToFunctionDefinition(*mcpTool)
	if err != nil {
		t.Fatalf("ToFunctionDefinition: %v", err)
	}
	if converted.Name != def.Name {
		t.Fatalf("round-trip name: got %q, want %q", converted.Name, def.Name)
	}
	if converted.Description != def.Description {
		t.Fatalf("round-trip desc: got %q, want %q", converted.Description, def.Description)
	}
}

func TestAddTools(t *testing.T) {
	s := NewServer("test", "1.0.0", nil)
	err := s.AddTools([]hwcloud.Tool{&echoTool{}})
	if err != nil {
		t.Fatalf("AddTools: %v", err)
	}
}

func TestClientSessionClose(t *testing.T) {
	ctx := context.Background()

	server := NewServer("test", "1.0.0", nil)
	server.AddTool(&echoTool{})

	st, ct := mcpsdk.NewInMemoryTransports()

	go server.Run(ctx, st)

	client := NewClient("test", "1.0.0")
	session, err := client.ConnectTransport(ctx, ct)
	if err != nil {
		t.Fatalf("ConnectTransport: %v", err)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type Test1Params struct {
	Message string `json:"message" jsonschema:"description=The message to echo"`
}

type Test2Params struct {
	X int `json:"x,omitempty"`
}

// TestSessionNamedToolPrefix: a Named session returns tools as
// "mcp__<server>__<tool>"; an unnamed session keeps original names.
func TestSessionNamedToolPrefix(t *testing.T) {
	ctx := context.Background()
	server := NewServer("test-server", "1.0.0", nil)
	if err := server.AddTool(&echoTool{}); err != nil {
		t.Fatalf("AddTool: %v", err)
	}
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx, serverTransport) }()
	client := NewClient("test-client", "1.0.0")
	session, err := client.ConnectTransport(ctx, clientTransport)
	if err != nil {
		t.Fatalf("ConnectTransport: %v", err)
	}
	defer session.Close()

	// Unnamed: original name.
	tools, err := session.Tools(ctx)
	if err != nil || len(tools) == 0 {
		t.Fatalf("Tools: %v (n=%d)", err, len(tools))
	}
	if got := tools[0].Definition().Name; got != "echo" {
		t.Fatalf("unnamed tool = %q, want echo", got)
	}

	// Named: prefixed + sanitized.
	named, err := session.Named("my server!").Tools(ctx)
	if err != nil {
		t.Fatalf("Named Tools: %v", err)
	}
	if got := named[0].Definition().Name; got != "mcp__my-server-__echo" {
		t.Fatalf("named tool = %q, want mcp__my-server-__echo", got)
	}
}

// TestSanitizeName: characters outside [A-Za-z0-9_-] become '-'; empty
// names fall back to "mcp".
func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"filesystem":  "filesystem",
		"my server!":  "my-server-",
		"abc/def":     "abc-def",
		"中文名":        "---",
		"":            "mcp",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Fatalf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNamedSessionExecuteUsesOriginalName: a named tool's Execute must
// call the server with the ORIGINAL tool name — the prefix is display-
// only. This is the regression for the "unknown tool" bug: sending the
// prefixed name in tools/call makes the server reject it.
func TestNamedSessionExecuteUsesOriginalName(t *testing.T) {
	ctx := context.Background()
	server := NewServer("test-server", "1.0.0", nil)
	if err := server.AddTool(&echoTool{}); err != nil {
		t.Fatalf("AddTool: %v", err)
	}
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx, serverTransport) }()
	client := NewClient("test-client", "1.0.0")
	session, err := client.ConnectTransport(ctx, clientTransport)
	if err != nil {
		t.Fatalf("ConnectTransport: %v", err)
	}
	defer session.Close()

	tools, err := session.Named("agentwork").Tools(ctx)
	if err != nil || len(tools) == 0 {
		t.Fatalf("Named Tools: %v", err)
	}
	named := tools[0]
	if got := named.Definition().Name; got != "mcp__agentwork__echo" {
		t.Fatalf("display name = %q, want mcp__agentwork__echo", got)
	}
	res := named.Execute(ctx, json.RawMessage(`{"message":"hi"}`))
	if res.Error != nil || res.Content != "echo: hi" {
		t.Fatalf("Execute = %+v, want echo: hi (server must receive the original name)", res)
	}
}
