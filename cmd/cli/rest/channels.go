// Package rest serves the CLI-level REST API: deployment/configuration
// endpoints that the agent-level rest package does not own — channel
// connection control and settings access. It lives under cmd/cli
// because it operates on CLI concepts (settings.json via cmd/cli/config,
// the process-level channel manager in cmd/cli/server); the agent-level
// API (sessions/chat/approve) stays in the top-level rest package.
package rest

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Register mounts the CLI-level API on mux. feishu and wechat are the
// process-level connection managers (never nil in practice — RunChannels
// always creates both; guarded anyway).
func Register(mux *http.ServeMux, feishu FeishuChannel, wechat WechatChannel, wecom WecomChannel) {
	mux.HandleFunc("GET /api/channels/feishu/status", func(w http.ResponseWriter, r *http.Request) {
		if feishu == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		writeJSON(w, http.StatusOK, feishu.Status())
	})
	mux.HandleFunc("POST /api/channels/feishu/connect", func(w http.ResponseWriter, r *http.Request) {
		if feishu == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		qrCh := make(chan string, 1)
		registration, err := feishu.ConnectAsync(func(url string, _ int) {
			select {
			case qrCh <- url:
			default:
			}
		})
		if err != nil {
			// Machine lock held by another instance — fail-fast surfaced
			// to the caller.
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if !registration {
			writeJSON(w, http.StatusOK, feishu.Status())
			return
		}
		// Registration flow: wait for the QR URL (registration start is
		// fast; the request context cancelling here does NOT cancel the
		// registration — it runs on the process context). The image is
		// read from the cache (written by the onQR callback).
		select {
		case url := <-qrCh:
			_, img, expireIn := feishu.QR()
			writeJSON(w, http.StatusAccepted, map[string]any{
				"status":          "registration",
				"qr_url":          url,
				"qr_img_base64":   img,
				"expires_in":      expireIn,
			})
		case <-r.Context().Done():
			// Client went away before the URL arrived; the registration
			// itself continues on the process context.
		}
	})
	mux.HandleFunc("POST /api/channels/feishu/disconnect", func(w http.ResponseWriter, r *http.Request) {
		if feishu == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		feishu.Disconnect()
		writeJSON(w, http.StatusOK, feishu.Status())
	})
	// Re-fetch the registration QR (URL + base64 PNG + remaining lifetime)
	// after a refresh — POST /connect is idempotent while registering and
	// does not re-issue it, so this endpoint restores the QR and restarts
	// the frontend countdown from the cached absolute expiry.
	mux.HandleFunc("GET /api/channels/feishu/qr", func(w http.ResponseWriter, r *http.Request) {
		if feishu == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		// Guard on the live phase, not just the cache file: a QR file
		// left over from a pre-restart registration must not be served.
		if feishu.Status().Phase != FeishuRegistering {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no QR registration in flight"})
			return
		}
		url, img, expireIn := feishu.QR()
		if url == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no QR registration in flight"})
			return
		}
		if expireIn <= 0 {
			// The QR is dead but the SDK has not yet surfaced the expiry
			// (it only happens on the next poll): the frontend must not
			// keep counting down — tell it to re-register.
			writeJSON(w, http.StatusOK, map[string]any{"expired": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"qr_url":        url,
			"qr_img_base64": img,
			"expires_in":    expireIn,
		})
	})

	// ── Wechat ──

	mux.HandleFunc("GET /api/channels/wechat/status", func(w http.ResponseWriter, r *http.Request) {
		if wechat == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wechat channel not configured"})
			return
		}
		writeJSON(w, http.StatusOK, wechat.Status())
	})
	mux.HandleFunc("POST /api/channels/wechat/connect", func(w http.ResponseWriter, r *http.Request) {
		if wechat == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wechat channel not configured"})
			return
		}
		qrCh := make(chan string, 1)
		scannedCh := make(chan struct{}, 1)
		registration, err := wechat.ConnectAsync(func(url string, _ int) {
			select {
			case qrCh <- url:
			default:
			}
		}, func() {
			select {
			case scannedCh <- struct{}{}:
			default:
			}
		})
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if !registration {
			writeJSON(w, http.StatusOK, wechat.Status())
			return
		}
		select {
		case url := <-qrCh:
			_, img, expireIn, scanned, vcReq, vcRetry := wechat.QR()
			writeJSON(w, http.StatusAccepted, map[string]any{
				"status":               "registration",
				"qr_url":               url,
				"qr_img_base64":        img,
				"expires_in":           expireIn,
				"scanned":              scanned,
				"verify_code_required": vcReq,
				"verify_code_retry":    vcRetry,
			})
		case <-scannedCh:
			// QR was scanned before the URL was read — return current state.
			url, img, expireIn, scanned, vcReq, vcRetry := wechat.QR()
			writeJSON(w, http.StatusAccepted, map[string]any{
				"status":               "registration",
				"qr_url":               url,
				"qr_img_base64":        img,
				"expires_in":           expireIn,
				"scanned":              scanned,
				"verify_code_required": vcReq,
				"verify_code_retry":    vcRetry,
			})
		case <-r.Context().Done():
			// Client went away before the URL arrived; the login itself
			// continues on the process context.
		}
	})
	mux.HandleFunc("POST /api/channels/wechat/disconnect", func(w http.ResponseWriter, r *http.Request) {
		if wechat == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wechat channel not configured"})
			return
		}
		wechat.Disconnect()
		writeJSON(w, http.StatusOK, wechat.Status())
	})
	// Re-fetch the registration QR after a refresh — same semantics as
	// the feishu endpoint, plus the login flow's interactive flags.
	mux.HandleFunc("GET /api/channels/wechat/qr", func(w http.ResponseWriter, r *http.Request) {
		if wechat == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wechat channel not configured"})
			return
		}
		if wechat.Status().Phase != WechatRegistering {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no QR registration in flight"})
			return
		}
		url, img, expireIn, scanned, vcReq, vcRetry := wechat.QR()
		if url == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no QR registration in flight"})
			return
		}
		if expireIn <= 0 {
			writeJSON(w, http.StatusOK, map[string]any{"expired": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"qr_url":               url,
			"qr_img_base64":        img,
			"expires_in":           expireIn,
			"scanned":              scanned,
			"verify_code_required": vcReq,
			"verify_code_retry":    vcRetry,
		})
	})
	// Pairing-code submission — the only channel for the need_verifycode
	// interaction (the phone shows digits; the user types them here).
	mux.HandleFunc("POST /api/channels/wechat/verifycode", func(w http.ResponseWriter, r *http.Request) {
		if wechat == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wechat channel not configured"})
			return
		}
		var req struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		if err := wechat.SubmitVerifyCode(req.Code); err != nil {
			if errors.Is(err, ErrWechatVerifyCodeNotPending) || errors.Is(err, ErrWechatRegistrationInFlight) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"submitted": true})
	})

	// ── Wecom ──

	mux.HandleFunc("GET /api/channels/wecom/status", func(w http.ResponseWriter, r *http.Request) {
		if wecom == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wecom channel not configured"})
			return
		}
		writeJSON(w, http.StatusOK, wecom.Status())
	})
	mux.HandleFunc("POST /api/channels/wecom/connect", func(w http.ResponseWriter, r *http.Request) {
		if wecom == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wecom channel not configured"})
			return
		}
		qrCh := make(chan string, 1)
		registration, err := wecom.ConnectAsync(func(url string, _ int) {
			select {
			case qrCh <- url:
			default:
			}
		})
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if !registration {
			writeJSON(w, http.StatusOK, wecom.Status())
			return
		}
		select {
		case url := <-qrCh:
			_, img, expireIn := wecom.QR()
			writeJSON(w, http.StatusAccepted, map[string]any{
				"status":        "registration",
				"qr_url":        url,
				"qr_img_base64": img,
				"expires_in":    expireIn,
			})
		case <-r.Context().Done():
			// Client went away before the URL arrived; the authorization
			// itself continues on the process context.
		}
	})
	mux.HandleFunc("POST /api/channels/wecom/disconnect", func(w http.ResponseWriter, r *http.Request) {
		if wecom == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wecom channel not configured"})
			return
		}
		wecom.Disconnect()
		writeJSON(w, http.StatusOK, wecom.Status())
	})
	// Re-fetch the authorization QR after a refresh — same semantics as
	// the other channels.
	mux.HandleFunc("GET /api/channels/wecom/qr", func(w http.ResponseWriter, r *http.Request) {
		if wecom == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wecom channel not configured"})
			return
		}
		if wecom.Status().Phase != WecomRegistering {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no QR registration in flight"})
			return
		}
		url, img, expireIn := wecom.QR()
		if url == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no QR registration in flight"})
			return
		}
		if expireIn <= 0 {
			writeJSON(w, http.StatusOK, map[string]any{"expired": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"qr_url":        url,
			"qr_img_base64": img,
			"expires_in":    expireIn,
		})
	})

	// Configuration endpoints (generic settings domain).
	registerSettings(mux, feishu)
	registerWechatSettings(mux, wechat)
	registerWecomSettings(mux, wecom)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("cli-rest: json encode failed", "error", err)
	}
}
