package server

import (
	"os"
	"path/filepath"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/utils"
)

// ChannelLock is the machine-level exclusive lock for a named IM channel
// (feishu, ...). Feishu's WebSocket keeps ONE active connection per app —
// a second instance would silently steal events from the first (the old
// connection stops receiving with no error, looking "up" but dead). The
// lock covers the whole channel lifecycle (credential registration +
// WebSocket), so two concurrently started servers cannot race the shared
// credential file or the connection.
//
// The lock file lives under config.Dir()/channel/<name>/, never CWD:
// the server may be started from
// any directory, and the lock is the same one regardless. Different
// config dirs = different locks = independent channel instances (the
// multi-bot deployment model). The underlying flock is released by the
// kernel when the process dies (even a crash), so there is no
// stale-lock problem.
type ChannelLock struct {
	*utils.FileLock
}

// AcquireChannelLock takes the machine-level lock for the named channel
// under config.Dir()/channel/. Returns an error when another
// instance holds it.
func AcquireChannelLock(name string) (*ChannelLock, error) {
	p := filepath.Join(channelDir(name), name+".lock")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	l, err := utils.AcquireFileLock(p)
	if err != nil {
		return nil, err
	}
	l.WritePID()
	return &ChannelLock{FileLock: l}, nil
}

// AcquireChannelLockRetry is AcquireChannelLock with a bounded retry
// window.
//
// Why the retry: a Disconnect that times out abandoning a stuck
// connection goroutine (disconnectTimeout) returns while that goroutine
// still holds the flock — it releases it when the SDK's shutdown
// finally completes, usually within a second or two. An immediate
// reconnect from the frontend would otherwise fail spuriously with
// "lock held by another process". Retrying for the window absorbs that
// handoff; a lock genuinely held by another LIVE process still fails
// after the window (it never releases).
//
// The handoff is race-free: the old goroutine releases ITS lock only
// when it exits (the connection is dead by then), so the new connect
// can never double-run against a live connection.
func AcquireChannelLockRetry(name string, timeout time.Duration) (*ChannelLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		lock, err := AcquireChannelLock(name)
		if err == nil {
			return lock, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}
