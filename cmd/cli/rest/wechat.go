package rest

import (
	"errors"
	"time"
)

// ErrWechatRegistrationInFlight marks a SetCredentials rejection while
// QR login is in flight (the login would overwrite the submitted values
// when it completes). Typed so handlers can map it to 409.
var ErrWechatRegistrationInFlight = errors.New("wechat: QR login in progress — wait for it to finish or refresh")

// ErrWechatVerifyCodeNotPending marks a pairing-code submission when no
// registration is waiting for one (not registering, or no code pending).
var ErrWechatVerifyCodeNotPending = errors.New("wechat: no pairing code pending")

// WechatPhase is the connection state machine phase of the wechat
// channel.
type WechatPhase string

const (
	// WechatIdle: never connected in this process.
	WechatIdle WechatPhase = "idle"
	// WechatRegistering: QR login in flight (no credentials yet);
	// SetCredentials is rejected (409) during this phase.
	WechatRegistering WechatPhase = "registering"
	// WechatConnecting: long-poll loop started, first poll not yet ok.
	WechatConnecting WechatPhase = "connecting"
	// WechatConnected: the message loop is live.
	WechatConnected WechatPhase = "connected"
	// WechatDisconnected: connection ended (error, session expiry, or
	// Disconnect).
	WechatDisconnected WechatPhase = "disconnected"
)

// WechatStatus is the queryable state of the wechat channel, returned by
// GET /api/channels/wechat/status. The connection is a process-level
// daemon — the status survives page refreshes.
type WechatStatus struct {
	Phase     WechatPhase `json:"phase"`
	Connected bool        `json:"connected"` // convenience: phase == connected
	AccountID string      `json:"account_id,omitempty"`
	// ConnectedAt is the last successful connection time (nil when never
	// connected). Pointer so the zero time is omitted from JSON.
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	// Scanned: the login QR has been scanned — the user must confirm on
	// their phone.
	Scanned bool `json:"scanned,omitempty"`
	// VerifyCodeRequired: the server asked for a pairing code — the
	// frontend shows an input box (POST /verifycode).
	VerifyCodeRequired bool `json:"verify_code_required,omitempty"`
	// VerifyCodeRetry: a previously submitted pairing code was rejected —
	// prompt the user again.
	VerifyCodeRetry bool `json:"verify_code_retry,omitempty"`
}

// WechatChannel is the process-level wechat connection manager as seen
// by the CLI REST layer. Defined here (the consumer) per Go convention —
// cmd/cli/server implements it, which keeps the dependency acyclic.
// Deliberately separate from FeishuChannel: the method signatures differ
// materially (4-field credentials, onScanned callback, pairing-code
// submission), and a shared interface would only erase type safety.
type WechatChannel interface {
	// Status returns the current connection state.
	Status() WechatStatus

	// ConnectAsync starts the connection flow (runs on the process
	// context, never the request context). onQR receives the QR URL when
	// login is needed; onScanned fires when the QR has been scanned.
	// Returns registrationStarted.
	ConnectAsync(onQR func(url string, expireIn int), onScanned func()) (registrationStarted bool, err error)

	// Disconnect tears down the connection.
	Disconnect()

	// Credentials returns the currently effective credentials (from
	// settings — the single source). Empty values when none configured.
	Credentials() (token, baseURL, accountID, userID string)

	// SetCredentials stores the submitted credentials to settings.json
	// and applies them to the in-memory copy (next connect uses them).
	SetCredentials(token, baseURL, accountID, userID string) error

	// ClearCredentials removes the wechat credentials from settings.json
	// and the in-memory copy. A subsequent connect then runs the QR login
	// flow (frontend "re-register" = clear + connect).
	ClearCredentials() error

	// QR returns the cached registration QR (URL + base64 PNG + remaining
	// lifetime) and the login flow's interactive flags (scanned /
	// pairing-code requested / retry). Empty when no login is in flight.
	QR() (url, imgBase64 string, expireIn int, scanned, verifyCodeRequired, verifyCodeRetry bool)

	// SubmitVerifyCode delivers a pairing code to an in-flight login.
	// ErrWechatVerifyCodeNotPending when no code is being awaited.
	SubmitVerifyCode(code string) error
}
