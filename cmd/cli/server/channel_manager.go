package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"github.com/Cloud-Developer-Department/hwcloud/channel"
	"github.com/Cloud-Developer-Department/hwcloud/channel/feishu"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
	"github.com/Cloud-Developer-Department/hwcloud/session"

	clirest "github.com/Cloud-Developer-Department/hwcloud/cmd/cli/rest"
)

// FeishuManager owns the feishu connection lifecycle. One instance per
// process; three entry points share it:
//
//  1. --channel feishu flag      — connect immediately at startup
//  2. settings channels.feishu   — configured = enabled, connect at startup
//  3. POST /api/channels/feishu/connect — frontend-triggered
//
// The connection is a process-level daemon: it runs on the base context
// (the serve process ctx), NEVER on an HTTP request context — the
// frontend only triggers and observes (status + SSE events); closing a
// page or a handler returning never affects it. The machine-level flock
// guarantees a single live connection per config dir — a second instance
// fails fast instead of silently stealing events from the first.
var _ clirest.FeishuChannel = (*FeishuManager)(nil)

type FeishuManager struct {
	baseCtx     context.Context
	cfg         *agent.Agent
	deps        kernel.Deps
	feishuCfg   *config.FeishuConfig // settings.json channels.feishu (may be nil)
	defaultMode string               // "manual" | "auto" (empty = "manual")
	workDir     string               // workspace root for channel-specific tools
	metaStore   session.Store        // session metadata store (nil = no meta tagging)

	mu     sync.Mutex
	lock   *ChannelLock
	status clirest.FeishuStatus
	cancel context.CancelFunc
	done   chan struct{} // closed when the connection goroutine exits (nil = none running)
	// stopping is the disconnect intent flag: set under the lock at the
	// START of Disconnect so a registration goroutine checking its
	// checkpoint can never race the (later) status flip — the status is
	// only written after the old goroutine has exited, which is exactly
	// the window the flag must cover. Reset when Disconnect returns.
	stopping bool
	subs     []func(clirest.FeishuStatus)
}

// NewFeishuManager creates the process-level feishu connection manager.
// env.Ctx is the serve process context — the connection and the QR
// registration run on it, so neither is torn down by an HTTP request
// returning. feishuCfg is the settings.json channels.feishu block (nil
// when the user did not configure credentials — the manager then runs
// the QR registration flow).
func NewFeishuManager(env ChannelEnv, feishuCfg *config.FeishuConfig) *FeishuManager {
	return &FeishuManager{
		baseCtx:     env.Ctx,
		feishuCfg:   feishuCfg,
		cfg:         env.Cfg,
		deps:        env.Deps,
		defaultMode: env.DefaultMode,
		workDir:     env.WorkDir,
		metaStore:   env.MetaStore,
		status:      clirest.FeishuStatus{Phase: clirest.FeishuIdle},
	}
}

// Status returns a snapshot of the current connection state.
func (m *FeishuManager) Status() clirest.FeishuStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Subscribe registers a callback invoked on every state change
// (connecting / connected / disconnected / error). Used by the REST
// layer to emit feishu.status events. Callbacks run on the caller's
// goroutine — keep them quick.
func (m *FeishuManager) Subscribe(fn func(clirest.FeishuStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, fn)
}

// setStatus publishes a state change. guard, when non-nil, is the done
// channel of the flow that owns the state: the publish is dropped when
// another flow (or a disconnect) has since taken over the manager — a
// stale goroutine abandoned by a disconnect timeout must not clobber the
// new connection's state. Callers that act on the user's behalf (connect,
// disconnect) pass nil.
func (m *FeishuManager) setStatus(s clirest.FeishuStatus, guard <-chan struct{}) {
	s.Connected = s.Phase == clirest.FeishuConnected
	m.mu.Lock()
	if guard != nil && m.done != guard {
		m.mu.Unlock()
		return // not the current flow — drop the stale publish
	}
	m.status = s
	subs := append([]func(clirest.FeishuStatus){}, m.subs...)
	m.mu.Unlock()
	for _, fn := range subs {
		fn(s)
	}
}

// Connect establishes the feishu connection (idempotent: a live
// connection returns nil) and returns once the flow is started. When
// registration is needed the QR code is rendered to the terminal (the
// no-frontend entry point) and the connection completes asynchronously
// once the user scans it. Frontend callers use ConnectAsync with their
// own onQR callback instead.
func (m *FeishuManager) Connect() error {
	_, err := m.ConnectAsync(nil)
	return err
}

