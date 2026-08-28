package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/channel"
	"github.com/Cloud-Developer-Department/hwcloud/channel/wecom"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/session"
	opentool "github.com/Cloud-Developer-Department/hwcloud/tool"

	clirest "github.com/Cloud-Developer-Department/hwcloud/cmd/cli/rest"
)

// WecomManager owns the WeCom smart-robot connection lifecycle. One
// instance per process; entry points mirror FeishuManager:
//
//  1. --channel wecom flag      — connect immediately at startup
//  2. settings channels.wecom   — configured = enabled, connect at startup
//  3. POST /api/channels/wecom/connect — frontend-triggered
//
// The concurrency machinery (flock single-instance, done-channel state
// guards, stopping intent, bounded disconnect wait) is a line-for-line
// copy of FeishuManager — those mechanisms were hardened against the
// disconnect/registration races and must stay identical.
//
// Wecom-specific: credentials are either configured by the user
// (BotID/Secret from the admin console) or obtained through the official
// QR authorization flow (scan → robot created automatically) — there is
// no pairing-code step, unlike wechat.
var _ clirest.WecomChannel = (*WecomManager)(nil)

type WecomManager struct {
	baseCtx   context.Context
	cfg       *agent.Agent
	deps      kernel.Deps
	wecomCfg  *config.WecomConfig // settings.json channels.wecom (may be nil)
	workDir   string              // workspace root for wecom_sendfile tool
	metaStore session.Store       // session metadata store (nil = no meta tagging)

	mu     sync.Mutex
	lock   *ChannelLock
	status clirest.WecomStatus
	cancel context.CancelFunc
	done   chan struct{} // closed when the connection goroutine exits (nil = none running)
	// stopping is the disconnect intent flag — see FeishuManager.
	stopping bool
	subs     []func(clirest.WecomStatus)
}

// NewWecomManager creates the process-level wecom connection manager.
// env.Ctx is the serve process context. wecomCfg is the settings.json
// channels.wecom block (nil when not configured — QR authorization).
func NewWecomManager(env ChannelEnv, wecomCfg *config.WecomConfig) *WecomManager {
	return &WecomManager{
		baseCtx:   env.Ctx,
		wecomCfg:  wecomCfg,
		cfg:       env.Cfg,
		deps:      env.Deps,
		workDir:   env.WorkDir,
		metaStore: env.MetaStore,
		status:    clirest.WecomStatus{Phase: clirest.WecomIdle},
	}
}

// Status returns a snapshot of the current connection state.
func (m *WecomManager) Status() clirest.WecomStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Subscribe registers a callback invoked on every state change. Used by
// the REST layer to emit wecom.status events.
func (m *WecomManager) Subscribe(fn func(clirest.WecomStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, fn)
}

// setStatus publishes a state change — same guard semantics as
// FeishuManager.setStatus (see there).
func (m *WecomManager) setStatus(s clirest.WecomStatus, guard <-chan struct{}) {
	s.Connected = s.Phase == clirest.WecomConnected
	m.mu.Lock()
	if guard != nil && m.done != guard {
		m.mu.Unlock()
		return // not the current flow — drop the stale publish
	}
	m.status = s
	subs := append([]func(clirest.WecomStatus){}, m.subs...)
	m.mu.Unlock()
	for _, fn := range subs {
		fn(s)
	}
}

// Connect establishes the wecom connection (idempotent). When no
// credentials exist the QR authorization flow runs (QR rendered to the
// terminal — the no-frontend entry point).
func (m *WecomManager) Connect() error {
	_, err := m.ConnectAsync(nil)
	return err
}

