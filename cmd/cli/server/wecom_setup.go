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

	"github.com/mdp/qrterminal/v3"
	"github.com/skip2/go-qrcode"

	"github.com/Cloud-Developer-Department/hwcloud/channel/wecom"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
)

// WecomCredentials holds the smart-robot credentials (BotID + Secret).
type WecomCredentials struct {
	BotID  string
	Secret string
}

// ResolveWecomCredentials runs the official QR authorization flow
// (blocks until the user scans with the WeCom app — the robot is
// created automatically) and persists the credentials to settings.json —
// settings is the single credential source.
//
// onQR, when non-nil, receives the QR content (an API-driven caller
// renders it); nil renders the QR in the terminal.
func ResolveWecomCredentials(ctx context.Context, onQR func(url string, expireIn int)) (WecomCredentials, error) {
	qr, err := wecom.GenerateQR(ctx)
	if err != nil {
		return WecomCredentials{}, fmt.Errorf("wecom qr: %w", err)
	}

	// Cache the QR (URL + base64 PNG) so the frontend can re-fetch it
	// after a refresh. The official flow has no explicit expiry in the
	// response — the cache TTL is a frontend countdown cap; the poll
	// itself times out after 5 minutes.
	if err := saveWecomQR(qr.AuthURL, wecomQRCacheTTL); err != nil {
		fmt.Fprintf(os.Stderr, "wecom: failed to cache QR: %v\n", err)
	}
	if onQR != nil {
		onQR(qr.AuthURL, wecomQRCacheTTL)
	} else {
		fmt.Fprintln(os.Stderr)
		qrterminal.GenerateHalfBlock(qr.AuthURL, qrterminal.L, os.Stderr)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintf(os.Stderr, "  Scan this with the WeCom app: %s\n", qr.AuthURL)
		fmt.Fprintf(os.Stderr, "  (or open the page: %s)\n", qr.PageURL)
		fmt.Fprintln(os.Stderr)
	}

	creds, err := wecom.PollQRResult(ctx, qr.SCode, nil)
	if err != nil {
		return WecomCredentials{}, fmt.Errorf("wecom qr scan: %w", err)
	}

	wc := WecomCredentials{BotID: creds.BotID, Secret: creds.Secret}
	if err := saveWecomToSettings(wc); err != nil {
		return WecomCredentials{}, fmt.Errorf("wecom qr: persist credentials: %w", err)
	}
	fmt.Fprintf(os.Stderr, "wecom: robot created — Bot ID: %s\n", wc.BotID)
	return wc, nil
}

// wecomQRCacheTTL is the QR cache lifetime used for expires_in (the
// official flow exposes no expiry; the poll bounds the real window at 5
// minutes — the TTL is a frontend countdown cap, phase-guarded anyway).
const wecomQRCacheTTL = 300

// ── QR cache ──

// wecomQRPath returns the QR cache paths under config.Dir()/channel/wecom
// directory (mirrors the feishu/wechat cache layout).
func wecomQRPath() (urlPath, imgPath, expiresAtPath string) {
	dir := channelDir("wecom")
	return filepath.Join(dir, "qr_url"), filepath.Join(dir, "qr_img_base64"), filepath.Join(dir, "qr_expires_at")
}

func saveWecomQR(url string, expireIn int) error {
	urlPath, imgPath, expiresAtPath := wecomQRPath()
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

func loadWecomQR() (url, imgBase64 string, expiresAt int64) {
	urlPath, imgPath, expiresAtPath := wecomQRPath()
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

func clearWecomQR() {
	urlPath, imgPath, expiresAtPath := wecomQRPath()
	os.Remove(urlPath)
	os.Remove(imgPath)
	os.Remove(expiresAtPath)
}

// ── Settings ──

// saveWecomToSettings persists the wecom credentials into the settings
// file (channels.wecom). "Interface is configuration": submissions from
// the control panel are user-level config in settings.json. Concurrency
// handled by config.UpdateSettings.
func saveWecomToSettings(creds WecomCredentials) error {
	return config.UpdateSettings(func(raw map[string]json.RawMessage) error {
		channels := map[string]json.RawMessage{}
		if c, ok := raw["channels"]; ok {
			if err := json.Unmarshal(c, &channels); err != nil {
				return fmt.Errorf("wecom: parse settings channels: %w", err)
			}
		}
		wecom, err := json.Marshal(map[string]string{
			"bot_id": creds.BotID,
			"secret": creds.Secret,
		})
		if err != nil {
			return fmt.Errorf("wecom: marshal credentials: %w", err)
		}
		channels["wecom"] = wecom
		raw["channels"], err = json.Marshal(channels)
		if err != nil {
			return fmt.Errorf("wecom: marshal channels: %w", err)
		}
		return nil
	})
}

// clearWecomFromSettings removes the channels.wecom key (all other
// settings fields preserved).
func clearWecomFromSettings() error {
	return config.UpdateSettings(func(raw map[string]json.RawMessage) error {
		channels := map[string]json.RawMessage{}
		if c, ok := raw["channels"]; ok {
			if err := json.Unmarshal(c, &channels); err != nil {
				return fmt.Errorf("wecom: parse settings channels: %w", err)
			}
		}
		delete(channels, "wecom")
		if len(channels) == 0 {
			delete(raw, "channels")
		} else {
			raw["channels"], _ = json.Marshal(channels)
		}
		return nil
	})
}

// wecomConfigFromSettings converts a config.WecomConfig to the
// credentials used by the channel package.
func wecomConfigFromSettings(cfg *config.WecomConfig) *wecom.BotCreds {
	if cfg == nil {
		return nil
	}
	return &wecom.BotCreds{BotID: cfg.BotID, Secret: cfg.Secret}
}