// ConnectAsync starts the feishu connection flow. Credentials come from
// settings (the single source); QR registration when none exist.
//
// Returns immediately; the flow runs on the process base context, so
// the caller (an HTTP handler) returning does not tear it down.
//
// The machine lock is taken for the whole connection lifetime and
// released on Disconnect / process exit. When another instance holds
// the lock the returned error is a fail-fast signal — the caller
// decides whether to abort the process or continue without the channel.
//
// onQR receives the registration QR URL when the flow has to register a
// new app (nil = render the QR in the terminal). registrationStarted is
// true in that case; the connection completes asynchronously once the
// user scans the QR — callers observe it via Status / Subscribe.
func (m *FeishuManager) ConnectAsync(onQR func(url string, expireIn int)) (bool, error) {
	m.mu.Lock()
	phase := m.status.Phase
	m.mu.Unlock()
	switch phase {
	case clirest.FeishuRegistering, clirest.FeishuConnecting, clirest.FeishuConnected:
		return false, nil // registration/connection already in flight (idempotent)
	}

	// Machine-level single instance: a second connection to the same
	// app would silently steal events from the first. Retry-windowed: a
	// disconnect that abandoned a stuck goroutine leaves the flock held
	// for a moment — the frontend reconnects immediately after, and the
	// retry absorbs that handoff (see AcquireChannelLockRetry).
	lock, err := AcquireChannelLockRetry("feishu", lockRetryWindow)
	if err != nil {
		return false, err
	}

	// Resolve credentials: the in-memory settings copy (updated live by
	// SetCredentials); QR registration when none.
	m.mu.Lock()
	feishuCfg := m.feishuCfg
	m.mu.Unlock()
	if feishuCfg != nil && feishuCfg.AppID != "" && feishuCfg.AppSecret != "" {
		m.startConnection(lock, FeishuCredentials{AppID: feishuCfg.AppID, AppSecret: feishuCfg.AppSecret})
		return false, nil
	}

	// No credentials — QR registration (settings is the single
	// credential source). Runs on a cancelable child of the process base
	// context: the HTTP handler returning does not tear it down, but
	// Disconnect can abort it (and a completed registration checks the
	// phase before connecting, so a disconnect during registration never
	// ends in an unexpected reconnect).
	m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuRegistering}, nil)
	regCtx, regCancel := context.WithCancel(m.baseCtx)
	regDone := make(chan struct{})
	m.mu.Lock()
	m.cancel = regCancel
	m.done = regDone
	m.mu.Unlock()
	go func() {
		defer close(regDone)
		reg, rerr := ResolveFeishuCredentials(regCtx, onQR)
		if rerr != nil {
			lock.Release()
			clearFeishuQR()
			m.mu.Lock()
			// A disconnect cancels the registration context; the SDK then
			// returns whatever error the cancellation produced (context
			// canceled, or a decode failure when a poll was mid-flight
			// and its response got torn). That is a byproduct of the
			// user's disconnect, not a real failure — surface a clean
			// "disconnected" without last_error. The flag is read under
			// the lock: Disconnect sets it before cancelling, so a
			// registration ending during a disconnect always sees it.
			disconnecting := m.stopping
			m.mu.Unlock()
			if disconnecting || errors.Is(rerr, context.Canceled) {
				m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuDisconnected}, regDone)
			} else {
				m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuDisconnected, LastError: fmt.Sprintf("feishu: %v", rerr)}, regDone)
				slog.Error("feishu: registration failed", "error", rerr)
			}
			// Publish BEFORE the cleanup — the guard compares m.done
			// against regDone, so clearing the field first would drop the
			// flow's own terminal publish. Cleanup itself is identity
			// guarded: a connect that took over since the publish must
			// not have its fields clobbered.
			m.mu.Lock()
			if m.done == regDone {
				m.cancel = nil
				m.done = nil
			}
			m.mu.Unlock()
			return
		}
		// Register the new credentials in the in-memory copy BEFORE the
		// connection check: a Disconnect may abandon the connection
		// (stopping), but the credentials are valid regardless — a later
		// connect must reuse them instead of forcing another scan.
		m.mu.Lock()
		m.feishuCfg = &config.FeishuConfig{AppID: reg.AppID, AppSecret: reg.AppSecret}
		// A disconnect that timed out on <-done has abandoned this flow
		// (it cleared the fields, then reset stopping — so the
		// startConnection checkpoint can no longer see the intent). The
		// credentials are still valid — a later connect reuses them —
		// but the abandoned flow must NOT auto-connect.
		abandoned := m.done != regDone
		m.mu.Unlock()
		if abandoned {
			lock.Release()
			return
		}
		// startConnection runs the disconnect checkpoint atomically with
		// its field replacement — a Disconnect that landed while the user
		// scanned (the SDK registration cannot always be aborted
		// mid-flight) is seen there, and no auto-connect happens.
		clearFeishuQR()
		m.startConnection(lock, reg)
	}()
	return true, nil
}

