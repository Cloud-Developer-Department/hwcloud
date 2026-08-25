package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/kernel"

	clirest "github.com/Cloud-Developer-Department/hwcloud/cmd/cli/rest"
)

// saveWecomToSettings preserves every other settings field and channel.
func TestSaveWecomToSettingsPreservesOtherFields(t *testing.T) {
	p := isolateSettings(t)
	existing := map[string]any{
		"provider": map[string]any{"openai": map[string]any{"api_key": "sk-old"}},
		"channels": map[string]any{"feishu": map[string]string{"app_id": "cli_old"}},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := saveWecomToSettings(WecomCredentials{BotID: "bot-new", Secret: "sec-new"}); err != nil {
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
	var channels map[string]json.RawMessage
	if err := json.Unmarshal(got["channels"], &channels); err != nil {
		t.Fatal(err)
	}
	if _, ok := channels["feishu"]; !ok {
		t.Fatal("channels.feishu lost")
	}
	var wc struct {
		BotID  string `json:"bot_id"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(channels["wecom"], &wc); err != nil {
		t.Fatalf("channels.wecom invalid: %v", err)
	}
	if wc.BotID != "bot-new" || wc.Secret != "sec-new" {
		t.Fatalf("wecom = %+v", wc)
	}
}

// ClearCredentials removes channels.wecom while preserving other
// channels and settings fields.
func TestClearWecomCredentialsPreservesOtherFields(t *testing.T) {
	p := isolateSettings(t)
	existing := map[string]any{
		"provider": map[string]any{"openai": map[string]any{"api_key": "sk-old"}},
		"channels": map[string]any{
			"wecom":  map[string]string{"bot_id": "bot-old"},
			"feishu": map[string]string{"app_id": "cli_old"},
		},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := saveWecomToSettings(WecomCredentials{BotID: "bot-new", Secret: "sec-new"}); err != nil {
		t.Fatal(err)
	}
	m := NewWecomManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
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
		t.Fatal(err)
	}
	var channels map[string]json.RawMessage
	if err := json.Unmarshal(got["channels"], &channels); err != nil {
		t.Fatal(err)
	}
	if _, ok := channels["wecom"]; ok {
		t.Fatal("channels.wecom not removed")
	}
	if _, ok := channels["feishu"]; !ok {
		t.Fatal("channels.feishu lost")
	}
}

// The QR cache round-trips URL, image, and absolute expiry.
func TestWecomQRCacheRoundTrip(t *testing.T) {
	t.Setenv("HWCLOUD_CLI_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	m := NewWecomManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)

	if err := saveWecomQR("https://auth.example/x", 300); err != nil {
		t.Fatal(err)
	}
	url, img, expireIn := m.QR()
	if url != "https://auth.example/x" {
		t.Fatalf("url = %q", url)
	}
	if img == "" {
		t.Fatal("image empty")
	}
	if expireIn <= 0 || expireIn > 300 {
		t.Fatalf("expireIn = %d, want 0 < x <= 300", expireIn)
	}
	if _, _, again := m.QR(); again > expireIn {
		t.Fatalf("second read grew: %d > %d", again, expireIn)
	}

	clearWecomQR()
	if url, _, expireIn := m.QR(); url != "" || expireIn != 0 {
		t.Fatalf("cache not cleared: url=%q expireIn=%d", url, expireIn)
	}
}

// An expired QR reports expireIn 0.
func TestWecomQRExpired(t *testing.T) {
	t.Setenv("HWCLOUD_CLI_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	m := NewWecomManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)

	if err := saveWecomQR("https://auth.example/x", 300); err != nil {
		t.Fatal(err)
	}
	_, _, expiresAtPath := wecomQRPath()
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

// SetCredentials is rejected while QR authorization is in flight.
func TestWecomSetCredentialsRejectedWhileRegistering(t *testing.T) {
	m := NewWecomManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	m.mu.Lock()
	m.status = clirest.WecomStatus{Phase: clirest.WecomRegistering}
	m.mu.Unlock()

	if err := m.SetCredentials("bot-1", "sec-1"); err != clirest.ErrWecomRegistrationInFlight {
		t.Fatalf("err = %v, want ErrWecomRegistrationInFlight", err)
	}
}

// ── Guard machinery (ported from the feishu tests — the mechanism is a
// line-for-line copy and must stay green) ──

// A disconnect whose flow is stuck must return within disconnectTimeout.
func TestWecomDisconnectTimesOutOnStuckFlow(t *testing.T) {
	m := NewWecomManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	cancelled := make(chan struct{})
	m.mu.Lock()
	m.cancel = func() { close(cancelled) }
	m.done = make(chan struct{}) // never closes — simulates a stuck flow
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
	if s := m.Status(); s.Phase != clirest.WecomDisconnected || s.LastError != "" {
		t.Fatalf("status = %+v", s)
	}
}

// setStatus guards: stale publishes are dropped, the current owner's go
// through.
func TestWecomSetStatusGuardDropsStalePublish(t *testing.T) {
	m := NewWecomManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	current := make(chan struct{})
	m.mu.Lock()
	m.done = current
	m.mu.Unlock()

	stale := make(chan struct{})
	m.setStatus(clirest.WecomStatus{Phase: clirest.WecomConnected}, stale)
	if s := m.Status(); s.Phase != clirest.WecomIdle {
		t.Fatalf("stale publish got through: %+v", s)
	}
	m.setStatus(clirest.WecomStatus{Phase: clirest.WecomConnected}, current)
	if s := m.Status(); s.Phase != clirest.WecomConnected {
		t.Fatalf("current owner publish dropped: %+v", s)
	}
}

// A flow's own terminal publish must not be dropped by its cleanup.
func TestWecomFlowTerminalPublishSurvivesOwnCleanup(t *testing.T) {
	m := NewWecomManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	done := make(chan struct{})
	m.mu.Lock()
	m.done = done
	m.mu.Unlock()

	m.setStatus(clirest.WecomStatus{Phase: clirest.WecomDisconnected}, done)
	m.mu.Lock()
	if m.done == done {
		m.done = nil
	}
	m.mu.Unlock()

	if s := m.Status(); s.Phase != clirest.WecomDisconnected {
		t.Fatalf("terminal publish dropped by own cleanup: %+v", s)
	}
}
