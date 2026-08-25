package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/skip2/go-qrcode"

	"github.com/Cloud-Developer-Department/hwcloud/channel/wechat"
	"github.com/Cloud-Developer-Department/hwcloud/channel/wechat/protocol"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
)

// WechatCredentials holds resolved bot credentials.
type WechatCredentials struct {
	Token     string
	BaseURL   string
	AccountID string
	UserID    string
}

func (c WechatCredentials) toProtocol() *protocol.Credentials {
	return &protocol.Credentials{Token: c.Token, BaseURL: c.BaseURL, AccountID: c.AccountID, UserID: c.UserID}
}

// ResolveWechatCredentials runs the QR code login flow (blocks until the
// user authorizes) and persists the bot credentials to settings.json —
// settings is the single credential source.
//
// localCreds carries the currently stored credentials (nil when none) —
// sent to the server so an already-bound bot answers binded_redirect and
// the stored session is reused without a new QR.
//
// onQR, onScanned, onVerifyCode, when non-nil, are forwarded to the login
// flow (an API-driven caller renders the QR and collects the pairing
// code; nil = terminal rendering and stdin prompt). onVerifyCode receives
// the login context — a cancelled login (disconnect) must unblock the
// pairing-code wait.
func ResolveWechatCredentials(ctx context.Context, localCreds *protocol.Credentials, onQR func(url string, expireIn int), onScanned func(), onVerifyCode func(ctx context.Context, isRetry bool) (string, error)) (WechatCredentials, error) {
	client := protocol.NewClient()
	baseURL := ""
	if localCreds != nil {
		baseURL = localCreds.BaseURL
	}
	qrIssuedAt := time.Now()
	creds, err := wechat.Login(ctx, client, wechat.LoginOptions{
		BaseURL:    baseURL,
		LocalCreds: localCreds,
		OnQRURL: func(url string) {
			// Cache the QR (URL + base64 PNG) so the frontend can
			// re-fetch it after a refresh. The ilinkai QR response has no
			// explicit expiry — the cache keeps a generous TTL; the
			// frontend stops counting down when the registration ends.
			if err := saveWechatQR(url, qrCacheTTL); err != nil {
				fmt.Fprintf(os.Stderr, "wechat: failed to cache QR: %v\n", err)
			}
			slog.Debug("wechat: QR code issued", "ttl", qrCacheTTL, "url", url)
			if onQR != nil {
				onQR(url, qrCacheTTL)
				return
			}
			fmt.Fprintln(os.Stderr)
			qrterminal.GenerateHalfBlock(url, qrterminal.L, os.Stderr)
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "  Scan this link in WeChat: %s\n", url)
			fmt.Fprintln(os.Stderr)
		},
		OnScanned: onScanned,
		OnExpired: func() {
			alive := time.Since(qrIssuedAt).Round(time.Second)
			slog.Debug("wechat: QR code expired", "alive", alive.String(), "ttl", qrCacheTTL)
			clearWechatQR()
		},
		OnVerifyCode: onVerifyCode,
	})
	if err != nil {
		return WechatCredentials{}, fmt.Errorf("wechat login: %w", err)
	}

	// Registration artifacts are configuration too — persist to
	// settings.json (the single credential source).
	wc := WechatCredentials{
		Token:     creds.Token,
		BaseURL:   creds.BaseURL,
		AccountID: creds.AccountID,
		UserID:    creds.UserID,
	}
	if err := saveWechatToSettings(wc); err != nil {
		return WechatCredentials{}, fmt.Errorf("wechat login: persist credentials: %w", err)
	}
	fmt.Fprintf(os.Stderr, "wechat: logged in — Account ID: %s\n", wc.AccountID)
	return wc, nil
}

// qrCacheTTL is the QR cache lifetime used for the expires_in field. Each
// QR expires after ~120s server-side (measured empirically); the TTL
// matches so the frontend countdown aligns with the real deadline. The
// cache is phase-guarded regardless.
const qrCacheTTL = 120

// ── QR cache ──

// wechatQRPath returns the QR cache paths under config.Dir()/channel/wechat
// directory (mirrors the feishu cache layout).
func wechatQRPath() (urlPath, imgPath, expiresAtPath string) {
	dir := channelDir("wechat")
	return filepath.Join(dir, "qr_url"), filepath.Join(dir, "qr_img_base64"), filepath.Join(dir, "qr_expires_at")
}

