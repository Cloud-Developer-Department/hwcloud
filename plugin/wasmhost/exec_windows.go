//go:build windows

package wasmhost

import (
	"fmt"
	"os/exec"
	"syscall"
)

// CREATE_NEW_PROCESS_GROUP is the Windows equivalent of Unix's Setpgid:
// the child becomes the root of a new process group. A Ctrl-Break /
// taskkill /T targeting it reaches its descendants.
const createNewProcessGroup = 0x00000200

// applyProcessGroup configures the child to start a new process group via
// CREATE_NEW_PROCESS_GROUP. This is the Windows analogue of Unix
// Setpgid — without it, a timeout can only terminate the direct child,
// leaving grandchildren (e.g. `sh -c "sleep 100 | grep x"`) as orphans.
func applyProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
}

// killProcessGroup terminates the process tree rooted at pid. Windows has
// no syscall.Kill with negative-pid group semantics; taskkill /T /F walks
// the child tree (the CREATE_NEW_PROCESS_GROUP group is not itself a kill
// target, but /T follows parent→child relationships which is what we need:
// every descendant of the spawned command dies).
func killProcessGroup(pid int) error {
	// taskkill /PID <pid> /T /F: /T = kill tree (children), /F = force.
	kill := exec.Command("taskkill", "/PID", fmt.Sprintf("%d", pid), "/T", "/F")
	// taskkill's own output must not leak to the parent's stdout/stderr —
	// the cancel path runs on timeout, and the plugin's stream readers are
	// already draining the killed child's pipes.
	kill.Stdout = nil
	kill.Stderr = nil
	if err := kill.Run(); err != nil {
		// Fall back to a hard TerminateProcess on the direct child if
		// taskkill is unavailable (e.g. stripped Windows images). The
		// grandchildren are then orphaned, but the direct child dies.
		if h, e := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(pid)); e == nil {
			defer syscall.CloseHandle(h)
			return syscall.TerminateProcess(h, 1)
		}
		return err
	}
	return nil
}
