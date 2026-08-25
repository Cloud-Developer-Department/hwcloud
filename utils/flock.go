package utils

import (
	"fmt"
	"os"
)

// FileLock is an exclusive advisory lock on a file, backed by flock(2) on
// Unix and LockFileEx on Windows.
//
// On Unix the lock is held by the KERNEL on the open file description, not
// by the file's contents: when the holding process exits — normally, via
// panic, or SIGKILL — the kernel closes the fd and the lock is released
// automatically. There is no stale-lock problem, which is why flock beats
// a PID file for single-instance guarantees. The lock file itself stays
// behind (its PID content is diagnostics only); the lock state lives in
// the kernel.
//
// On Windows the same semantics hold via LockFileEx (exclusive, with the
// file handle closed on process exit releasing the lock).
type FileLock struct {
	f *os.File
}

// AcquireFileLock takes the exclusive lock on path, creating the file
// (0600) if needed. Blocks never; returns an error when another process
// holds the lock. The caller must keep the returned FileLock alive for
// the whole critical section — closing the file releases the lock.
func AcquireFileLock(path string) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("file lock: open %s: %w", path, err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("file lock: %s is held by another process: %w", path, err)
	}
	return &FileLock{f: f}, nil
}

// WritePID records the holding process's PID in the lock file (for
// diagnostics — "cat <lockfile>"). Best effort; failure is not fatal.
func (l *FileLock) WritePID() {
	if l == nil || l.f == nil {
		return
	}
	_, _ = l.f.Seek(0, 0)
	_ = l.f.Truncate(0)
	_, _ = fmt.Fprintf(l.f, "%d\n", os.Getpid())
	_ = l.f.Sync()
}

// Release releases the lock (idempotent).
func (l *FileLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	unlockFile(l.f)
	_ = l.f.Close()
	l.f = nil
}