// startConnection launches the connection goroutine on the process base
// context and marks the state. The caller owns lock.
func (m *FeishuManager) startConnection(lock *ChannelLock, creds FeishuCredentials) {
	connCtx, cancel := context.WithCancel(m.baseCtx)
	connDone := make(chan struct{})
	// The disconnect checkpoint and the cancel/done field replacement
	// happen in ONE critical section: either this check sees stopping
	// (Disconnect already began — abandon, release the flock), or
	// Disconnect acquires the lock after us and reads the NEW cancel
	// (which fires, tearing the connection down). There is no interleaving
	// where the check passes but Disconnect's cancel is stale — that was
	// the race when the check lived in the registration goroutine.
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		cancel() // never used — release the derived context
		lock.Release()
		return
	}
	m.lock = lock
	m.cancel = cancel
	m.done = connDone
	m.mu.Unlock()
	m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuConnecting, AppID: creds.AppID}, connDone)

	go func() {
		defer close(connDone)
		// The connection blocks until the process context is cancelled
		// or the connection is permanently lost; the SDK reconnects on
		// transient failures internally.
		ch := feishu.New(creds.AppID, creds.AppSecret, m.defaultMode)
		mem := governance.NewSessionApprovalMemory()
		deps := m.deps
		// Clone the tools slice so the channel-specific SendFile tool
		// does not pollute the shared deps.Tools (other channels and
		// the REST/ACP paths share the same deps).
		deps.Tools = append([]hwcloud.Tool{}, m.deps.Tools...)
		if m.workDir != "" {
			deps.Tools = append(deps.Tools, feishu.NewSendFile(ch, m.workDir))
		}
		deps.HumanApprover = ch.Approver(mem)
		deps.ApprovalMemory = mem
		slog.Info("channel approver enabled", "channel", "feishu")
		everReady := false
		ch.SetOnReady(func() {
			// The SDK flips to ready after the WebSocket connects (and
			// after every reconnect) — this is the only place the
			// connected state is observable, since Start() blocks for
			// the whole connection lifetime.
			everReady = true
			now := time.Now()
			m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuConnected, AppID: creds.AppID, ConnectedAt: &now}, connDone)
		})
		ch.SetOnReconnecting(func() {
			// Auto-reconnect kicked in after a drop: the frontend must
			// see "connecting" (not a stale "connected") while the SDK
			// is re-establishing the WebSocket.
			m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuConnecting, AppID: creds.AppID}, connDone)
		})
		ch.SetOnError(func(err error) {
			// Never connected successfully (bad credentials, bootstrap
			// rejection): surface the failure instead of the SDK's silent
			// auto-reconnect loop, which would keep the status stuck on
			// "connecting" forever. Once ready, transient reconnect
			// failures keep "connecting" (the SDK is retrying).
			if !everReady {
				m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuDisconnected, AppID: creds.AppID, LastError: err.Error()}, connDone)
			}
		})
		err := ch.Start(connCtx, feishuMessageHandler(m.cfg, deps, ch, m.metaStore))
		lock.Release()
		// Publish BEFORE the cleanup — the guard compares m.done against
		// connDone, so clearing the field first would drop the flow's own
		// terminal publish and leave the status stuck on "connected".
		m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuDisconnected, AppID: creds.AppID, LastError: errString(err)}, connDone)
		m.mu.Lock()
		// Cleanup is identity guarded: a connect that took over since the
		// publish must not have its fields clobbered.
		if m.done == connDone {
			m.lock = nil
			m.cancel = nil
			m.done = nil
		}
		m.mu.Unlock()
		slog.Warn("feishu: connection closed", "error", err)
	}()
}

