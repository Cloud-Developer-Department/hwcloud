package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/mcp"
	"github.com/Cloud-Developer-Department/hwcloud/version"

	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
)

// connectMcpFromConfig connects to all configured MCP servers and returns
// their tools plus a cleanup function that closes all sessions.
// Failed connections are logged but not fatal.
func connectMcpFromConfig(ctx context.Context, servers map[string]config.McpServerConfig) ([]hwcloud.Tool, func()) {
	if len(servers) == 0 {
		return nil, func() {}
	}

	client := mcp.NewClient(version.Name, version.Version)
	var tools []hwcloud.Tool
	var sessions []*mcp.Session

	for name, s := range servers {
		sess, err := connectMcpOne(ctx, client, s)
		if err != nil {
			slog.Warn("mcp connect failed", "name", name, "error", err)
			continue
		}
		// Named so tools are "mcp__<server>__<tool>" (Claude Code
		// convention) — unique across servers, self-describing.
		list, err := sess.Named(name).Tools(ctx)
		if err != nil {
			sess.Close()
			slog.Warn("mcp list tools failed", "name", name, "error", err)
			continue
		}
		sessions = append(sessions, sess)
		tools = append(tools, list...)
		slog.Info("mcp connected", "name", name, "tools", len(list))
	}

	return tools, func() {
		for _, s := range sessions {
			s.Close()
		}
	}
}

func connectMcpOne(ctx context.Context, client *mcp.Client, cfg config.McpServerConfig) (*mcp.Session, error) {
	switch cfg.Type {
	case "http", "sse":
		if cfg.URL == "" {
			return nil, fmt.Errorf("missing url")
		}
		return client.ConnectHTTP(ctx, cfg.URL)
	default:
		if cfg.Command == "" {
			return nil, fmt.Errorf("missing command")
		}
		cmd := cfg.Command
		if strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") {
			if abs, err := filepath.Abs(cmd); err == nil {
				cmd = abs
			}
		}
		var env []string
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		env = append(env, os.Environ()...)
		return client.ConnectStdioWithEnv(ctx, cmd, cfg.Args, env)
	}
}
