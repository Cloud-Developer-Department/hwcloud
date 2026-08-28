package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// AcquireChannelLock must be exclusive per machine, independent of the
// process working directory, and re-acquirable after release.
func TestChannelLockExclusive(t *testing.T) {
	isolateConfig(t)
	lockPath := filepath.Join(channelDir("feishu"), "feishu.lock")

	l1, err := AcquireChannelLock("feishu")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l1.Release()

	// Lock file lives under $profile/channel/feishu/ (never CWD-relative).
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file not under $profile/channel/feishu: %v", err)
	}

	// Second acquire must fail — same profile, same machine.
	if _, err := AcquireChannelLock("feishu"); err == nil {
		t.Fatal("second acquire succeeded; lock not exclusive")
	} else if !strings.Contains(err.Error(), "held by another process") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Different channel name = independent lock.
	l2, err := AcquireChannelLock("slack")
	if err != nil {
		t.Fatalf("different channel lock should be free: %v", err)
	}
	l2.Release()

	// After release the lock is re-acquirable.
	l1.Release()
	l3, err := AcquireChannelLock("feishu")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	l3.Release()

	// The lock file records the holding PID for diagnostics.
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid != os.Getpid() {
		t.Fatalf("lock pid = %q (err %v), want %d", strings.TrimSpace(string(data)), err, os.Getpid())
	}
}
