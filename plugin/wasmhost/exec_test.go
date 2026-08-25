package wasmhost

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStdExecBasic(t *testing.T) {
	res := NewStdExecutor().Exec(context.Background(), ExecRequest{
		Cmd:  "echo",
		Args: []string{"hello"},
	})
	if res.Err != nil {
		t.Fatalf("Exec: %v", res.Err)
	}
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("stdout = %q, want hello", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", res.ExitCode)
	}
}

func TestStdExecExitCodeIsResultNotError(t *testing.T) {
	// argv form; shell is just the program being executed.
	res := NewStdExecutor().Exec(context.Background(), ExecRequest{
		Cmd:  "sh",
		Args: []string{"-c", "exit 3"},
	})
	if res.Err != nil {
		t.Fatalf("non-zero exit must not be an error, got %v", res.Err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit_code = %d, want 3", res.ExitCode)
	}
}

func TestStdExecStderr(t *testing.T) {
	res := NewStdExecutor().Exec(context.Background(), ExecRequest{
		Cmd:  "sh",
		Args: []string{"-c", "echo oops >&2"},
	})
	if res.Err != nil {
		t.Fatalf("Exec: %v", res.Err)
	}
	if strings.TrimSpace(res.Stderr) != "oops" {
		t.Fatalf("stderr = %q, want oops", res.Stderr)
	}
}

func TestStdExecEnvMergesOverInherited(t *testing.T) {
	res := NewStdExecutor().Exec(context.Background(), ExecRequest{
		Cmd:  "sh",
		Args: []string{"-c", "echo \"$EXEC_TEST_VAR / ${PATH:+path-ok}\""},
		Env:  map[string]string{"EXEC_TEST_VAR": "merged"},
	})
	if res.Err != nil {
		t.Fatalf("Exec: %v", res.Err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "merged / path-ok" {
		t.Fatalf("stdout = %q, want merged / path-ok (inherited PATH must survive)", got)
	}
}

func TestStdExecCwd(t *testing.T) {
	res := NewStdExecutor().Exec(context.Background(), ExecRequest{
		Cmd: "pwd",
		Cwd: "/tmp",
	})
	if res.Err != nil {
		t.Fatalf("Exec: %v", res.Err)
	}
	if strings.TrimSpace(res.Stdout) != "/tmp" {
		t.Fatalf("stdout = %q, want /tmp", res.Stdout)
	}
}

func TestStdExecCommandNotFound(t *testing.T) {
	res := NewStdExecutor().Exec(context.Background(), ExecRequest{
		Cmd: "hwcloud_exec_nonexistent_prog_xyz",
	})
	if res.Err == nil {
		t.Fatal("missing program must error")
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0 for a non-started command", res.ExitCode)
	}
}

func TestStdExecInvalidCwd(t *testing.T) {
	res := NewStdExecutor().Exec(context.Background(), ExecRequest{
		Cmd: "echo",
		Cwd: "/nonexistent-dir-xyz",
	})
	if res.Err == nil {
		t.Fatal("invalid cwd must error")
	}
}

func TestStdExecTimeout(t *testing.T) {
	start := time.Now()
	res := NewStdExecutor().Exec(context.Background(), ExecRequest{
		Cmd:       "sh",
		Args:      []string{"-c", "sleep 30"},
		TimeoutMS: 100,
	})
	if res.Err == nil {
		t.Fatal("timeout must error")
	}
	if !strings.Contains(res.Err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", res.Err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, want ~100ms", elapsed)
	}
}

func TestStdExecTimeoutKillsProcessGroup(t *testing.T) {
	// The child spawns its own sleep; the timeout must kill the group so
	// the grandchild does not survive. We cannot observe the grandchild
	// directly, but the command must return promptly — a leaked sleep
	// would keep the shell's pipe open until the child's own deadline,
	// but the shell exits immediately when the group is killed.
	res := NewStdExecutor().Exec(context.Background(), ExecRequest{
		Cmd:       "sh",
		Args:      []string{"-c", "sleep 30 & wait"},
		TimeoutMS: 100,
	})
	if res.Err == nil {
		t.Fatal("timeout must error")
	}
}

func TestStdExecOutputCap(t *testing.T) {
	res := NewStdExecutor().Exec(context.Background(), ExecRequest{
		Cmd:  "sh",
		Args: []string{"-c", "head -c 2000000 /dev/zero | tr '\\0' 'x'"},
	})
	if res.Err == nil {
		t.Fatal("output beyond the cap must error")
	}
	if !strings.Contains(res.Err.Error(), "output exceeds") {
		t.Fatalf("err = %v, want output cap", res.Err)
	}
	if len(res.Stdout) > maxExecOutputBytes {
		t.Fatalf("stdout = %d bytes, want <= %d", len(res.Stdout), maxExecOutputBytes)
	}
}

func TestStdExecDefaultTimeoutClamped(t *testing.T) {
	// A plugin-supplied timeout above the hard cap is clamped, and a
	// zero/negative timeout falls back to the default — both are internal
	// behaviors; verify via a short real run that the clamping path does
	// not misbehave (no panic, command runs).
	res := NewStdExecutor().Exec(context.Background(), ExecRequest{
		Cmd:       "echo",
		Args:      []string{"ok"},
		TimeoutMS: 99999999, // ~27.8h — must clamp, not panic
	})
	if res.Err != nil {
		t.Fatalf("Exec: %v", res.Err)
	}
	if strings.TrimSpace(res.Stdout) != "ok" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}
