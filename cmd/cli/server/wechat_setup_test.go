package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/session"

	clirest "github.com/Cloud-Developer-Department/hwcloud/cmd/cli/rest"
)

// saveWechatToSettings must preserve every other settings field (user
// settings, unknown future fields, other channels) and write atomically.
func TestSaveWechatToSettingsPreservesOtherFields(t *testing.T) {
	p := isolateSettings(t)
	existing := map[string]any{
		"provider":             map[string]any{"openai": map[string]any{"api_key": "sk-old"}},
		"unknown_future_field": map[string]any{"keep": "me"},
		"channels":             map[string]any{"feishu": map[string]string{"app_id": "cli_old"}},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	creds := WechatCredentials{Token: "tok-new", BaseURL: "https://ilinkai.example", AccountID: "bot-new", UserID: "user-new"}
	if err := saveWechatToSettings(creds); err != nil {
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
	// channels.feishu preserved, channels.wechat added.
	var channels map[string]json.RawMessage
	if err := json.Unmarshal(got["channels"], &channels); err != nil {
		t.Fatal(err)
	}
	if _, ok := channels["feishu"]; !ok {
		t.Fatal("channels.feishu lost")
	}
	var wx struct {
		Token     string `json:"token"`
		BaseURL   string `json:"base_url"`
		AccountID string `json:"account_id"`
		UserID    string `json:"user_id"`
	}
	if err := json.Unmarshal(channels["wechat"], &wx); err != nil {
		t.Fatalf("channels.wechat invalid: %v", err)
	}
	if wx.Token != "tok-new" || wx.BaseURL != "https://ilinkai.example" || wx.AccountID != "bot-new" || wx.UserID != "user-new" {
		t.Fatalf("wechat = %+v", wx)
	}
}

// Concurrent submissions must not lose updates.
func TestSaveWechatToSettingsConcurrent(t *testing.T) {
	isolateSettings(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := saveWechatToSettings(WechatCredentials{Token: "tok-keep"}); err != nil {
				t.Errorf("save %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	p := os.Getenv("HWCLOUD_CLI_CONFIG")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("settings corrupt after concurrent saves: %s", raw)
	}
}

// ClearCredentials removes channels.wechat while preserving other
// channels and settings fields.
func TestClearWechatCredentialsPreservesOtherFields(t *testing.T) {
	p := isolateSettings(t)
	existing := map[string]any{
		"provider": map[string]any{"openai": map[string]any{"api_key": "sk-old"}},
		"channels": map[string]any{
			"wechat": map[string]string{"token": "tok-old"},
			"feishu": map[string]string{"app_id": "cli_old"},
		},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := saveWechatToSettings(WechatCredentials{Token: "tok-new"}); err != nil {
		t.Fatal(err)
	}
	m := NewWechatManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	if err := m.ClearCredentials(); err != nil {
		t.Fatal(err)
	}
	if tok, _, _, _ := m.Credentials(); tok != "" {
		t.Fatalf("in-memory credentials not cleared: %q", tok)
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
	if _, ok := channels["wechat"]; ok {
		t.Fatal("channels.wechat not removed")
	}
	if _, ok := channels["feishu"]; !ok {
		t.Fatal("channels.feishu lost")
	}
	if !json.Valid(got["provider"]) {
		t.Fatal("provider mangled")
	}
}

// The QR cache round-trips URL, image, and absolute expiry; a re-read
// never grows the remaining lifetime.
func TestWechatQRCacheRoundTrip(t *testing.T) {
	t.Setenv("HWCLOUD_CLI_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	m := NewWechatManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)

	if err := saveWechatQR("https://qr.example/x", 300); err != nil {
		t.Fatal(err)
	}
	url, img, expireIn, _, _, _ := m.QR()
	if url != "https://qr.example/x" {
		t.Fatalf("url = %q", url)
	}
	if img == "" {
		t.Fatal("image empty")
	}
	if expireIn <= 0 || expireIn > 300 {
		t.Fatalf("expireIn = %d, want 0 < x <= 300", expireIn)
	}
	if _, _, again, _, _, _ := m.QR(); again > expireIn {
		t.Fatalf("second read grew: %d > %d", again, expireIn)
	}

	clearWechatQR()
	if url, _, expireIn, _, _, _ := m.QR(); url != "" || expireIn != 0 {
		t.Fatalf("cache not cleared: url=%q expireIn=%d", url, expireIn)
	}
}

// An expired QR reports expireIn 0.
func TestWechatQRExpired(t *testing.T) {
	t.Setenv("HWCLOUD_CLI_CONFIG", filepath.Join(t.TempDir(), "settings.json"))
	m := NewWechatManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)

	if err := saveWechatQR("https://qr.example/x", 300); err != nil {
		t.Fatal(err)
	}
	_, _, expiresAtPath := wechatQRPath()
	if err := os.WriteFile(expiresAtPath, []byte(strconv.FormatInt(time.Now().Unix()-1, 10)), 0o600); err != nil {
		t.Fatal(err)
	}

	url, _, expireIn, _, _, _ := m.QR()
	if url == "" {
		t.Fatal("url lost")
	}
	if expireIn != 0 {
		t.Fatalf("expireIn = %d, want 0", expireIn)
	}
}

// SubmitVerifyCode is rejected when no registration is waiting.
func TestWechatSubmitVerifyCodeNotPending(t *testing.T) {
	m := NewWechatManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	if err := m.SubmitVerifyCode("123456"); err != clirest.ErrWechatVerifyCodeNotPending {
		t.Fatalf("err = %v, want ErrWechatVerifyCodeNotPending", err)
	}
}

// A SubmitVerifyCode racing a registration failure (which nil-ifies the
// channel) must fail with the typed error — never panic on a send to a
// closed channel. The channel is cleared, not closed (see the failure
// path comment in ConnectAsync).
func TestWechatSubmitVerifyCodeRacingFailureNoPanic(t *testing.T) {
	m := NewWechatManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	m.mu.Lock()
	m.status = clirest.WechatStatus{Phase: clirest.WechatRegistering}
	m.verifyCodeCh = make(chan string, 1)
	m.mu.Unlock()

	// Registration failure path: nil-ify the channel (never close).
	m.mu.Lock()
	m.verifyCodeCh = nil
	m.status = clirest.WechatStatus{Phase: clirest.WechatDisconnected}
	m.mu.Unlock()

	// The submission must return the typed error, not panic.
	if err := m.SubmitVerifyCode("123456"); err != clirest.ErrWechatVerifyCodeNotPending {
		t.Fatalf("err = %v, want ErrWechatVerifyCodeNotPending", err)
	}
}

// waitVerifyCode must unblock immediately when the login context is
// cancelled (a disconnect) — otherwise the registration goroutine holds
// the flock for the full verifyCodeTimeout and reconnects keep failing.
func TestWechatWaitVerifyCodeCancels(t *testing.T) {
	m := NewWechatManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	m.mu.Lock()
	m.status = clirest.WechatStatus{Phase: clirest.WechatRegistering}
	m.verifyCodeCh = make(chan string)
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.waitVerifyCode(ctx, false)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let it block on the channel
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitVerifyCode did not unblock on context cancel")
	}
}

// A pairing code submitted to an in-flight registration is delivered.
func TestWechatSubmitVerifyCodeDelivered(t *testing.T) {
	m := NewWechatManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	m.mu.Lock()
	m.status = clirest.WechatStatus{Phase: clirest.WechatRegistering}
	m.verifyCodeCh = make(chan string, 1)
	m.mu.Unlock()

	done := make(chan string, 1)
	go func() {
		code, err := m.waitVerifyCode(context.Background(), false)
		if err != nil {
			done <- "err: " + err.Error()
			return
		}
		done <- code
	}()
	// Give the goroutine time to block on the channel.
	time.Sleep(50 * time.Millisecond)
	if err := m.SubmitVerifyCode("123456"); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != "123456" {
			t.Fatalf("code = %q", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("verify code not delivered")
	}
}

// ── Channel session meta ──

// fakeMetaStore is a minimal in-memory session.Store for meta tests.
type fakeMetaStore struct {
	sessions map[string]session.SessionInfo
}

func newFakeMetaStore() *fakeMetaStore { return &fakeMetaStore{sessions: map[string]session.SessionInfo{}} }

func (f *fakeMetaStore) Save(ctx context.Context, info session.SessionInfo) error {
	f.sessions[info.ID] = info
	return nil
}
func (f *fakeMetaStore) Get(ctx context.Context, id string) (*session.SessionInfo, error) {
	if s, ok := f.sessions[id]; ok {
		return &s, nil
	}
	return nil, nil
}
func (f *fakeMetaStore) List(ctx context.Context) ([]session.SessionInfo, error) {
	out := make([]session.SessionInfo, 0, len(f.sessions))
	for _, s := range f.sessions {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeMetaStore) Delete(ctx context.Context, id string) error {
	delete(f.sessions, id)
	return nil
}
func (f *fakeMetaStore) Close() error { return nil }

// ensureChannelMeta tags a channel session with "_channel" once; a
// second call is a no-op, and a nil store is a no-op.
func TestEnsureChannelMeta(t *testing.T) {
	store := newFakeMetaStore()

	ensureChannelMeta(store, "wechat_user1", "wechat", "第一条消息")
	info, err := store.Get(context.Background(), "wechat_user1")
	if err != nil || info == nil {
		t.Fatalf("session not created: %v", err)
	}
	if v, ok := session.GetMeta[string](*info, "_channel"); !ok || v != "wechat" {
		t.Fatalf("_channel meta = %q, %v", v, ok)
	}
	if info.Title != "第一条消息" {
		t.Fatalf("title = %q, want first message", info.Title)
	}

	// Second call must not overwrite (idempotent) — even with a new
	// message.
	ensureChannelMeta(store, "wechat_user1", "wechat", "第二条消息")
	list, _ := store.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("session duplicated: %d", len(list))
	}
	// Title stays the FIRST message (idempotent, like ACP).
	info, _ = store.Get(context.Background(), "wechat_user1")
	if info.Title != "第一条消息" {
		t.Fatalf("title overwritten: %q", info.Title)
	}

	// Nil store is a silent no-op.
	ensureChannelMeta(nil, "wechat_user2", "wechat", "x")
	if _, err := store.Get(context.Background(), "wechat_user2"); err != nil || len(store.sessions) != 1 {
		t.Fatalf("nil store wrote something: %v", err)
	}
}

// ── Guard machinery (ported from the feishu tests — the mechanism is a
// line-for-line copy and must stay green) ──

// A disconnect whose flow is stuck must return within disconnectTimeout
// instead of hanging the endpoint.
func TestWechatDisconnectTimesOutOnStuckFlow(t *testing.T) {
	m := NewWechatManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
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
	if s := m.Status(); s.Phase != clirest.WechatDisconnected || s.LastError != "" {
		t.Fatalf("status = %+v", s)
	}
}

// setStatus guards: a stale flow's publish is dropped, the current
// owner's goes through.
func TestWechatSetStatusGuardDropsStalePublish(t *testing.T) {
	m := NewWechatManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	current := make(chan struct{})
	m.mu.Lock()
	m.done = current
	m.mu.Unlock()

	stale := make(chan struct{})
	m.setStatus(clirest.WechatStatus{Phase: clirest.WechatConnected}, stale)
	if s := m.Status(); s.Phase != clirest.WechatIdle {
		t.Fatalf("stale publish got through: %+v", s)
	}
	m.setStatus(clirest.WechatStatus{Phase: clirest.WechatConnected}, current)
	if s := m.Status(); s.Phase != clirest.WechatConnected {
		t.Fatalf("current owner publish dropped: %+v", s)
	}
}

// A flow's own terminal publish must not be dropped by its cleanup
// (publish-first ordering).
func TestWechatFlowTerminalPublishSurvivesOwnCleanup(t *testing.T) {
	m := NewWechatManager(ChannelEnv{Ctx: context.Background(), Deps: kernel.Deps{}}, nil)
	done := make(chan struct{})
	m.mu.Lock()
	m.done = done
	m.mu.Unlock()

	m.setStatus(clirest.WechatStatus{Phase: clirest.WechatDisconnected}, done)
	m.mu.Lock()
	if m.done == done {
		m.done = nil
	}
	m.mu.Unlock()

	if s := m.Status(); s.Phase != clirest.WechatDisconnected {
		t.Fatalf("terminal publish dropped by own cleanup: %+v", s)
	}
}
