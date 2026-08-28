package wasmhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// ExecDefaultTimeout bounds one exec_command when the plugin does not
// set timeout_ms.
const ExecDefaultTimeout = 120 * time.Second

// ExecMaxTimeout clamps plugin-supplied timeouts. A command must never
// hold a host goroutine for an unbounded period; a plugin that needs
// longer than this is a misconfiguration.
const ExecMaxTimeout = 10 * time.Minute

// maxExecOutputBytes caps one command's stdout and stderr. Unbounded
// output would grow host memory without limit.
const maxExecOutputBytes = 1 << 20 // 1 MiB each

// maxExecGuestResponse bounds the exec response the host serializes into
// the guest heap. The guest heap is 512 MiB (WithMemoryLimitPages(8192)
// = 8192 × 64 KiB) and the plugin's own allocations share it — a
// response near 3 MiB would still be safe, but we reject earlier with a
// specific error to leave headroom for the plugin's own allocations.
const maxExecGuestResponse = 3 << 20 // 3 MiB

// stdExecutor implements Executor via os/exec: argv execution with PATH
// lookup, host environment inheritance, and a process-group kill on
// timeout (children spawned by the command do not survive).
type stdExecutor struct{}

// NewStdExecutor returns the default command executor.
func NewStdExecutor() Executor { return stdExecutor{} }

// Exec runs one command under req. The timeout is clamped to
// [ExecDefaultTimeout, ExecMaxTimeout]; the output cap is 1 MiB per
// stream (excess is truncated and reported as an error).
func (stdExecutor) Exec(ctx context.Context, req ExecRequest) ExecResult {
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = ExecDefaultTimeout
	}
	if timeout > ExecMaxTimeout {
		timeout = ExecMaxTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Cmd, req.Args...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = buildEnv(os.Environ(), req.Env, req.EnvReplace)
	// Run in its own process group so the timeout can kill the whole
	// group — CommandContext's default Cancel only kills the command
	// itself, leaving children (e.g. a `sh -c "sleep 100 | grep x"`
	// spawned by the plugin) behind as orphans. applyProcessGroup +
	// killProcessGroup are platform-specific (Setpgid+Kill on Unix,
	// CREATE_NEW_PROCESS_GROUP+taskkill on Windows).
	applyProcessGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return killProcessGroup(cmd.Process.Pid)
		}
		return nil
	}

	stdoutR, err := cmd.StdoutPipe()
	if err != nil {
		return ExecResult{Err: fmt.Errorf("exec: stdout pipe: %w", err)}
	}
	stderrR, err := cmd.StderrPipe()
	if err != nil {
		return ExecResult{Err: fmt.Errorf("exec: stderr pipe: %w", err)}
	}

	var stdout, stderr []byte
	var outTrunc, errTrunc bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		stdout, outTrunc = readCapped(stdoutR)
	}()
	go func() {
		defer wg.Done()
		stderr, errTrunc = readCapped(stderrR)
	}()

	if err := cmd.Start(); err != nil {
		// Command not found, cwd invalid, permission denied, ...
		// os/exec closes the child pipe ends on Start failure (defer in
		// Cmd.Start), so the readers see EOF and wg.Wait cannot block.
		wg.Wait()
		return ExecResult{Err: err}
	}
	// Wait while the readers drain concurrently; then finish the reads
	// BEFORE interpreting waitErr. os/exec documents: "It is thus
	// incorrect to call Wait before all reads from the pipe have
	// completed." The readers have been consuming since before Start, so
	// the pipes never back up; waiting here just orders the outcome.
	waitErr := cmd.Wait()
	wg.Wait()

	res := ExecResult{
		Stdout: string(stdout),
		Stderr: string(stderr),
	}
	if outTrunc || errTrunc {
		res.Err = fmt.Errorf("exec: output exceeds %d bytes", maxExecOutputBytes)
		return res
	}
	if waitErr != nil {
		// Timeout wins over every other outcome: the process-group kill
		// on ctx expiry surfaces as an ExitError (signal killed) from
		// Wait, not as the ctx error — check the deadline FIRST so the
		// report says "timed out", not "killed by signal".
		if errors.Is(waitErr, context.DeadlineExceeded) || ctx.Err() != nil {
			res.Err = fmt.Errorf("exec: timed out after %s", timeout)
			return res
		}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			// The process ran and terminated with a non-zero status —
			// a business result, not a host error. A signal kill (from
			// outside the timeout path) is reported as an error.
			if st := cmd.ProcessState; st != nil {
				if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					res.Err = fmt.Errorf("exec: killed by signal %v", ws.Signal())
					return res
				}
				res.ExitCode = st.ExitCode()
			}
			return res
		}
		// Some other failure to run to completion: report it, but only
		// after the stream reads above so the output is not lost.
		res.Err = waitErr
		return res
	}
	if st := cmd.ProcessState; st != nil {
		res.ExitCode = st.ExitCode()
	}
	return res
}

// readCapped reads a stream up to maxExecOutputBytes and then DRAINS
// the remainder to EOF: a child that keeps writing into a pipe whose
// reader has given up blocks forever on the full pipe, and cmd.Wait
// never returns. The drain is bounded by the command's timeout — an
// unbounded writer (e.g. `yes`) is killed by the process-group kill.
// Returns the capped bytes and whether the stream exceeded the cap.
func readCapped(r io.Reader) ([]byte, bool) {
	b, _ := io.ReadAll(io.LimitReader(r, maxExecOutputBytes+1))
	if len(b) > maxExecOutputBytes {
		io.Copy(io.Discard, r) // drain — the child must not block
		return b[:maxExecOutputBytes], true
	}
	return b, false
}

// buildEnv constructs the child process environment.
//
// When replace is true, overrides is the *complete* environment — the
// host process environment (base) is NOT merged. This is the correct
// mode for a minimal allowlist that excludes host secrets (API keys,
// tokens): the child process sees only what the caller explicitly passes.
//
// When replace is false, overrides overlays base: every inherited
// variable stays unless overridden (Go's exec.Cmd.Env REPLACES the
// environment when non-nil, so the merge is done here — a plugin adding
// one variable must not lose PATH/HOME).
func buildEnv(base []string, overrides map[string]string, replace bool) []string {
	if replace {
		out := make([]string, 0, len(overrides))
		for key, v := range overrides {
			out = append(out, key+"="+v)
		}
		return out
	}
	if len(overrides) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key, _, ok := cutEnv(kv)
		if !ok {
			continue
		}
		if v, overridden := overrides[key]; overridden {
			out = append(out, key+"="+v)
			continue
		}
		out = append(out, kv)
	}
	for key, v := range overrides {
		if !hasEnv(out, key) {
			out = append(out, key+"="+v)
		}
	}
	return out
}

func cutEnv(kv string) (key, value string, ok bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}

func hasEnv(env []string, key string) bool {
	for _, kv := range env {
		if k, _, ok := cutEnv(kv); ok && k == key {
			return true
		}
	}
	return false
}
