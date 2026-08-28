package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
	"github.com/mdp/qrterminal/v3"
	"github.com/skip2/go-qrcode"

	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
)

// FeishuCredentials holds resolved app credentials.
type FeishuCredentials struct {
	AppID     string
	AppSecret string
}

// ResolveFeishuCredentials runs the QR code registration flow (blocks
// until the user authorizes) and persists the created app's credentials
// to settings.json — settings is the single credential source.
//
// onQR, when non-nil, receives the registration QR info (an API-driven
// caller renders it for the user instead of the terminal); when nil the
// QR is printed to stderr.
func ResolveFeishuCredentials(ctx context.Context, onQR func(url string, expireIn int)) (FeishuCredentials, error) {
	fmt.Fprintln(os.Stderr, "feishu: no credentials found. Starting one-click app registration...")
	return registerFeishuApp(ctx, onQR)
}

// ── QR cache ──

// feishuQRPath returns the QR cache paths under config.Dir()/channel/feishu
// directory: the registration URL, the QR image as base64-encoded PNG,
// and the absolute expiry timestamp (Unix seconds). Cached so the
// frontend can re-fetch the QR after a refresh — the connect endpoint is
// idempotent while registering and does not re-issue the URL — and so it
// can restart its countdown from the remaining lifetime (expires_at
// instead of a total expireIn, which would be meaningless after a
// refresh).
func feishuQRPath() (urlPath, imgPath, expiresAtPath string) {
	dir := channelDir("feishu")
	return filepath.Join(dir, "qr_url"), filepath.Join(dir, "qr_img_base64"), filepath.Join(dir, "qr_expires_at")
}

// saveFeishuQR persists the registration QR (URL + base64 PNG image) and
// its expiry. Best-effort cache: a failed write only costs a
// re-registration.
func saveFeishuQR(url string, expireIn int) error {
	urlPath, imgPath, expiresAtPath := feishuQRPath()
	if err := os.MkdirAll(filepath.Dir(urlPath), 0o755); err != nil {
		return err
	}
	png, err := qrcode.Encode(url, qrcode.Medium, 256)
	if err != nil {
		return err
	}
	// Image and URL first, expiry last — the expiry file is the "ready"
	// marker (all three must exist for the cache to be complete).
	if err := os.WriteFile(imgPath, []byte(base64.StdEncoding.EncodeToString(png)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(urlPath, []byte(url), 0o600); err != nil {
		return err
	}
	return os.WriteFile(expiresAtPath, []byte(strconv.FormatInt(time.Now().Unix()+int64(expireIn), 10)), 0o600)
}

// loadFeishuQR reads the cached registration QR (empty strings and a zero
// expiry when none).
func loadFeishuQR() (url, imgBase64 string, expiresAt int64) {
	urlPath, imgPath, expiresAtPath := feishuQRPath()
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

// clearFeishuQR removes the QR cache (registration finished, expired).
func clearFeishuQR() {
	urlPath, imgPath, expiresAtPath := feishuQRPath()
	os.Remove(urlPath)
	os.Remove(imgPath)
	os.Remove(expiresAtPath)
}

// saveFeishuToSettings persists the feishu credentials into the settings
// file (channels.feishu). The "interface is configuration" path: a
// submission from the control panel is user-level config, so it lives in
// settings.json — the single credential source — and takes effect for
// the running process via the manager's in-memory copy (no restart
// needed). Concurrency is handled by config.UpdateSettings (process-wide
// serialized read-modify-write).
func saveFeishuToSettings(appID, appSecret string) error {
	return config.UpdateSettings(func(raw map[string]json.RawMessage) error {
		channels := map[string]json.RawMessage{}
		if c, ok := raw["channels"]; ok {
			if err := json.Unmarshal(c, &channels); err != nil {
				return fmt.Errorf("feishu: parse settings channels: %w", err)
			}
		}
		feishu, err := json.Marshal(map[string]string{"app_id": appID, "app_secret": appSecret})
		if err != nil {
			return fmt.Errorf("feishu: marshal credentials: %w", err)
		}
		channels["feishu"] = feishu
		raw["channels"], err = json.Marshal(channels)
		if err != nil {
			return fmt.Errorf("feishu: marshal channels: %w", err)
		}
		return nil
	})
}

// ── Registration flow ──

func registerFeishuApp(ctx context.Context, onQR func(url string, expireIn int)) (FeishuCredentials, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	result, err := registration.RegisterApp(ctx, &registration.Options{
		AppPreset: &registration.AppPreset{
			Name: "hwcloud-bot",
			Desc: "AI coding agent powered by hwcloud",
		},
		Addons: &registration.AppAddons{
			Scopes: registration.AppAddonsScopes{
				Tenant: []string{
					"im:message",
					"im:message:send_as_bot",
				},
			},
			Events: registration.AppAddonsEvents{
				Items: registration.AppAddonsEventItems{
					Tenant: []string{
						"im.message.receive_v1",
						"card.action.trigger",
					},
				},
			},
		},
		OnQRCode: func(info *registration.QRCodeInfo) {
			// Cache the QR (URL + base64 PNG) so the frontend can
			// re-fetch it after a refresh — the connect endpoint is
			// idempotent while registering and does not re-issue it.
			if err := saveFeishuQR(info.URL, info.ExpireIn); err != nil {
				fmt.Fprintf(os.Stderr, "feishu: failed to cache QR: %v\n", err)
			}
			if onQR != nil {
				// API-driven caller renders the QR for the user.
				onQR(info.URL, info.ExpireIn)
				return
			}
			// Terminal mode: render the QR code inline.
			fmt.Fprintln(os.Stderr)
			qrterminal.GenerateHalfBlock(info.URL, qrterminal.L, os.Stderr)
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "  Open this link in Feishu: %s\n", info.URL)
			fmt.Fprintf(os.Stderr, "  (expires in %d seconds)\n", info.ExpireIn)
			fmt.Fprintln(os.Stderr)
		},
		OnStatusChange: func(info *registration.StatusChangeInfo) {
			// Quiet polling; no console spam.
			_ = info
		},
	})
	if err != nil {
		return FeishuCredentials{}, fmt.Errorf("feishu registration: %w", err)
	}

	creds := FeishuCredentials{
		AppID:     result.ClientID,
		AppSecret: result.ClientSecret,
	}
	// Registration artifacts are configuration too — persist to
	// settings.json (the single credential source).
	if err := saveFeishuToSettings(creds.AppID, creds.AppSecret); err != nil {
		return FeishuCredentials{}, fmt.Errorf("feishu registration: persist credentials: %w", err)
	}

	fmt.Fprintf(os.Stderr, "feishu: app created — App ID: %s\n", creds.AppID)
	fmt.Fprintln(os.Stderr, "feishu: credentials saved to settings.json (channels.feishu)")

	return creds, nil
}
