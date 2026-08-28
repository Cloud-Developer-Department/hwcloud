package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
	"github.com/Cloud-Developer-Department/hwcloud/version"
)

// TestConnectMcpFromConfig_ClientIdentity verifies the HTTP-path MCP
// client reports the build identity (version.Name/Version) to the MCP
// server in the initialize handshake, not a hardcoded constant.
//
// It spawns a stub MCP server that records the clientInfo from the
// initialize request, then asserts it matches version.Name/Version.
func TestConnectMcpFromConfig_ClientIdentity(t *testing.T) {
	stubSrc := strings.Join([]string{
		"package main",
		`import (`,
		`	"bufio"`,
		`	"encoding/json"`,
		`	"fmt"`,
		`	"os"`,
		`	"strings"`,
		`)`,
		`func main() {`,
		`	sc := bufio.NewScanner(os.Stdin)`,
		`	for sc.Scan() {`,
		`		line := sc.Text()`,
		`		if !strings.Contains(line, "initialize") { continue }`,
		`		var env struct { Params struct { ClientInfo struct { Name, Version string } ` + "`json:\"clientInfo\"`" + ` } ` + "`json:\"params\"`" + ` }`,
		`		_ = json.Unmarshal([]byte(line), &env)`,
		`		out := env.Params.ClientInfo.Name + "|" + env.Params.ClientInfo.Version`,
		`		_ = os.WriteFile(os.Args[1], []byte(out), 0644)`,
		`		var id json.RawMessage`,
		`		_ = json.Unmarshal([]byte(line), &id)`,
		`		_ = id`,
		`		fmt.Println(` + "`" + `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"stub","version":"1.0.0"},"capabilities":{}}}` + "`" + `)`,
		`		return`,
		`	}`,
		`}`,
	}, "\n")

	dir := t.TempDir()
	stubPath := filepath.Join(dir, "stub.go")
	if err := os.WriteFile(stubPath, []byte(stubSrc), 0644); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(dir, "stub")
	cmd := exec.Command("go", "build", "-o", binPath, stubPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub server: %v\n%s", err, out)
	}

	outFile := filepath.Join(dir, "clientinfo.txt")
	cfg := map[string]config.McpServerConfig{
		"stub": {Command: binPath, Args: []string{outFile}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, cleanup := connectMcpFromConfig(ctx, cfg)
	defer cleanup()

	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(outFile); err == nil && len(b) > 0 {
			got = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == "" {
		t.Fatal("stub server never recorded clientInfo")
	}
	parts := strings.SplitN(got, "|", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected clientInfo output: %q", got)
	}
	if parts[0] != version.Name {
		t.Errorf("clientInfo.Name = %q, want %q", parts[0], version.Name)
	}
	if parts[1] != version.Version {
		t.Errorf("clientInfo.Version = %q, want %q", parts[1], version.Version)
	}
}

// keep encoding/json import used for future expansion of stub protocol
var _ = json.Marshal
