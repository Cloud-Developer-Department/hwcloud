package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/channel"
	"github.com/Cloud-Developer-Department/hwcloud/channel/wechat"
	"github.com/Cloud-Developer-Department/hwcloud/channel/wechat/protocol"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/session"

	clirest "github.com/Cloud-Developer-Department/hwcloud/cmd/cli/rest"
)

// WechatManager owns the WeChat connection lifecycle. One instance per
// process; entry points mirror FeishuManager:
//
//  1. --channel wechat flag      — connect immediately at startup
//  2. settings channels.wechat   — configured = enabled, connect at startup
//  3. POST /api/channels/wechat/connect — frontend-triggered
//
// The concurrency machinery (flock single-instance, done-channel state
// guards, stopping intent, bounded disconnect wait) is a line-for-line
// copy of FeishuManager — those mechanisms were hardened against the
// disconnect/registration races and must stay identical.
//
// Wechat-specific: the QR login has a pairing-code step
// (need_verifycode) — the registration goroutine blocks on
// verifyCodeCh until the frontend submits the code via
// POST /api/channels/wechat/verifycode — and the message session can
// expire server-side (errcode -14), which clears the credentials so the
// next connect re-scans.
var _ clirest.WechatChannel = (*WechatManager)(nil)

type WechatManager struct {
	baseCtx   context.Context
	cfg       *agent.Agent
	deps      kernel.Deps
	wechatCfg *config.WechatConfig // settings.json channels.wechat (may be nil)
	metaStore session.Store       // session metadata store (nil = no meta tagging)

	mu     sync.Mutex
	lock   *ChannelLock
	status clirest.WechatStatus
	cancel context.CancelFunc
	done   chan struct{} // closed when the connection goroutine exits (nil = none running)
	// stopping is the disconnect intent flag — see FeishuManager.
	stopping bool
	subs     []func(clirest.WechatStatus)
	// verifyCodeCh delivers pairing codes submitted via the API to the
	// registration goroutine; nil unless a registration is in flight and
	// the server asked for a pairing code.
	verifyCodeCh chan string
}

// NewWechatManager creates the process-level wechat connection manager.
// env.Ctx is the serve process context. wechatCfg is the settings.json
// channels.wechat block (nil when not configured — QR login then).
func NewWechatManager(env ChannelEnv, wechatCfg *config.WechatConfig) *WechatManager {
	return &WechatManager{
		baseCtx:   env.Ctx,
		wechatCfg: wechatCfg,
		cfg:       env.Cfg,
		deps:      env.Deps,
		metaStore: env.MetaStore,
		status:    clirest.WechatStatus{Phase: clirest.WechatIdle},
	}
}

// Status returns a snapshot of the current connection state.
func (m *WechatManager) Status() clirest.WechatStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Subscribe registers a callback invoked on every state change. Used by
// the REST layer to emit wechat.status events.
func (m *WechatManager) Subscribe(fn func(clirest.WechatStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, fn)
}

// setStatus publishes a state change — same guard semantics as
// FeishuManager.setStatus (see there).
func (m *WechatManager) setStatus(s clirest.WechatStatus, guard <-chan struct{}) {
	s.Connected = s.Phase == clirest.WechatConnected
	m.mu.Lock()
	if guard != nil && m.done != guard {
		m.mu.Unlock()
		return // not the current flow — drop the stale publish
	}
	m.status = s
	subs := append([]func(clirest.WechatStatus){}, m.subs...)
	m.mu.Unlock()
	for _, fn := range subs {
		fn(s)
	}
}

// Connect establishes the wechat connection (idempotent). When login is
// needed the QR is rendered to the terminal and the pairing code is read
// from stdin (the no-frontend entry point).
func (m *WechatManager) Connect() error {
	_, err := m.ConnectAsync(nil, nil)
	return err
}

