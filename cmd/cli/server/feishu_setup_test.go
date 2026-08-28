package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/kernel"

	clirest "github.com/Cloud-Developer-Department/hwcloud/cmd/cli/rest"
)

// isolateSettings points the settings file at a temp directory via
// HWCLOUD_CLI_CONFIG so real user settings are never touched.
func isolateSettings(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("HWCLOUD_CLI_CONFIG", p)
	return p
}

// isolateProfiles points profile resolution at a temp directory: CWD is
// moved (resolveProfilesDir prefers $(pwd)/profiles) and HOME is set so
// the fallback lands in the same sandbox. Real credentials are never
// touched.
func isolateConfig(t *testing.T) {
	t.Helper()
	// Point the config directory at a temp dir: every persistent path
	// (profile, plugins, channel state) derives from HWCLOUD_CLI_CONFIG.
	t.Setenv("HWCLOUD_CLI_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
}

// saveFeishuToSettings must preserve every other settings field (user
// settings, unknown future fields) and write atomically.
func TestSaveFeishuToSettingsPreservesOtherFields(t *testing.T) {
	p := isolateSettings(t)
	existing := map[string]any{
		"provider":             map[string]any{"openai": map[string]any{"api_key": "sk-old"}},
		"unknown_future_field": map[string]any{"keep": "me"},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := saveFeishuToSettings("cli_new", "secret_new"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings not valid JSON: %v", err)
	}
	// Unknown / unrelated fields survive (valid JSON, same content).
	var unknown map[string]string
	if err := json.Unmarshal(got["unknown_future_field"], &unknown); err != nil {
		t.Fatalf("unknown field mangled: %s (%v)", got["unknown_future_field"], err)
	}
	if unknown["keep"] != "me" {
		t.Fatalf("unknown field content lost: %+v", unknown)
	}
	if !json.Valid(got["provider"]) {
		t.Fatalf("provider field mangled: %s", got["provider"])
	}
	// channels.feishu carries the new credentials.
	var channels map[string]struct {
		AppID     string `json:"app_id"`
		AppSecret string `json:"app_secret"`
	}
	if err := json.Unmarshal(got["channels"], &channels); err != nil {
		t.Fatalf("channels not valid: %v", err)
	}
	feishu, ok := channels["feishu"]
	if !ok {
		t.Fatalf("channels.feishu missing: %+v", channels)
	}
	if feishu.AppID != "cli_new" || feishu.AppSecret != "secret_new" {
		t.Fatalf("feishu = %+v", feishu)
	}
}

// Creating the settings file from scratch works too.
// A lock released by a dying goroutine (disconnect timeout abandon)
// must be re-acquirable by an immediate reconnect — the retry window
// absorbs the handoff.
func TestAcquireChannelLockRetryHandsOff(t *testing.T) {
	t.Setenv("HWCLOUD_CLI_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	held, err := AcquireChannelLock("feishu")
	if err != nil {
		t.Fatal(err)
	}
	// The retry must NOT succeed while the lock is held.
	done := make(chan error, 1)
	go func() {
		_, err := AcquireChannelLockRetry("feishu", 300*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("acquired while lock held")
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not finish")
	}

	// Release and retry again with a longer window: succeeds.
	held.Release()
	if _, err := AcquireChannelLockRetry("feishu", 2*time.Second); err != nil {
		t.Fatalf("retry after release failed: %v", err)
	}
}

// A lock held by a live holder still fails after the window.
func TestAcquireChannelLockRetryFailsWhileHeld(t *testing.T) {
	t.Setenv("HWCLOUD_CLI_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	held, err := AcquireChannelLock("feishu")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	start := time.Now()
	if _, err := AcquireChannelLockRetry("feishu", 300*time.Millisecond); err == nil {
		t.Fatal("acquired while held")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("failed too fast: %v", elapsed)
	}
}

func TestSaveFeishuToSettingsCreatesFile(t *testing.T) {
	p := isolateSettings(t)
	if err := saveFeishuToSettings("cli_fresh", "secret_fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("settings not created: %v", err)
	}
}

// Concurrent submissions must not lose updates (read-modify-write
// serialized by the package mutex).
func TestSaveFeishuToSettingsConcurrent(t *testing.T) {
	p := isolateSettings(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := saveFeishuToSettings("cli_keep", "secret_keep"); err != nil {
				t.Errorf("save %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// The file must be valid JSON after the concurrent storm (no torn
	// writes / interleaved cycles).
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("settings corrupt after concurrent saves: %s", raw)
	}
}

// ClearCredentials removes channels.feishu while preserving every other
// settings field (and drops the channels key when nothing remains).
func TestClearCredentialsPreservesOtherFields(t *testing.T) {
	p := isolateSettings(t)
	existing := map[string]any{
		"provider": map[string]any{"openai": map[string]any{"api_key": "sk-old"}},
		"channels": map[string]any{
			"feishu": map[string]string{"app_id": "cli_old", "app_secret": "secret_old"},
			"slack":  map[string]string{"token": "xoxb"},
		},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := saveFeishuToSettings("cli_new", "secret_new"); err != nil {
		t.Fatal(err)
	}
	// Now clear via the manager path used by the DELETE endpoint.
	m := NewFeishuManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	if err := m.ClearCredentials(); err != nil {
		t.Fatal(err)
	}
	if id, _ := m.Credentials(); id != "" {
		t.Fatalf("in-memory credentials not cleared: %q", id)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings corrupt: %v", err)
	}
	// feishu gone, slack preserved.
	var channels map[string]json.RawMessage
	if err := json.Unmarshal(got["channels"], &channels); err != nil {
		t.Fatal(err)
	}
	if _, ok := channels["feishu"]; ok {
		t.Fatal("channels.feishu not removed")
	}
	if _, ok := channels["slack"]; !ok {
		t.Fatal("channels.slack lost")
	}
	// Unrelated fields survive.
	if !json.Valid(got["provider"]) {
		t.Fatal("provider mangled")
	}
}

// The QR cache persists URL, image, and the absolute expiry — the
// frontend re-fetches it after a refresh and restarts its countdown from
// the remaining time (GET /qr returns expires_in), not the original
// total.
func TestFeishuQRCacheRoundTrip(t *testing.T) {
	t.Setenv("HWCLOUD_CLI_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	m := NewFeishuManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)

	if err := saveFeishuQR("https://qr.example/x", 300); err != nil {
		t.Fatal(err)
	}
	url, img, expireIn := m.QR()
	if url != "https://qr.example/x" {
		t.Fatalf("url = %q", url)
	}
	if img == "" {
		t.Fatal("image empty")
	}
	// Remaining lifetime: at most the total, and > 0 (a fresh cache).
	if expireIn <= 0 || expireIn > 300 {
		t.Fatalf("expireIn = %d, want 0 < x <= 300", expireIn)
	}
	// The cached expiry is absolute — a re-read must never see MORE
	// remaining time than the first read (it would mean the countdown
	// restarted from the total). Same-second reads may be equal (Unix
	// second granularity); the refresh-page case reads later and sees
	// strictly less.
	if _, _, again := m.QR(); again > expireIn {
		t.Fatalf("second read grew: %d > %d", again, expireIn)
	}

	clearFeishuQR()
	if url, _, expireIn := m.QR(); url != "" || expireIn != 0 {
		t.Fatalf("cache not cleared: url=%q expireIn=%d", url, expireIn)
	}
}

// An expired QR reports expireIn 0 — the frontend stops counting down
// and asks the user to re-register instead of waiting for the SDK poll
// to surface the expiry on its next cycle.
func TestFeishuQRExpired(t *testing.T) {
	t.Setenv("HWCLOUD_CLI_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	m := NewFeishuManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)

	if err := saveFeishuQR("https://qr.example/x", 300); err != nil {
		t.Fatal(err)
	}
	_, _, expiresAtPath := feishuQRPath()
	if err := os.WriteFile(expiresAtPath, []byte(strconv.FormatInt(time.Now().Unix()-1, 10)), 0o600); err != nil {
		t.Fatal(err)
	}

	url, _, expireIn := m.QR()
	if url == "" {
		t.Fatal("url lost")
	}
	if expireIn != 0 {
		t.Fatalf("expireIn = %d, want 0", expireIn)
	}
}

// A disconnect whose flow is stuck (SDK poll body read — context does
// not cancel it, and the SDK polls without a client timeout) must return
// within disconnectTimeout instead of hanging the endpoint.
func TestDisconnectTimesOutOnStuckFlow(t *testing.T) {
	m := NewFeishuManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	cancelled := make(chan struct{})
	m.mu.Lock()
	m.cancel = func() { close(cancelled) }
	m.done = make(chan struct{}) // never closes — simulates a stuck poll
	m.mu.Unlock()

	start := time.Now()
	m.Disconnect()
	if elapsed := time.Since(start); elapsed >= disconnectTimeout+2*time.Second {
		t.Fatalf("Disconnect hung: %v", elapsed)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("cancel not invoked")
	}
	if s := m.Status(); s.Phase != clirest.FeishuDisconnected || s.LastError != "" {
		t.Fatalf("status = %+v", s)
	}
}

// setStatus guards: a publish from a flow that no longer owns the
// manager (a disconnect timeout abandoned it) is dropped, while the
// current owner publishes normally. Without the guard, a stale
// registration goroutine finishing late would clobber the state of a
// newer connection.
func TestSetStatusGuardDropsStalePublish(t *testing.T) {
	m := NewFeishuManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	current := make(chan struct{})
	m.mu.Lock()
	m.done = current
	m.mu.Unlock()

	stale := make(chan struct{})
	m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuConnected}, stale)
	if s := m.Status(); s.Phase != clirest.FeishuIdle {
		t.Fatalf("stale publish got through: %+v", s)
	}
	m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuConnected}, current)
	if s := m.Status(); s.Phase != clirest.FeishuConnected {
		t.Fatalf("current owner publish dropped: %+v", s)
	}
}

// A flow's own terminal publish must not be dropped by its cleanup: the
// goroutine tail publishes the terminal status with the fields still set
// (so the guard passes), and only then clears them. Publishing after the
// cleanup (the buggy ordering) makes the guard see m.done != guard and
// drop the publish, leaving the status stuck on the previous phase.
func TestFlowTerminalPublishSurvivesOwnCleanup(t *testing.T) {
	m := NewFeishuManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	done := make(chan struct{})
	m.mu.Lock()
	m.done = done
	m.mu.Unlock()

	// Flow tail, publish-first ordering.
	m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuDisconnected}, done)
	m.mu.Lock()
	if m.done == done {
		m.done = nil
	}
	m.mu.Unlock()

	if s := m.Status(); s.Phase != clirest.FeishuDisconnected {
		t.Fatalf("terminal publish dropped by own cleanup: %+v", s)
	}
}
