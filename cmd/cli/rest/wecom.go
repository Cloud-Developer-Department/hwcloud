package rest

import (
	"errors"
	"time"
)

// ErrWecomRegistrationInFlight marks a SetCredentials rejection while QR
// authorization is in flight (the authorization would overwrite the
// submitted values when it completes). Typed so handlers can map it to
// 409.
var ErrWecomRegistrationInFlight = errors.New("wecom: QR authorization in progress — wait for it to finish or refresh")

// WecomPhase is the connection state machine phase of the wecom channel.
type WecomPhase string

const (
	// WecomIdle: never connected in this process.
	WecomIdle WecomPhase = "idle"
	// WecomRegistering: QR authorization in flight (no credentials yet);
	// SetCredentials is rejected (409) during this phase.
	WecomRegistering WecomPhase = "registering"
	// WecomConnecting: WebSocket connecting / reconnecting.
	WecomConnecting WecomPhase = "connecting"
	// WecomConnected: subscribed and receiving messages.
	WecomConnected WecomPhase = "connected"
	// WecomDisconnected: connection ended (error or Disconnect).
	WecomDisconnected WecomPhase = "disconnected"
)

// WecomStatus is the queryable state of the wecom channel, returned by
// GET /api/channels/wecom/status. The connection is a process-level
// daemon — the status survives page refreshes.
type WecomStatus struct {
	Phase     WecomPhase `json:"phase"`
	Connected bool       `json:"connected"` // convenience: phase == connected
	BotID     string     `json:"bot_id,omitempty"`
	// ConnectedAt is the last successful connection time (nil when never
	// connected). Pointer so the zero time is omitted from JSON.
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

// WecomChannel is the process-level wecom connection manager as seen by
// the CLI REST layer. Defined here (the consumer) per Go convention —
// cmd/cli/server implements it, which keeps the dependency acyclic.
type WecomChannel interface {
	// Status returns the current connection state.
	Status() WecomStatus

	// ConnectAsync starts the connection flow (runs on the process
	// context, never the request context). onQR receives the QR content
	// when authorization is needed. Returns registrationStarted.
	ConnectAsync(onQR func(url string, expireIn int)) (registrationStarted bool, err error)

	// Disconnect tears down the connection.
	Disconnect()

	// Credentials returns the currently effective credentials (from
	// settings — the single source). Empty values when none configured.
	Credentials() (botID, secret string)

	// SetCredentials stores the submitted credentials to settings.json
	// and applies them to the in-memory copy (next connect uses them).
	SetCredentials(botID, secret string) error

	// ClearCredentials removes the wecom credentials from settings.json
	// and the in-memory copy. A subsequent connect then runs the QR
	// authorization flow (frontend "re-register" = clear + connect).
	ClearCredentials() error

	// QR returns the cached authorization QR (URL + base64 PNG + remaining
	// lifetime). Empty when no authorization is in flight.
	QR() (url, imgBase64 string, expireIn int)
}