// saveWechatQR persists the registration QR (URL + base64 PNG image) and
// its expiry. Best-effort cache: a failed write only costs a
// re-registration.
func saveWechatQR(url string, expireIn int) error {
	urlPath, imgPath, expiresAtPath := wechatQRPath()
	if err := os.MkdirAll(filepath.Dir(urlPath), 0o755); err != nil {
		return err
	}
	png, err := qrcode.Encode(url, qrcode.Medium, 256)
	if err != nil {
		return err
	}
	if err := os.WriteFile(imgPath, []byte(base64.StdEncoding.EncodeToString(png)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(urlPath, []byte(url), 0o600); err != nil {
		return err
	}
	return os.WriteFile(expiresAtPath, []byte(strconv.FormatInt(time.Now().Unix()+int64(expireIn), 10)), 0o600)
}

// loadWechatQR reads the cached registration QR (empty strings and a
// zero expiry when none).
func loadWechatQR() (url, imgBase64 string, expiresAt int64) {
	urlPath, imgPath, expiresAtPath := wechatQRPath()
	if b, err := os.ReadFile(urlPath); err == nil {
		url = string(b)
	}
	if b, err := os.ReadFile(imgPath); err == nil {
		imgBase64 = string(b)
	}
	if b, err := os.ReadFile(expiresAtPath); err == nil {
		expiresAt, _ = strconv.ParseInt(string(b), 10, 64)
	}
	return url, imgBase64, expiresAt
}

// clearWechatQR removes the QR cache (registration finished, expired).
func clearWechatQR() {
	urlPath, imgPath, expiresAtPath := wechatQRPath()
	os.Remove(urlPath)
	os.Remove(imgPath)
	os.Remove(expiresAtPath)
}

// ── Settings ──

// saveWechatToSettings persists the wechat credentials into the settings
// file (channels.wechat). The "interface is configuration" path: a
// submission from the control panel is user-level config, so it lives in
// settings.json — the single credential source. Concurrency is handled by
// config.UpdateSettings (process-wide serialized read-modify-write).
func saveWechatToSettings(creds WechatCredentials) error {
	return config.UpdateSettings(func(raw map[string]json.RawMessage) error {
		channels := map[string]json.RawMessage{}
		if c, ok := raw["channels"]; ok {
			if err := json.Unmarshal(c, &channels); err != nil {
				return fmt.Errorf("wechat: parse settings channels: %w", err)
			}
		}
		wechat, err := json.Marshal(map[string]string{
			"token":      creds.Token,
			"base_url":   creds.BaseURL,
			"account_id": creds.AccountID,
			"user_id":    creds.UserID,
		})
		if err != nil {
			return fmt.Errorf("wechat: marshal credentials: %w", err)
		}
		channels["wechat"] = wechat
		raw["channels"], err = json.Marshal(channels)
		if err != nil {
			return fmt.Errorf("wechat: marshal channels: %w", err)
		}
		return nil
	})
}

// clearWechatFromSettings removes the channels.wechat key (all other
// settings fields preserved; the channels key itself dropped when empty).
func clearWechatFromSettings() error {
	return config.UpdateSettings(func(raw map[string]json.RawMessage) error {
		channels := map[string]json.RawMessage{}
		if c, ok := raw["channels"]; ok {
			if err := json.Unmarshal(c, &channels); err != nil {
				return fmt.Errorf("wechat: parse settings channels: %w", err)
			}
		}
		delete(channels, "wechat")
		if len(channels) == 0 {
			delete(raw, "channels")
		} else {
			raw["channels"], _ = json.Marshal(channels)
		}
		return nil
	})
}

// wechatConfigFromSettings converts a config.WechatConfig to the
// protocol credentials used by the channel package.
func wechatConfigFromSettings(cfg *config.WechatConfig) *protocol.Credentials {
	if cfg == nil {
		return nil
	}
	return &protocol.Credentials{
		Token:     cfg.Token,
		BaseURL:   cfg.BaseURL,
		AccountID: cfg.AccountID,
		UserID:    cfg.UserID,
	}
}
