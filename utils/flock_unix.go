//go:build !windows

package utils

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive, non-blocking advisory lock on the open
// file description via flock(2). The lock is kernel-held on the fd, so
// process exit (normal, panic, or SIGKILL) releases it automatically.
func lockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

// unlockFile releases the advisory lock (the close in Release also
// releases it, but an explicit unlock keeps the intent readable and
// matches the Windows path where UnlockFileEx is required).
func unlockFile(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
