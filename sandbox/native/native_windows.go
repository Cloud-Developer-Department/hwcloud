package native

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// Windows sandbox is NYI. Commands run unconfined with a warning.
// Full implementation would use Restricted Token + Job Object.
// See: CreateRestrictedToken, CreateJobObject, AssignProcessToJobObject.

func (s *Sandbox) confineAndRun(ctx context.Context, cmd hwcloud.Command) (hwcloud.Result, error) {
	c := exec.Command(cmd.Program, cmd.Args...)
	c.Dir = s.workDir
	for _, e := range cmd.Env {
		c.Env = append(c.Env, e)
	}
	if cmd.Stdin != "" {
		c.Stdin = strings.NewReader(cmd.Stdin)
	}

	var stdout, stderr strings.Builder
	outW := io.Writer(&stdout)
	errW := io.Writer(&stderr)
	if cmd.StdoutW != nil {
		outW = io.MultiWriter(&stdout, cmd.StdoutW)
	}
	if cmd.StderrW != nil {
		errW = io.MultiWriter(&stderr, cmd.StderrW)
	}
	c.Stdout = outW
	c.Stderr = errW

	if err := c.Start(); err != nil {
		return hwcloud.Result{}, fmt.Errorf("native sandbox (windows): %w", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- c.Wait() }()

	select {
	case <-ctx.Done():
		return hwcloud.Result{
			Stdout:   stdout.String(),
			Stderr:   stderr.String() + "\n[warning: windows sandbox not yet implemented, running unconfined]",
			ExitCode: -1,
			PID:      c.Process.Pid,
		}, hwcloud.ErrProcessRunning
	case err := <-waitCh:
		result := hwcloud.Result{
			Stdout: stdout.String(),
			Stderr: stderr.String() + "\n[warning: windows sandbox not yet implemented, running unconfined]",
			PID:    c.Process.Pid,
		}
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				result.ExitCode = exitErr.ExitCode()
				return result, nil
			}
			return result, fmt.Errorf("native sandbox (windows): %w", err)
		}
		return result, nil
	}
}

func (s *Sandbox) confineAndRunStream(ctx context.Context, cmd *hwcloud.Command) <-chan hwcloud.ToolStreamChunk {
	ch := make(chan hwcloud.ToolStreamChunk, 1)
	go func() {
		defer close(ch)
		result, err := s.confineAndRun(ctx, *cmd)
		if err != nil {
			ch <- hwcloud.ToolStreamChunk{Error: err}
		} else {
			ch <- hwcloud.ToolStreamChunk{Content: result.Stdout + result.Stderr}
		}
	}()
	return ch
}