// ConnectAsync starts the wecom connection flow. Credentials come from
// settings (the single source); QR authorization when none exist.
//
// Returns immediately; the flow runs on the process base context, never
// an HTTP request context. The machine lock is held for the whole
// connection lifetime (see FeishuManager.ConnectAsync).
//
// onQR receives the QR content when authorization is needed (nil =
// render in the terminal). registrationStarted is true in that case; the
// connection completes asynchronously once the user scans — observe via
// Status / Subscribe.
func (m *WecomManager) ConnectAsync(onQR func(url string, expireIn int)) (bool, error) {
	m.mu.Lock()
	phase := m.status.Phase
	m.mu.Unlock()
	switch phase {
	case clirest.WecomRegistering, clirest.WecomConnecting, clirest.WecomConnected:
		return false, nil // authorization/connection already in flight (idempotent)
	}

	lock, err := AcquireChannelLockRetry("wecom", lockRetryWindow)
	if err != nil {
		return false, err
	}

	// Resolve credentials: the in-memory settings copy; QR authorization
	// when none.
	m.mu.Lock()
	wecomCfg := m.wecomCfg
	m.mu.Unlock()
	if wecomCfg != nil && wecomCfg.BotID != "" && wecomCfg.Secret != "" {
		m.startConnection(lock, wecomConfigFromSettings(wecomCfg))
		return false, nil
	}

	// No credentials — QR authorization. Same lifecycle as feishu
	// registration: cancelable child of the base context,
	// disconnect-abortable.
	m.setStatus(clirest.WecomStatus{Phase: clirest.WecomRegistering}, nil)
	regCtx, regCancel := context.WithCancel(m.baseCtx)
	regDone := make(chan struct{})
	m.mu.Lock()
	m.cancel = regCancel
	m.done = regDone
	m.mu.Unlock()
	go func() {
		defer close(regDone)
		creds, rerr := ResolveWecomCredentials(regCtx, onQR)
		if rerr != nil {
			lock.Release()
			clearWecomQR()
			m.mu.Lock()
			disconnecting := m.stopping
			m.mu.Unlock()
			if disconnecting || errors.Is(rerr, context.Canceled) {
				m.setStatus(clirest.WecomStatus{Phase: clirest.WecomDisconnected}, regDone)
				return
			}
			m.setStatus(clirest.WecomStatus{Phase: clirest.WecomDisconnected, LastError: fmt.Sprintf("wecom: %v", rerr)}, regDone)
			slog.Error("wecom: authorization failed", "error", rerr)
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
		m.wecomCfg = &config.WecomConfig{BotID: creds.BotID, Secret: creds.Secret}
		// A disconnect that timed out on <-done has abandoned this flow —
		// the abandoned flow must NOT auto-connect.
		abandoned := m.done != regDone
		m.mu.Unlock()
		if abandoned {
			lock.Release()
			return
		}
		clearWecomQR()
		m.startConnection(lock, wecomConfigFromSettings(m.wecomCfg))
	}()
	return true, nil
}

// startConnection launches the connection goroutine on the process base
// context and marks the state — same mechanism as FeishuManager.
func (m *WecomManager) startConnection(lock *ChannelLock, creds *wecom.BotCreds) {
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
	m.setStatus(clirest.WecomStatus{Phase: clirest.WecomConnecting, BotID: creds.BotID}, connDone)

	go func() {
		defer close(connDone)
		ch := wecom.New(creds.BotID, creds.Secret)

		// Clone deps so the channel-specific SendFile tool does not
		// pollute the shared deps (other channels and the REST/ACP
		// paths share the same base deps).
		deps := m.deps
		deps.Tools = append([]hwcloud.Tool{}, m.deps.Tools...)
		if m.workDir != "" {
			deps.Tools = append(deps.Tools, wecom.NewSendFile(ch, m.workDir))
		}
		mem := governance.NewSessionApprovalMemory()
		deps.HumanApprover = ch.Approver(mem)
		deps.ApprovalMemory = mem
		slog.Info("channel approver enabled", "channel", "wecom")

		everReady := false
		ch.SetOnReady(func() {
			everReady = true
			now := time.Now()
			m.setStatus(clirest.WecomStatus{Phase: clirest.WecomConnected, BotID: creds.BotID, ConnectedAt: &now}, connDone)
		})
		ch.SetOnReconnecting(func() {
			m.setStatus(clirest.WecomStatus{Phase: clirest.WecomConnecting, BotID: creds.BotID}, connDone)
		})
		ch.SetOnError(func(err error) {
			if !everReady {
				slog.Error("wecom: connection failed before ready", "error", err)
				m.setStatus(clirest.WecomStatus{Phase: clirest.WecomDisconnected, BotID: creds.BotID, LastError: err.Error()}, connDone)
			}
		})
		err := ch.Start(connCtx, wecomMessageHandler(m.cfg, deps, m.metaStore, ch))
		lock.Release()
		// Publish before the cleanup (guard semantics — see FeishuManager).
		m.setStatus(clirest.WecomStatus{Phase: clirest.WecomDisconnected, BotID: creds.BotID, LastError: errString(err)}, connDone)
		m.mu.Lock()
		if m.done == connDone {
			m.lock = nil
			m.cancel = nil
			m.done = nil
		}
		m.mu.Unlock()
		slog.Warn("wecom: connection closed", "error", err)
	}()
}

// QR returns the cached authorization QR (URL + base64 PNG image) and
// its remaining lifetime in seconds (0 when expired or none in flight).
func (m *WecomManager) QR() (url, imgBase64 string, expireIn int) {
	url, imgBase64, expiresAt := loadWecomQR()
	if expiresAt <= 0 {
		return url, imgBase64, 0
	}
	remaining := expiresAt - time.Now().Unix()
	if remaining < 0 {
		return url, imgBase64, 0
	}
	return url, imgBase64, int(remaining)
}

// ClearCredentials removes the wecom credentials: the settings.json
// channels.wecom key (all other fields preserved) and the in-memory
// copy. A running connection keeps working until the next connect; the
// frontend's "re-register" flow is clear + connect (QR authorization).
func (m *WecomManager) ClearCredentials() error {
	m.mu.Lock()
	registering := m.status.Phase == clirest.WecomRegistering
	m.mu.Unlock()
	if registering {
		return clirest.ErrWecomRegistrationInFlight
	}
	if err := clearWecomFromSettings(); err != nil {
		return err
	}
	m.mu.Lock()
	m.wecomCfg = nil
	m.mu.Unlock()
	return nil
}

// Disconnect tears down the wecom connection — same bounded-wait
// machinery as FeishuManager.Disconnect (see there).
func (m *WecomManager) Disconnect() {
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
			}
			m.mu.Unlock()
		}
	}
	m.setStatus(clirest.WecomStatus{Phase: clirest.WecomDisconnected}, nil)
	m.mu.Lock()
	m.stopping = false
	m.mu.Unlock()
}

