package wechat

import (
	"context"
	"fmt"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/channel/wechat/protocol"
)

// LoginOptions configures the QR login flow.
type LoginOptions struct {
	BaseURL string // default protocol.DefaultBaseURL
	// LocalCreds carries the currently stored credentials (nil when none).
	// Its token is sent to the server so an already-bound bot answers
	// binded_redirect, in which case LocalCreds are returned as-is (the
	// session is still valid — no duplicate session is issued).
	LocalCreds *protocol.Credentials

	// OnQRURL receives the QR content (URL). nil = render the QR in the
	// terminal (no-frontend entry point).
	OnQRURL func(url string)
	// OnScanned fires when the QR has been scanned — the frontend shows
	// "scanned, confirm on your phone".
	OnScanned func()
	// OnExpired fires when the QR expired (login will abort).
	OnExpired func()
	// OnVerifyCode is called when the server requires a pairing code (the
	// digits shown in WeChat on the user's phone). isRetry is true when a
	// previously submitted code was rejected. The context is the login
	// context — a cancelled login (disconnect) must unblock the wait.
	// nil = stdin prompt.
	OnVerifyCode func(ctx context.Context, isRetry bool) (string, error)
}

// pollInterval is the QR status poll cadence; a package var so tests can
// shrink it.
var pollInterval = 2 * time.Second

// Login runs the QR code login flow and returns bot credentials.
// Blocks until the user authorizes (or ctx is cancelled).
//
// State machine (8 states, polled every 2s): wait → scaned → confirmed;
// need_verifycode asks the user for the pairing code shown on the phone;
// verify_code_blocked and expired abort the login (single QR, no refresh);
// scaned_but_redirect switches the poll host (IDC redirect);
// binded_redirect reuses the local credentials for an already-bound bot.
func Login(ctx context.Context, client *protocol.Client, opts LoginOptions) (*protocol.Credentials, error) {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = protocol.DefaultBaseURL
	}

	// Send the known local token so the server can answer
	// binded_redirect instead of issuing a duplicate session for an
	// already-bound bot.
	var localTokens []string
	if opts.LocalCreds != nil && opts.LocalCreds.Token != "" {
		localTokens = []string{opts.LocalCreds.Token}
	}

	qr, err := client.GetQRCode(ctx, baseURL, localTokens)
	if err != nil {
		return nil, fmt.Errorf("get qr code: %w", err)
	}
	if opts.OnQRURL != nil {
		opts.OnQRURL(qr.QRCodeImgURL)
	} else {
		fmt.Printf("Scan this URL in WeChat: %s\n", qr.QRCodeImgURL)
	}

	lastStatus := ""
	currentPollBaseURL := baseURL
	pendingVerifyCode := ""

	// Poll loop. Each poll is a long request (~35s); a network error is
	// treated as "still waiting" and retried.
	for {
		status, err := client.PollQRStatus(ctx, currentPollBaseURL, qr.QRCode, pendingVerifyCode)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			time.Sleep(pollInterval) // transient — keep waiting
			continue
		}

		if status.Status != lastStatus {
			lastStatus = status.Status
			switch status.Status {
			case "scaned":
				// A pending pairing code that leads back to scaned was
				// accepted — clear it so subsequent polls are clean.
				pendingVerifyCode = ""
				if opts.OnScanned != nil {
					opts.OnScanned()
				}
			case "confirmed":
				// no callback; handled below
			case "expired":
				if opts.OnExpired != nil {
					opts.OnExpired()
				}
			}
		}

		switch status.Status {
		case "confirmed":
			if status.BotToken == "" || status.BotID == "" || status.UserID == "" {
				return nil, fmt.Errorf("login confirmed but credentials missing (bot_token=%q bot_id=%q user_id=%q)",
					status.BotToken, status.BotID, status.UserID)
			}
			resolvedBase := baseURL
			if status.BaseURL != "" {
				resolvedBase = status.BaseURL
			}
			return &protocol.Credentials{
				Token:     status.BotToken,
				BaseURL:   resolvedBase,
				AccountID: status.BotID,
				UserID:    status.UserID,
			}, nil

		case "need_verifycode":
			// The phone shows a pairing code; ask the user and
			// re-poll immediately with it attached.
			isRetry := pendingVerifyCode != ""
			prompt := opts.OnVerifyCode
			if prompt == nil {
				prompt = readVerifyCode
			}
			code, err := prompt(ctx, isRetry)
			if err != nil {
				return nil, fmt.Errorf("read pairing code: %w", err)
			}
			pendingVerifyCode = code
			continue

		case "verify_code_blocked":
			// Too many wrong pairing codes — the QR is dead; abort.
			return nil, fmt.Errorf("wechat: pairing code blocked — too many wrong attempts")

		case "binded_redirect":
			// The bot is already bound to this client: the existing
			// session is still valid — reuse the stored credentials
			// instead of issuing a duplicate.
			if opts.LocalCreds != nil && opts.LocalCreds.Token != "" {
				return opts.LocalCreds, nil
			}
			return nil, fmt.Errorf("server reports bot already bound (binded_redirect) but no local credentials were found")

		case "scaned_but_redirect":
			// IDC redirect: continue polling on the new host.
			if status.RedirectHost != "" {
				currentPollBaseURL = "https://" + status.RedirectHost
			}
			time.Sleep(pollInterval)
			continue

		case "expired":
			return nil, fmt.Errorf("wechat: qr code expired — login aborted")
		}

		time.Sleep(pollInterval)
	}
}

// readVerifyCode is the default pairing-code prompt: read a line from
// stdin (the --channel wechat terminal entry point). ctx is accepted for
// signature uniformity; a terminal prompt cannot be interrupted by it.
func readVerifyCode(ctx context.Context, isRetry bool) (string, error) {
	prompt := "Enter the pairing code shown in WeChat on your phone: "
	if isRetry {
		prompt = "Code mismatch — enter the pairing code shown in WeChat again: "
	}
	fmt.Print(prompt)
	var code string
	if _, err := fmt.Scanln(&code); err != nil {
		return "", err
	}
	return code, nil
}