// QR returns the cached registration QR (URL + base64 PNG image) and its
// remaining lifetime in seconds (0 when expired or none in flight). The
// cache lives on disk under config.Dir()/channel/<name>/ so the frontend can
// re-fetch it after a refresh; the remaining time is computed from the
// cached absolute expiry, so a refresh restarts the countdown from where
// it actually is — not from the original total.
func (m *FeishuManager) QR() (url, imgBase64 string, expireIn int) {
	url, imgBase64, expiresAt := loadFeishuQR()
	if expiresAt <= 0 {
		return url, imgBase64, 0
	}
	remaining := expiresAt - time.Now().Unix()
	if remaining < 0 {
		return url, imgBase64, 0
	}
	return url, imgBase64, int(remaining)
}

// ClearCredentials removes the feishu credentials: the settings.json
// channels.feishu key (all other settings fields preserved) and the
// in-memory copy. A running connection keeps working with the old
// credentials until the next connect — credentials and connection are
// separate. The frontend's "re-register" flow is clear + connect (the
// connect then has no credentials and runs QR registration).
func (m *FeishuManager) ClearCredentials() error {
	m.mu.Lock()
	registering := m.status.Phase == clirest.FeishuRegistering
	m.mu.Unlock()
	if registering {
		return clirest.ErrFeishuRegistrationInFlight
	}
	err := config.UpdateSettings(func(raw map[string]json.RawMessage) error {
		channels := map[string]json.RawMessage{}
		if c, ok := raw["channels"]; ok {
			if uerr := json.Unmarshal(c, &channels); uerr != nil {
				return fmt.Errorf("feishu: parse settings channels: %w", uerr)
			}
		}
		delete(channels, "feishu")
		if len(channels) == 0 {
			delete(raw, "channels") // no channels left — drop the key
		} else {
			raw["channels"], _ = json.Marshal(channels)
		}
		return nil
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.feishuCfg = nil
	m.mu.Unlock()
	return nil
}

// lockRetryWindow is how long a connect retries the flock after a
// Disconnect abandoned a stuck goroutine (see AcquireChannelLockRetry).
// The abandoned goroutine releases the lock when its stuck request
// finally dies — usually within a second or two; the window covers that
// without making a genuinely-locked connect wait forever.
const lockRetryWindow = 5 * time.Second

// disconnectTimeout bounds the <-done wait in Disconnect. Normally the
// cancel makes the SDK return immediately; the exception is a
// registration poll whose HTTP response body is mid-read — Go's http
// body reads are NOT cancelled by context, and the SDK polls with
// http.DefaultClient (no Timeout), so the registration goroutine can be
// stuck until the server responds. Waiting forever would hang the
// disconnect endpoint; the timeout gives up and lets the stale goroutine
// finish whenever its request does.
const disconnectTimeout = 5 * time.Second

// Disconnect tears down the feishu connection and WAITS for the
// connection goroutine to exit (releasing the machine lock). A
// subsequent Connect can therefore re-acquire the lock immediately —
// without the wait, "disconnect → reconnect" (e.g. applying new
// credentials) would hit the still-held lock and fail.
//
// The wait is bounded by disconnectTimeout: a registration whose poll is
// stuck on an unresponsive server (see disconnectTimeout) must not hang
// the caller. On timeout the manager forgets the flow — its state
// publishes and field cleanup are identity-guarded (setStatus guard,
// `m.done == <flow>`), so the stale goroutine can never clobber a newer
// connection; the flock stays held until the goroutine exits, so a
// reconnect within that window fails fast with the lock error — retry.
func (m *FeishuManager) Disconnect() {
	// Intent flag first, atomically visible to any in-flight registration
	// goroutine's checkpoint (it may hold a cancel that already fired —
	// the registration completed — so the flag, not the cancel, is the
	// reliable signal).
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
		case <-done: // goroutine releases the lock and clears state before closing
		case <-time.After(disconnectTimeout):
			// Abandon the wait — the flow may be stuck in a poll body
			// read. Forget it so its late cleanup and publishes are
			// identity-guarded away (done must no longer match).
			m.mu.Lock()
			if m.done == done {
				m.cancel = nil
				m.done = nil
			}
			m.mu.Unlock()
		}
	}
	m.setStatus(clirest.FeishuStatus{Phase: clirest.FeishuDisconnected}, nil)
	m.mu.Lock()
	m.stopping = false
	m.mu.Unlock()
}

// Credentials returns the currently effective credentials (the in-memory
// settings copy — updated live by SetCredentials). Empty values when
// none are configured.
func (m *FeishuManager) Credentials() (appID, appSecret string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.feishuCfg != nil && m.feishuCfg.AppID != "" && m.feishuCfg.AppSecret != "" {
		return m.feishuCfg.AppID, m.feishuCfg.AppSecret
	}
	return "", ""
}

