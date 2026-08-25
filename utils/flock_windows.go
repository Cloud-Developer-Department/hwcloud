//go:build windows

package utils

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive, non-blocking advisory lock via
// LockFileEx. Windows has no flock(2); LockFileEx on an open file
// handle is the equivalent advisory (not mandatory) byte-range lock.
// A zero-length range [0,0) locks the whole file per the Windows
// convention. The lock is released when the handle is closed, so
// process exit (normal or crash) releases it automatically — same
// auto-release guarantee as Unix flock.
func lockFile(f *os.File) error {
	var ol windows.Overlapped
	// lengthLow/lengthHigh = 0 → [0,0) → whole-file lock (Windows
	// convention for "lock the entire file").
	if err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, // reserved
		0, 0, // low/high 32 bits of the 64-bit length (0 = whole file)
		&ol); err != nil {
		return err
	}
	return nil
}

// unlockFile releases the LockFileEx-held byte range via UnlockFileEx.
func unlockFile(f *os.File) {
	var ol windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 0, 0, &ol)
}