// ConnectAsync starts the wechat connection flow. Credentials come from
// settings (the single source); QR login when none exist.
//
// Returns immediately; the flow runs on the process base context, never
// an HTTP request context. The machine lock is held for the whole
// connection lifetime (see FeishuManager.ConnectAsync).
//
// onQR receives the registration QR URL when login is needed (nil =
// render in the terminal). onScanned fires when the QR has been scanned
// (frontend shows "scanned, confirm on your phone"). registrationStarted
// is true when login runs; the connection completes asynchronously once
// the user authorizes — observe via Status / Subscribe.
func (m *WechatManager) ConnectAsync(onQR func(url string, expireIn int), onScanned func()) (bool, error) {
	m.mu.Lock()
	phase := m.status.Phase
	m.mu.Unlock()
	switch phase {
	case clirest.WechatConnecting, clirest.WechatConnected:
		return false, nil // connection already in flight (idempotent)
	case clirest.WechatRegistering:
		// If the QR cache is still valid, the registration is live — idempotent.
		_, _, expiresAt := loadWechatQR()
		if expiresAt > 0 && time.Now().Unix() < expiresAt {
			return false, nil
		}
		// QR cache expired — the old registration is stale (the server
		// hasn't returned "expired" yet). Cancel it and start fresh.
		m.Disconnect()
	}

	// Retry-windowed for the same reason as feishu: a disconnect that
	// abandoned a stuck goroutine leaves the flock held momentarily (see
	// AcquireChannelLockRetry).
	lock, err := AcquireChannelLockRetry("wechat", lockRetryWindow)
	if err != nil {
		return false, err
	}

	// Resolve credentials: the in-memory settings copy; QR login when
	// none.
	m.mu.Lock()
	wechatCfg := m.wechatCfg
	m.mu.Unlock()
	if wechatCfg != nil && wechatCfg.Token != "" {
		m.startConnection(lock, wechatConfigFromSettings(wechatCfg))
		return false, nil
	}

	// No credentials — QR login. Same lifecycle as feishu registration:
	// cancelable child of the base context, disconnect-abortable.
	m.setStatus(clirest.WechatStatus{Phase: clirest.WechatRegistering}, nil)
	regCtx, regCancel := context.WithCancel(m.baseCtx)
	regDone := make(chan struct{})
	m.mu.Lock()
	m.cancel = regCancel
	m.done = regDone
	m.verifyCodeCh = make(chan string)
	m.mu.Unlock()
	go func() {
		defer close(regDone)
		// Read under the lock: SetCredentials/ClearCredentials write the
		// in-memory copy concurrently — an unlocked read is a data race.
		m.mu.Lock()
		localCreds := wechatConfigFromSettings(m.wechatCfg)
		m.mu.Unlock()
		creds, rerr := ResolveWechatCredentials(regCtx, localCreds, onQR, func() {
			// QR scanned — the frontend flips its hint from "scan" to
			// "confirm on your phone". Guarded like every other publish
			// from this goroutine: a disconnect that took over the
			// manager must not have its state clobbered.
			m.setStatus(clirest.WechatStatus{Phase: clirest.WechatRegistering, Scanned: true}, regDone)
		}, m.waitVerifyCode)
		if rerr != nil {
			lock.Release()
			clearWechatQR()
			m.mu.Lock()
			disconnecting := m.stopping
			// Clear the channel WITHOUT closing it: a concurrent
			// SubmitVerifyCode that read the old pointer before this
			// critical section would otherwise panic on a send to a
			// closed channel. The nil check in SubmitVerifyCode (under
			// the same lock) already makes it fail fast with 409 — there
			// is nothing to unblock: waitVerifyCode is the source of this
			// error and has already returned.
			m.verifyCodeCh = nil
			m.mu.Unlock()
			if disconnecting || errors.Is(rerr, context.Canceled) {
				m.setStatus(clirest.WechatStatus{Phase: clirest.WechatDisconnected}, regDone)
				return
			}
			m.setStatus(clirest.WechatStatus{Phase: clirest.WechatDisconnected, LastError: fmt.Sprintf("wechat: %v", rerr)}, regDone)
			slog.Error("wechat: login failed", "error", rerr)
			m.mu.Lock()
			if m.done == regDone {
				m.cancel = nil
				m.done = nil
			}
			m.mu.Unlock()
			return
		}
		// Credentials are valid regardless of a disconnect — persist them
		// in memory so a later connect reuses them.
		m.mu.Lock()
		m.wechatCfg = &config.WechatConfig{Token: creds.Token, BaseURL: creds.BaseURL, AccountID: creds.AccountID, UserID: creds.UserID}
		// A disconnect that timed out on <-done has abandoned this flow —
		// the abandoned flow must NOT auto-connect.
		abandoned := m.done != regDone
		m.verifyCodeCh = nil
		m.mu.Unlock()
		if abandoned {
			lock.Release()
			return
		}
		clearWechatQR()
		m.startConnection(lock, creds.toProtocol())
	}()
	return true, nil
}

// waitVerifyCode blocks the registration goroutine until the frontend
// submits a pairing code (or the entry times out, or the login context
// is cancelled — a disconnect must not wait out the full timeout holding
// the flock). Marks the status so the frontend knows a pairing code is
// requested / retry. All publishes are guarded by the flow's own done
// channel: a disconnect that took over the manager (and possibly started
// a new connection) must not have its state clobbered by this flow's
// late status writes.
func (m *WechatManager) waitVerifyCode(ctx context.Context, isRetry bool) (string, error) {
	m.mu.Lock()
	ch := m.verifyCodeCh
	guard := m.done // this registration flow's own done (may be nil after an abandoned disconnect)
	m.mu.Unlock()
	if ch == nil {
		return "", fmt.Errorf("wechat: verify code channel not ready")
	}
	m.setStatus(clirest.WechatStatus{Phase: clirest.WechatRegistering, Scanned: true, VerifyCodeRequired: true, VerifyCodeRetry: isRetry}, guard)
	defer m.setStatus(clirest.WechatStatus{Phase: clirest.WechatRegistering, Scanned: true, VerifyCodeRequired: false}, guard)
	select {
	case code := <-ch:
		return code, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(verifyCodeTimeout):
		return "", fmt.Errorf("wechat: pairing code entry timed out")
	}
}