// SetCredentials stores the submitted credentials to settings.json (the
// single credential source — written atomically, preserving all other
// settings fields) and applies them to the in-memory copy. Credentials
// are separate from the connection: saving never touches a running
// connection — the frontend reconnects (disconnect + connect) when it
// wants the new values to take effect.
//
// An empty appSecret keeps the current secret (edit-form semantics).
// Returns an error while QR registration is in flight (the registration
// would overwrite the submitted values when it completes).
func (m *FeishuManager) SetCredentials(appID, appSecret string) error {
	m.mu.Lock()
	registering := m.status.Phase == clirest.FeishuRegistering
	m.mu.Unlock()
	if registering {
		return clirest.ErrFeishuRegistrationInFlight
	}

	// Empty secret keeps the current one (edit-form semantics).
	if appSecret == "" {
		cur, curSecret := m.Credentials()
		if curSecret == "" {
			return fmt.Errorf("feishu: app_secret required (no current secret to keep)")
		}
		appSecret = curSecret
		if appID == "" {
			appID = cur
		}
	}
	if appID == "" || appSecret == "" {
		return fmt.Errorf("feishu: app_id and app_secret are required")
	}

	// Persist to settings.json (atomic, preserves all other fields) and
	// update the in-memory copy so the value takes effect without a
	// restart. The "interface is configuration" semantics: submissions
	// from the control panel are user-level config, so settings (the
	// highest-priority source) is where they live; QR-registration
	// artifacts stay in the settings credential file.
	if err := saveFeishuToSettings(appID, appSecret); err != nil {
		return err
	}
	m.mu.Lock()
	m.feishuCfg = &config.FeishuConfig{AppID: appID, AppSecret: appSecret}
	m.mu.Unlock()
	return nil
}

// feishuMessageHandler routes incoming Feishu messages to the agent,
// one ephemeral run per message. ch is the concrete channel, used for
// runCardUpdater / ModeController interface checks.
func feishuMessageHandler(cfg *agent.Agent, deps kernel.Deps, ch channel.Channel, metaStore session.Store) channel.MessageHandler {
	var updater runCardUpdater
	if u, ok := ch.(runCardUpdater); ok {
		updater = u
	}

	var sizer CardSizer
	if s, ok := ch.(CardSizer); ok {
		sizer = s
	}

	return func(msgCtx context.Context, msg channel.IncomingMessage, reply channel.ReplyFunc) {
		sessionID := "feishu_" + msg.ChatID
		ensureChannelMeta(metaStore, sessionID, "feishu", msg.Text)

		// Intercept /clear before sending to the agent.
		if isClearCommand(msg.Text) {
			handleClearCommand(deps, reply, msgCtx, sessionID)
			return
		}

		// Intercept /mode command before sending to the agent.
		if mc, ok := ch.(ModeController); ok && isModeCommand(msg.Text) {
			args := parseModeArgs(msg.Text)
			if channel.IsValidMode(args) {
				mc.SetMode(msg.ChatID, args)
				_, _ = reply(msgCtx, channel.ReplyMessage{Text: "✅ 已切换到 " + args + " 模式"})
			} else {
				_, _ = reply(msgCtx, channel.ReplyMessage{Card: mc.BuildModeCard(msg.ChatID)})
			}
			return
		}

		go func() {
			// Carry the resolved Model instance so downstream consumers
			// (RunHooks via SessionFromContext, e.g. the artifact hook's
			// context-window threshold) read the same model the runner
			// uses.
			//
			// Metadata carries the Feishu receive_id so channel-specific
			// tools (e.g. feishu_sendfile) can send messages back to the
			// originating chat without a separate context key.
			session := hwcloud.Session{
				ID:        sessionID,
				Model:     cfg.Model,
				CreatedAt: time.Now(),
				Metadata:  feishu.ReceiveMetadata(msg),
			}
			rt := kernel.New(cfg, deps)
			// In auto mode, disable human approval so tools execute
			// without prompting. In manual mode (default), the
			// feishu approver wired in startConnection stays active.
			if mc, ok := ch.(ModeController); ok && mc.GetMode(msg.ChatID) == channel.ModeAuto {
				rt.SetHumanApprover(nil)
			}
			stream := rt.RunStream(msgCtx, session, hwcloud.UserMessage(msg.Text))
			streamReply(reply, stream, sessionID, updater, sizer)
		}()
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
