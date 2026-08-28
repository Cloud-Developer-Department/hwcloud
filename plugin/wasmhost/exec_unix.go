//go:build !windows

package wasmhost

import (
	"os/exec"
	"syscall"
)

// applyProcessGroup configures the child to run in its own process group
// (setsid-equivalent via Setpgid) so a timeout can signal the whole group
// and kill children the command spawned (e.g. `sh -c "sleep 100 | grep x"`).
// CommandContext's default Cancel only kills the direct child, leaving
// grandchildren as orphans.
func applyProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGKILL to the whole process group identified by
// pid (the negative pid in syscall.Kill addresses the group, not a single
// process). Children spawned by the command die with it.
func killProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