// verifyCodeTimeout bounds the pairing-code wait; on expiry the
// registration fails with a visible last_error and the user re-connects.
const verifyCodeTimeout = 2 * time.Minute

// SubmitVerifyCode delivers a pairing code to an in-flight registration.
// 409 when no registration is waiting for one.
func (m *WechatManager) SubmitVerifyCode(code string) error {
	if code == "" {
		return fmt.Errorf("wechat: code is required")
	}
	m.mu.Lock()
	ch := m.verifyCodeCh
	registering := m.status.Phase == clirest.WechatRegistering
	m.mu.Unlock()
	if ch == nil || !registering {
		return clirest.ErrWechatVerifyCodeNotPending
	}
	select {
	case ch <- code:
		return nil
	default:
		return clirest.ErrWechatVerifyCodeNotPending
	}
}

// startConnection launches the connection goroutine on the process base
// context and marks the state — same mechanism as FeishuManager.
func (m *WechatManager) startConnection(lock *ChannelLock, creds *protocol.Credentials) {
	connCtx, cancel := context.WithCancel(m.baseCtx)
	connDone := make(chan struct{})
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		cancel()
		lock.Release()
		return
	}
	m.lock = lock
	m.cancel = cancel
	m.done = connDone
	m.mu.Unlock()
	m.setStatus(clirest.WechatStatus{Phase: clirest.WechatConnecting, AccountID: creds.AccountID}, connDone)

	go func() {
		defer close(connDone)
		mediaDir := filepath.Join(channelDir("wechat"), "media")
		ch := wechat.New(creds, mediaDir)
		everReady := false
		ch.SetOnReady(func() {
			everReady = true
			now := time.Now()
			m.setStatus(clirest.WechatStatus{Phase: clirest.WechatConnected, AccountID: creds.AccountID, ConnectedAt: &now}, connDone)
		})
		ch.SetOnReconnecting(func() {
			m.setStatus(clirest.WechatStatus{Phase: clirest.WechatConnecting, AccountID: creds.AccountID}, connDone)
		})
		ch.SetOnError(func(err error) {
			if !everReady {
				m.setStatus(clirest.WechatStatus{Phase: clirest.WechatDisconnected, AccountID: creds.AccountID, LastError: err.Error()}, connDone)
			}
		})
		err := ch.Start(connCtx, wechatMessageHandler(m.cfg, m.deps, ch, m.metaStore))
		lock.Release()
		if errors.Is(err, wechat.ErrSessionExpired) {
			// The bot session died server-side: clear the credentials so
			// the next connect runs the QR login again. Best-effort —
			// settings persistence failing only costs a re-scan.
			if cerr := clearWechatFromSettings(); cerr != nil {
				slog.Warn("wechat: failed to clear expired session from settings", "error", cerr)
			}
			m.mu.Lock()
			if m.done == connDone {
				m.wechatCfg = nil
			}
			m.mu.Unlock()
			// Publish before the cleanup (guard semantics).
			m.setStatus(clirest.WechatStatus{Phase: clirest.WechatDisconnected, AccountID: creds.AccountID, LastError: "wechat: session expired — 请重新扫码"}, connDone)
		} else {
			m.setStatus(clirest.WechatStatus{Phase: clirest.WechatDisconnected, AccountID: creds.AccountID, LastError: errString(err)}, connDone)
		}
		m.mu.Lock()
		if m.done == connDone {
			m.lock = nil
			m.cancel = nil
			m.done = nil
		}
		m.mu.Unlock()
		slog.Warn("wechat: connection closed", "error", err)
	}()
}

// QR returns the cached registration QR and the login flow's interactive
// state (remaining lifetime computed from the cached expiry; scanned /
// pairing-code flags from the live status). Empty when no registration is
// in flight.
func (m *WechatManager) QR() (url, imgBase64 string, expireIn int, scanned, verifyCodeRequired, verifyCodeRetry bool) {
	url, imgBase64, expiresAt := loadWechatQR()
	if expiresAt > 0 {
		remaining := expiresAt - time.Now().Unix()
		if remaining > 0 {
			expireIn = int(remaining)
		}
	}
	s := m.Status()
	return url, imgBase64, expireIn, s.Scanned, s.VerifyCodeRequired, s.VerifyCodeRetry
}

