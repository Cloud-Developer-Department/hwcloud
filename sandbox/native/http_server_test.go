package native

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/process"
)

// TestShellPythonHTTPServer reproduces the "Empty reply from server" bug and
// verifies the *os.File fix. It simulates the exact code path the shell tool
// uses: ProcessManager + unconfinedRun with StdoutW/StderrW.
func TestShellPythonHTTPServer(t *testing.T) {
	// Skip if no python3
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	workDir := t.TempDir()
	sb, err := New(workDir)
	if err != nil {
		t.Fatal(err)
	}

	pm, err := process.NewManager(workDir + "/.hwcloud/proc")
	if err != nil {
		t.Fatal(err)
	}
	defer pm.Cleanup()

	ctx := process.WithManager(context.Background(), pm)

	// Step 1: Start python http.server (same as shell tool does)
	proc, err := pm.Create("python3 -m http.server 2345")
	if err != nil {
		t.Fatal(err)
	}

	cmd := hwcloud.Command{
		Program: "/bin/bash",
		Args:    []string{"-c", "python3 -m http.server 2345"},
		WorkDir: workDir,
		StdoutW: proc.StdoutW(),
		StderrW: proc.StderrW(),
	}

	// Confirm that StdoutW/StderrW are *os.File (as intended by process.Manager)
	t.Logf("StdoutW type: %T", proc.StdoutW())
	t.Logf("StderrW type: %T", proc.StderrW())

	shellCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	result, runErr := sb.Run(shellCtx, cmd)
	t.Logf("Run returned: err=%v, exit=%d", runErr, result.ExitCode)

	if !strings.Contains(runErr.Error(), "running") {
		t.Fatalf("expected ErrProcessRunning (process should outlive timeout), got: %v", runErr)
	}
	proc.SetPID(result.PID)

	// Step 2: The Go process doesn't call ctx.Done() anymore — we continue
	// to serve. Simulate parent-exit scenario: close the sandbox/process
	// objects as if the Go program exited. Since *os.File are used directly,
	// the child's output doesn't depend on Go staying alive.
	t.Logf("Server PID: %d, stdout=%s, stderr=%s", proc.PID, proc.StdoutPath, proc.StderrPath)

	// Step 3: Make a request — after our fix, this should work.
	time.Sleep(500 * time.Millisecond) // let server start

	// Simulate what happens after parent Go process exits:
	// We close our end of the files (the Go-end of the fd) to simulate
	// Go process exit. With *os.File direct assignment, the child's fd
	// is the same file opened independently, so this doesn't break.
	t.Log("Simulating Go process exit (close our file handles)...")
	proc.Close() // close our fds to simulate Go process exit
	pm.Remove(proc.ID)

	// Now the server is orphaned with stderr going directly to a file.
	time.Sleep(200 * time.Millisecond)

	t.Log("Making curl request...")
	out, err := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "http://localhost:2345/").CombinedOutput()
	if err != nil {
		t.Logf("curl error: %v (may be expected if server already stopped)", err)
	}
	status := strings.TrimSpace(string(out))
	t.Logf("HTTP status: %s", status)

	if status != "200" {
		t.Errorf("Expected HTTP 200, got %q (Empty reply bug reproduced)", status)
	}

	// Read the stderr file — should have the access log
	stderrContent, err := os.ReadFile(proc.StderrPath)
	if err != nil {
		t.Logf("stderr file not found (expected after pm.Remove): %v", err)
	}
	t.Logf("stderr content: %q", string(stderrContent))

	// Cleanup
	exec.Command("kill", fmt.Sprintf("%d", proc.PID)).Run()
}
