// Package wecom implements channel.Channel for WeCom (企业微信) smart
// robots via the official long-connection API (wss://openws.work.weixin.
// qq.com).
//
// Credentials (BotID + Secret) come from either the admin console (manual
// config) or the official QR authorization flow: the robot is created
// automatically when the user scans with the WeCom app — the same
// one-scan-create model as personal WeChat's ilinkai channel. The
// endpoints below are the official ones used by the open-source
// wecom-cli project.
package wecom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"time"
)

// QR authorization endpoints (official, from the open-source wecom-cli).
// Vars (not consts) so tests can redirect them to a fake server.
var (
	qrGenerateURL = "https://work.weixin.qq.com/ai/qc/generate"
	qrQueryURL    = "https://work.weixin.qq.com/ai/qc/query_result"
	qrCodePage    = "https://work.weixin.qq.com/ai/qc/gen"

	// Source identifies this client to the QR service (observability /
	// quota attribution). The official CLI uses "wecom_cli_external".
	source = "bot"

	pollInterval = 3 * time.Second
	pollTimeout  = 5 * time.Minute
)

// BotCreds are the credentials issued by the QR flow (or the admin
// console): the smart robot's identity and long-connection secret.
type BotCreds struct {
	BotID  string
	Secret string
}

// qrGenerateResponse is the shape of GET /ai/qc/generate.
type qrGenerateResponse struct {
	Data *struct {
		SCode   string `json:"scode"`
		AuthURL string `json:"auth_url"`
	} `json:"data"`
}

// qrQueryResponse is the shape of GET /ai/qc/query_result.
type qrQueryResponse struct {
	Data *struct {
		Status  string `json:"status"`
		BotInfo *struct {
			BotID  string `json:"botid"`
			Secret string `json:"secret"`
		} `json:"bot_info"`
	} `json:"data"`
}

// QRCode carries the registration QR (auth_url is what the WeCom app
// scans; pageURL is a browser-fallback page) and the polling scode.
type QRCode struct {
	AuthURL string
	PageURL string
	SCode   string
}

var qrClient = &http.Client{Timeout: 30 * time.Second}

// GenerateQR starts the QR authorization flow: returns the QR to render
// (auth_url) and the scode to poll. The user scans with the WeCom app
// and confirms; the robot is created automatically.
func GenerateQR(ctx context.Context) (*QRCode, error) {
	u := fmt.Sprintf("%s?source=%s&plat=%d", qrGenerateURL, url.QueryEscape(source), platCode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := qrClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wecom qr generate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("wecom qr generate: HTTP %d", resp.StatusCode)
	}
	var out qrGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("wecom qr generate decode: %w", err)
	}
	if out.Data == nil || out.Data.SCode == "" || out.Data.AuthURL == "" {
		return nil, fmt.Errorf("wecom qr generate: missing scode/auth_url")
	}
	return &QRCode{
		AuthURL: out.Data.AuthURL,
		PageURL: fmt.Sprintf("%s?source=%s&scode=%s", qrCodePage, url.QueryEscape(source), url.QueryEscape(out.Data.SCode)),
		SCode:   out.Data.SCode,
	}, nil
}

// PollQRResult polls the scan result until the robot is created
// (status "success") or the timeout expires. onStatus, when non-nil,
// receives every non-success status (e.g. waiting for scan) so the
// frontend can show progress.
func PollQRResult(ctx context.Context, scode string, onStatus func(status string)) (*BotCreds, error) {
	deadline := time.Now().Add(pollTimeout)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("wecom qr scan timed out (%s)", pollTimeout)
		}
		u := fmt.Sprintf("%s?scode=%s", qrQueryURL, url.QueryEscape(scode))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := qrClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			time.Sleep(pollInterval) // transient network error — keep polling
			continue
		}
		var out qrQueryResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			time.Sleep(pollInterval)
			continue
		}
		if decodeErr != nil {
			time.Sleep(pollInterval)
			continue
		}
		if out.Data == nil {
			time.Sleep(pollInterval)
			continue
		}
		if out.Data.Status == "success" {
			if out.Data.BotInfo == nil || out.Data.BotInfo.BotID == "" || out.Data.BotInfo.Secret == "" {
				return nil, fmt.Errorf("wecom qr scan succeeded but bot credentials missing")
			}
			return &BotCreds{BotID: out.Data.BotInfo.BotID, Secret: out.Data.BotInfo.Secret}, nil
		}
		if onStatus != nil {
			onStatus(out.Data.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// platCode mirrors wecom-cli: mac=1, windows=2, linux=3, else 0.
func platCode() int {
	switch runtime.GOOS {
	case "darwin":
		return 1
	case "windows":
		return 2
	case "linux":
		return 3
	}
	return 0
}
