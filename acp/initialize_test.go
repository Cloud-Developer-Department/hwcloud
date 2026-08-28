package acp

import (
	"context"
	"testing"

	openacp "github.com/Cloud-Developer-Department/hwcloud/acp/sdk"

	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
)

// TestOnInitialize_AgentInfoFromFields verifies OnInitialize reports the
// AgentName/AgentVersion fields rather than hardcoded constants.
func TestOnInitialize_AgentInfoFromFields(t *testing.T) {
	srv := NewAgentServer(agent.New("test"), kernel.Deps{}, nil, nil)
	srv.AgentName = "test-agent"
	srv.AgentVersion = "v9.9.9"

	resp, err := srv.OnInitialize(context.Background(), openacp.InitializeRequest{})
	if err != nil {
		t.Fatalf("OnInitialize: %v", err)
	}
	if resp.AgentInfo == nil {
		t.Fatal("AgentInfo is nil")
	}
	if resp.AgentInfo.Name != "test-agent" {
		t.Errorf("AgentInfo.Name = %q, want %q", resp.AgentInfo.Name, "test-agent")
	}
	if resp.AgentInfo.Version != "v9.9.9" {
		t.Errorf("AgentInfo.Version = %q, want %q", resp.AgentInfo.Version, "v9.9.9")
	}
}

// TestOnInitialize_AgentInfoEmpty verifies that an unconfigured AgentServer
// reports empty identity (a wiring signal, not a hidden default).
func TestOnInitialize_AgentInfoEmpty(t *testing.T) {
	srv := NewAgentServer(agent.New("test"), kernel.Deps{}, nil, nil)
	// AgentName/AgentVersion left zero — no default in NewAgentServer.

	resp, err := srv.OnInitialize(context.Background(), openacp.InitializeRequest{})
	if err != nil {
		t.Fatalf("OnInitialize: %v", err)
	}
	if resp.AgentInfo.Name != "" {
		t.Errorf("AgentInfo.Name = %q, want empty (no default)", resp.AgentInfo.Name)
	}
	if resp.AgentInfo.Version != "" {
		t.Errorf("AgentInfo.Version = %q, want empty (no default)", resp.AgentInfo.Version)
	}
}

// TestConnectMCP_EmptyIdentityDoesNotPanic verifies that an AgentServer
// with empty AgentName/AgentVersion can construct an MCP client without
// panicking — mcp.NewClient("", "") must be a valid call. MCPEnabled is
// true and the server list is nil, so the client is constructed but no
// real dial happens.
func TestConnectMCP_EmptyIdentityDoesNotPanic(t *testing.T) {
	srv := NewAgentServer(agent.New("test"), kernel.Deps{}, nil, nil)
	// AgentName/AgentVersion left zero.
	srv.MCPEnabled = true // exercise client construction

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("connectMCP panicked with empty identity: %v", r)
		}
	}()
	_, _ = srv.connectMCP(context.Background(), nil)
}