// Credentials returns the currently effective credentials (the in-memory
// settings copy). Empty values when none are configured.
func (m *WecomManager) Credentials() (botID, secret string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wecomCfg != nil && m.wecomCfg.BotID != "" && m.wecomCfg.Secret != "" {
		return m.wecomCfg.BotID, m.wecomCfg.Secret
	}
	return "", ""
}

// SetCredentials stores the submitted credentials to settings.json and
// applies them to the in-memory copy. Credentials are separate from the
// connection: saving never touches a running connection. Returns an
// error while QR authorization is in flight.
func (m *WecomManager) SetCredentials(botID, secret string) error {
	m.mu.Lock()
	registering := m.status.Phase == clirest.WecomRegistering
	m.mu.Unlock()
	if registering {
		return clirest.ErrWecomRegistrationInFlight
	}
	if botID == "" || secret == "" {
		return fmt.Errorf("wecom: bot_id and secret are required")
	}
	if err := saveWecomToSettings(WecomCredentials{BotID: botID, Secret: secret}); err != nil {
		return err
	}
	m.mu.Lock()
	m.wecomCfg = &config.WecomConfig{BotID: botID, Secret: secret}
	m.mu.Unlock()
	return nil
}

// ── Message handler ──

// wecomMessageHandler routes incoming WeCom messages to the agent with
// STREAMING replies: the final answer is sent as a stream message that
// grows in place (finish=false refreshes → finish=true ends), throttled
// to ~1 refresh/second so the user sees the answer build up without
// spamming. req_id is echoed verbatim (the reply func captures it).
func wecomMessageHandler(cfg *agent.Agent, deps kernel.Deps, metaStore session.Store, ch *wecom.Channel) channel.MessageHandler {
	return func(msgCtx context.Context, msg channel.IncomingMessage, reply channel.ReplyFunc) {
		sessionID := "wecom_" + msg.ChatID
		ensureChannelMeta(metaStore, sessionID, "wecom", msg.Text)

		// Intercept /clear before sending to the agent.
		if isClearCommand(msg.Text) {
			handleClearCommand(deps, reply, msgCtx, sessionID)
			return
		}

		go func() {
			session := hwcloud.Session{
				ID:        sessionID,
				Model:     cfg.Model,
				CreatedAt: time.Now(),
				Metadata:  wecom.ReceiveMetadata(msg),
			}

			var b strings.Builder
			var streamID string
			var lastDisplay string
			sentLen := 0
			lastFlush := time.Now()
			hasText := false

			flush := func() {
				text := b.String()
				if text == "" {
					return
				}
				if streamID == "" {
					streamID, _ = reply(msgCtx, channel.ReplyMessage{Text: text})
				} else {
					_, _ = reply(msgCtx, channel.ReplyMessage{UpdateID: streamID, Text: text})
				}
				lastDisplay = text
				sentLen = b.Len()
				lastFlush = time.Now()
			}

			// Register a pre-approval hook: when the approver is about
			// to send the approval card, finish the current streaming
			// reply first. WeCom rejects aibot_send_msg while a stream
			// is open (errcode 6000). After the hook fires, streamID
			// is cleared so the next flush starts a fresh stream.
			ch.SetPreApprovalHook(func() {
				if streamID != "" {
					_, _ = reply(msgCtx, channel.ReplyMessage{UpdateID: wecom.FinishMarker, Text: lastDisplay})
					streamID = ""
					sentLen = 0
					hasText = false
					b.Reset()
				}
			})

			stream := kernel.New(cfg, deps).RunStream(msgCtx, session, hwcloud.UserMessage(msg.Text))

			for evt := range stream {
				switch evt.Type {
				case hwcloud.StreamTextDelta:
					b.WriteString(evt.Text)
					if !hasText {
						hasText = true
						flush()
					} else if time.Since(lastFlush) >= time.Second || b.Len()-sentLen >= 200 {
						flush()
					}
				case hwcloud.StreamToolCall:
					for _, tc := range evt.Message.ToolCalls {
						title := opentool.ToolTitle(tc.Function.Name, tc.Function.Arguments)
						hint := toolEmoji(tc.Function.Name) + " " + title
						display := b.String()
						if display == "" {
							display = hint + "..."
						} else {
							display += "\n\n" + hint + "..."
						}
						_, _ = reply(msgCtx, channel.ReplyMessage{UpdateID: streamID, Text: display})
						lastDisplay = display
						lastFlush = time.Now()
					}
				case hwcloud.StreamError:
					if evt.Error != nil {
						b.WriteString("\n[error: " + evt.Error.Error() + "]")
					}
				case hwcloud.StreamAborted:
					if b.Len() == 0 {
						b.WriteString("已取消")
					}
				}
			}
			// Terminal: flush what remains and end the stream (finish=true).
			if b.Len() == 0 {
				b.WriteString("✅ 已完成")
			}
			if streamID == "" || b.Len() > sentLen {
				flush()
			}
			if streamID != "" {
				_, _ = reply(msgCtx, channel.ReplyMessage{UpdateID: wecom.FinishMarker, Text: b.String()})
			}
		}()
	}
}
