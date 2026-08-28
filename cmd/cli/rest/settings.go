package rest

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Settings routes under /api/settings/ — the generic configuration
// domain. Currently only channels/feishu; future config items
// (provider, embedding, ...) register here too.
//
//	GET /api/settings/channels/feishu — current feishu configuration
//	PUT /api/settings/channels/feishu — store it (next connect applies)
func registerSettings(mux *http.ServeMux, mgr FeishuChannel) {
	mux.HandleFunc("GET /api/settings/channels/feishu", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		appID, secret := mgr.Credentials()
		writeJSON(w, http.StatusOK, map[string]any{
			"app_id":     appID,
			"app_secret": maskSecret(secret),
		})
	})
	mux.HandleFunc("PUT /api/settings/channels/feishu", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		var req struct {
			AppID     string `json:"app_id"`
			AppSecret string `json:"app_secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		if err := mgr.SetCredentials(req.AppID, req.AppSecret); err != nil {
			if errors.Is(err, ErrFeishuRegistrationInFlight) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"saved": true})
	})
	// Clear the feishu credentials — the frontend "re-register" flow is
	// DELETE + POST /connect (with no credentials left, connect runs QR
	// registration). A running connection keeps working until the next
	// connect (credentials and connection are separate).
	mux.HandleFunc("DELETE /api/settings/channels/feishu", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feishu channel not configured"})
			return
		}
		if err := mgr.ClearCredentials(); err != nil {
			if errors.Is(err, ErrFeishuRegistrationInFlight) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
	})
}

// registerWechatSettings mounts the wechat settings routes — the same
// generic settings domain as feishu: GET (masked), PUT (store), DELETE
// (clear — the "re-register" flow is DELETE + POST /connect, which then
// runs the QR login).
func registerWechatSettings(mux *http.ServeMux, mgr WechatChannel) {
	mux.HandleFunc("GET /api/settings/channels/wechat", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wechat channel not configured"})
			return
		}
		token, baseURL, accountID, userID := mgr.Credentials()
		writeJSON(w, http.StatusOK, map[string]any{
			"token":      maskSecret(token),
			"base_url":   baseURL,
			"account_id": accountID,
			"user_id":    userID,
		})
	})
	mux.HandleFunc("PUT /api/settings/channels/wechat", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wechat channel not configured"})
			return
		}
		var req struct {
			Token     string `json:"token"`
			BaseURL   string `json:"base_url"`
			AccountID string `json:"account_id"`
			UserID    string `json:"user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		if err := mgr.SetCredentials(req.Token, req.BaseURL, req.AccountID, req.UserID); err != nil {
			if errors.Is(err, ErrWechatRegistrationInFlight) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"saved": true})
	})
	mux.HandleFunc("DELETE /api/settings/channels/wechat", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wechat channel not configured"})
			return
		}
		if err := mgr.ClearCredentials(); err != nil {
			if errors.Is(err, ErrWechatRegistrationInFlight) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
	})
}

// registerWecomSettings mounts the wecom settings routes — GET (masked
// secret), PUT (store BotID/Secret from the admin console), DELETE
// (clear — the "re-register" flow is DELETE + POST /connect, which then
// runs the QR authorization).
func registerWecomSettings(mux *http.ServeMux, mgr WecomChannel) {
	mux.HandleFunc("GET /api/settings/channels/wecom", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wecom channel not configured"})
			return
		}
		botID, secret := mgr.Credentials()
		writeJSON(w, http.StatusOK, map[string]any{
			"bot_id": botID,
			"secret": maskSecret(secret),
		})
	})
	mux.HandleFunc("PUT /api/settings/channels/wecom", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wecom channel not configured"})
			return
		}
		var req struct {
			BotID  string `json:"bot_id"`
			Secret string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		if err := mgr.SetCredentials(req.BotID, req.Secret); err != nil {
			if errors.Is(err, ErrWecomRegistrationInFlight) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"saved": true})
	})
	mux.HandleFunc("DELETE /api/settings/channels/wecom", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "wecom channel not configured"})
			return
		}
		if err := mgr.ClearCredentials(); err != nil {
			if errors.Is(err, ErrWecomRegistrationInFlight) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
	})
}

// maskSecret masks a secret for display: "****" + last 4 chars (or
// "****" alone when shorter). The full value is never returned to the
// frontend.
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= 4 {
		return "****"
	}
	return "****" + string(runes[len(runes)-4:])
}