// ClearCredentials removes the wechat credentials: the settings.json
// channels.wechat key (all other fields preserved) and the in-memory
// copy. A running connection keeps working until the next connect; the
// frontend's "re-register" flow is clear + connect (QR login then).
func (m *WechatManager) ClearCredentials() error {
	m.mu.Lock()
	registering := m.status.Phase == clirest.WechatRegistering
	m.mu.Unlock()
	if registering {
		return clirest.ErrWechatRegistrationInFlight
	}
	if err := clearWechatFromSettings(); err != nil {
		return err
	}
	m.mu.Lock()
	m.wechatCfg = nil
	m.mu.Unlock()
	return nil
}

// Disconnect tears down the wechat connection — same bounded-wait
// machinery as FeishuManager.Disconnect (see there).
func (m *WechatManager) Disconnect() {
	m.mu.Lock()
	cancel := m.cancel
	done := m.done
	m.stopping = true
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(disconnectTimeout):
			m.mu.Lock()
			if m.done == done {
				m.cancel = nil
				m.done = nil
				m.verifyCodeCh = nil
			}
			m.mu.Unlock()
		}
	}
	m.setStatus(clirest.WechatStatus{Phase: clirest.WechatDisconnected}, nil)
	m.mu.Lock()
	m.stopping = false
	m.mu.Unlock()
}

// Credentials returns the currently effective credentials (the in-memory
// settings copy). Empty values when none are configured.
func (m *WechatManager) Credentials() (token, baseURL, accountID, userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wechatCfg != nil && m.wechatCfg.Token != "" {
		return m.wechatCfg.Token, m.wechatCfg.BaseURL, m.wechatCfg.AccountID, m.wechatCfg.UserID
	}
	return "", "", "", ""
}

// SetCredentials stores the submitted credentials to settings.json and
// applies them to the in-memory copy. Credentials are separate from the
// connection: saving never touches a running connection. Returns an
// error while QR login is in flight (the login would overwrite the
// submitted values when it completes).
func (m *WechatManager) SetCredentials(token, baseURL, accountID, userID string) error {
	m.mu.Lock()
	registering := m.status.Phase == clirest.WechatRegistering
	m.mu.Unlock()
	if registering {
		return clirest.ErrWechatRegistrationInFlight
	}
	if token == "" {
		return fmt.Errorf("wechat: token is required")
	}
	if baseURL == "" {
		baseURL = wechatDefaultBaseURL
	}
	if err := saveWechatToSettings(WechatCredentials{Token: token, BaseURL: baseURL, AccountID: accountID, UserID: userID}); err != nil {
		return err
	}
	m.mu.Lock()
	m.wechatCfg = &config.WechatConfig{Token: token, BaseURL: baseURL, AccountID: accountID, UserID: userID}
	m.mu.Unlock()
	return nil
}

// wechatDefaultBaseURL is used when submitted credentials carry no base
// URL (the login flow normally fills it in).
const wechatDefaultBaseURL = "https://ilinkai.weixin.qq.com"

// wechatMessageHandler routes incoming WeChat messages to the agent: one
// ephemeral run per message, collecting the final answer and sending it
// ONCE when the run finishes (WeChat has no card/message-edit API — no
// progressive card stream). A typing indicator runs while the agent
// thinks.
func wechatMessageHandler(cfg *agent.Agent, deps kernel.Deps, ch *wechat.Channel, metaStore session.Store) channel.MessageHandler {
	return func(msgCtx context.Context, msg channel.IncomingMessage, reply channel.ReplyFunc) {
		sessionID := "wechat_" + msg.ChatID
		ensureChannelMeta(metaStore, sessionID, "wechat", msg.Text)

		// Intercept /clear before sending to the agent.
		if isClearCommand(msg.Text) {
			handleClearCommand(deps, reply, msgCtx, sessionID)
			return
		}

		go func() {
			// "对方正在输入" while the agent works (best-effort).
			_ = ch.SendTyping(msgCtx, msg.UserID)

			session := hwcloud.Session{
				ID:        sessionID,
				Model:     cfg.Model,
				CreatedAt: time.Now(),
			}
			stream := kernel.New(cfg, deps).RunStream(msgCtx, session, hwcloud.UserMessage(msg.Text))
			var b strings.Builder
			for evt := range stream {
				switch evt.Type {
				case hwcloud.StreamTextDelta:
					b.WriteString(evt.Text)
				case hwcloud.StreamError:
					if evt.Error != nil {
						b.WriteString("\n[error: " + evt.Error.Error() + "]")
					}
				}
			}
			// Send the final answer once. The reply func resolves media
			// markers ([file: path]) to CDN uploads and chunks long text.
			if text := strings.TrimSpace(b.String()); text != "" {
				_, _ = reply(msgCtx, channel.ReplyMessage{Text: text})
			}
		}()
	}
}
