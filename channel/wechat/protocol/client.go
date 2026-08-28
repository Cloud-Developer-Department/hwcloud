package protocol

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the fixed API base for QR code requests and, when
	// the login response carries no baseurl, for message traffic.
	DefaultBaseURL = "https://ilinkai.weixin.qq.com"
	// ChannelVersion identifies this client build (base_info + client version header).
	ChannelVersion = "0.1.0"
	// iLinkAppID is the application identifier header. The channel is
	// open to third-party AI clients — the reference implementations use
	// the generic value "bot" (verified end-to-end by the community Go
	// SDK), not a per-vendor app id.
	iLinkAppID = "bot"
	// iLinkClientVer is iLink-App-ClientVersion for 0.1.0 (0x00MMNNPP = 256).
	iLinkClientVer = "256"

	// Long-poll timeout: the server holds the request until new messages
	// or ~35s, whichever first. The client-level Timeout (45s) is the
	// backstop for a server that never responds — Go http body reads are
	// NOT cancelled by context, so a bare context deadline would still
	// leave the goroutine stuck (the feishu disconnect hang, structurally
	// avoided here).
	longPollTimeout  = 45 * time.Second
	apiTimeout       = 15 * time.Second
	defaultClientTO  = 45 * time.Second
	sessionExpiredCC = -14
)

// APIError is returned when the iLink API returns a non-zero ret/errcode
// or an HTTP error.
type APIError struct {
	Message    string
	HTTPStatus int
	ErrCode    int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ilink api: %s (http=%d, errcode=%d)", e.Message, e.HTTPStatus, e.ErrCode)
}

// IsSessionExpired reports whether the error is the session-timeout
// signal (errcode -14). The caller must re-run the QR login flow.
func (e *APIError) IsSessionExpired() bool {
	return e.ErrCode == sessionExpiredCC
}

// CDNBaseURL hosts encrypted media upload/download. A var (not const):
// the upstream plugin exposes it as configuration, and tests redirect it
// to a fake server.
var CDNBaseURL = "https://novac2c.cdn.weixin.qq.com/c2c"

// Client wraps HTTP calls to the iLink API.
type Client struct {
	HTTP *http.Client
	// BotAgent identifies the app driving this bot; sent as
	// base_info.bot_agent (like HTTP User-Agent). Empty = DefaultBotAgent.
	BotAgent string
}

// NewClient creates a protocol client. The Timeout is a hard backstop
// for hung servers (see longPollTimeout) — it bounds every request,
// including long-poll body reads that a context cancel cannot interrupt.
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: defaultClientTO}}
}

// DefaultBotAgent is used when no bot_agent is configured or the
// configured value is invalid.
const DefaultBotAgent = "WeChatBot/" + ChannelVersion

// bot_agent grammar (matches openclaw-wechat): product *( SP product ),
// product = name "/" version [ SP "(" comment ")" ].
var botAgentRe = regexp.MustCompile(
	`^[A-Za-z0-9_.\-]{1,32}/[A-Za-z0-9_.+\-]{1,32}( \([\x20-\x27\x2A-\x7E]{1,64}\))?` +
		`( [A-Za-z0-9_.\-]{1,32}/[A-Za-z0-9_.+\-]{1,32}( \([\x20-\x27\x2A-\x7E]{1,64}\))?)*$`,
)

// SanitizeBotAgent validates a user-supplied bot_agent into a wire-safe
// string. Any invalid input falls back to DefaultBotAgent wholesale.
func SanitizeBotAgent(raw string) string {
	const maxLen = 256
	normalized := strings.Join(strings.Fields(raw), " ")
	if normalized == "" || len(normalized) > maxLen || !botAgentRe.MatchString(normalized) {
		return DefaultBotAgent
	}
	return normalized
}

func (c *Client) baseInfo() map[string]string {
	agent := c.BotAgent
	if agent == "" {
		agent = DefaultBotAgent
	}
	return map[string]string{"channel_version": ChannelVersion, "bot_agent": agent}
}

// randomWechatUIN generates the X-WECHAT-UIN header value: a random
// uint32 as a decimal string, base64-encoded.
func randomWechatUIN() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	val := binary.BigEndian.Uint32(buf[:])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(val), 10)))
}

func commonHeaders() http.Header {
	h := http.Header{}
	h.Set("iLink-App-Id", iLinkAppID)
	h.Set("iLink-App-ClientVersion", iLinkClientVer)
	return h
}

func authHeaders(token string) http.Header {
	h := commonHeaders()
	h.Set("Content-Type", "application/json")
	h.Set("AuthorizationType", "ilink_bot_token")
	h.Set("Authorization", "Bearer "+token)
	h.Set("X-WECHAT-UIN", randomWechatUIN())
	return h
}

// GetQRCode requests a new QR code for login.
//
// localTokenList carries known local bot tokens (newest first, up to 10)
// so the server can answer binded_redirect for an already-bound bot
// instead of issuing a duplicate session.
func (c *Client) GetQRCode(ctx context.Context, baseURL string, localTokenList []string) (*QRCodeResponse, error) {
	if localTokenList == nil {
		localTokenList = []string{}
	}
	body, _ := json.Marshal(map[string]any{"local_token_list": localTokenList})
	u := baseURL + "/ilink/bot/get_bot_qrcode?bot_type=3"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range commonHeaders() {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get_bot_qrcode: %w", err)
	}
	defer resp.Body.Close()
	var result QRCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("get_bot_qrcode decode: %w", err)
	}
	return &result, nil
}

// PollQRStatus polls the QR code scan status.
//
// verifyCode submits a pairing code after the server answered
// need_verifycode (the digits shown in WeChat on the user's phone).
// Pass "" when none. The poll is a long request (~35s) — the client
// Timeout bounds it; a timeout surfaces as an error, and the caller
// treats it as "still waiting".
func (c *Client) PollQRStatus(ctx context.Context, baseURL, qrcode, verifyCode string) (*QRStatusResponse, error) {
	u := baseURL + "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(qrcode)
	if verifyCode != "" {
		u += "&verify_code=" + url.QueryEscape(verifyCode)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range commonHeaders() {
		req.Header[k] = v
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result QRStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("get_qrcode_status decode: %w", err)
	}
	return &result, nil
}

// apiPost sends a POST to the iLink API and parses the response. The
// context deadline (timeout) is applied on top of the client Timeout —
// both bound the request; the context also enables external cancellation.
func (c *Client) apiPost(ctx context.Context, baseURL, endpoint, token string, body any, timeout time.Duration) (json.RawMessage, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(httpCtx, http.MethodPost, baseURL+endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	for k, v := range authHeaders(token) {
		req.Header[k] = v
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read body: %w", endpoint, err)
	}
	if resp.StatusCode >= 400 {
		return nil, &APIError{Message: string(raw), HTTPStatus: resp.StatusCode}
	}

	var check struct {
		Ret     int    `json:"ret"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	_ = json.Unmarshal(raw, &check)
	if check.Ret != 0 || check.ErrCode != 0 {
		code := check.ErrCode
		if code == 0 {
			code = check.Ret
		}
		msg := check.ErrMsg
		if msg == "" {
			msg = fmt.Sprintf("ret=%d", check.Ret)
		}
		return nil, &APIError{Message: msg, HTTPStatus: resp.StatusCode, ErrCode: code}
	}
	return json.RawMessage(raw), nil
}

// GetUpdates performs a long-poll for new messages. On client-side
// timeout the server returned nothing within longPollTimeout: the caller
// simply retries (normal for long-poll; the empty response shape is
// equivalent to "no messages yet").
func (c *Client) GetUpdates(ctx context.Context, baseURL, token, cursor string) (*GetUpdatesResponse, error) {
	body := map[string]any{
		"get_updates_buf": cursor,
		"base_info":       c.baseInfo(),
	}
	raw, err := c.apiPost(ctx, baseURL, "/ilink/bot/getupdates", token, body, longPollTimeout)
	if err != nil {
		return nil, err
	}
	var result GetUpdatesResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("getupdates decode: %w", err)
	}
	return &result, nil
}

// SendMessage sends a message through the iLink API.
func (c *Client) SendMessage(ctx context.Context, baseURL, token string, msg any) error {
	body := map[string]any{"msg": msg, "base_info": c.baseInfo()}
	_, err := c.apiPost(ctx, baseURL, "/ilink/bot/sendmessage", token, body, apiTimeout)
	return err
}

// GetConfig gets the typing ticket for a user.
func (c *Client) GetConfig(ctx context.Context, baseURL, token, userID, contextToken string) (*GetConfigResponse, error) {
	body := map[string]any{
		"ilink_user_id": userID,
		"context_token": contextToken,
		"base_info":     c.baseInfo(),
	}
	raw, err := c.apiPost(ctx, baseURL, "/ilink/bot/getconfig", token, body, apiTimeout)
	if err != nil {
		return nil, err
	}
	var result GetConfigResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("getconfig decode: %w", err)
	}
	return &result, nil
}

// SendTyping sends (status 1) or cancels (status 2) the typing indicator.
func (c *Client) SendTyping(ctx context.Context, baseURL, token, userID, ticket string, status int) error {
	body := map[string]any{
		"ilink_user_id": userID,
		"typing_ticket": ticket,
		"status":        status,
		"base_info":     c.baseInfo(),
	}
	_, err := c.apiPost(ctx, baseURL, "/ilink/bot/sendtyping", token, body, apiTimeout)
	return err
}

// NotifyStart notifies the server that this client is coming online
// (non-fatal; the server keeps delivering queued messages regardless).
func (c *Client) NotifyStart(ctx context.Context, baseURL, token string) error {
	body := map[string]any{"base_info": c.baseInfo()}
	_, err := c.apiPost(ctx, baseURL, "/ilink/bot/msg/notifystart", token, body, apiTimeout)
	return err
}

// NotifyStop notifies the server that this client is going offline
// (non-fatal).
func (c *Client) NotifyStop(ctx context.Context, baseURL, token string) error {
	body := map[string]any{"base_info": c.baseInfo()}
	_, err := c.apiPost(ctx, baseURL, "/ilink/bot/msg/notifystop", token, body, apiTimeout)
	return err
}

// GetUploadURL requests a CDN upload URL for encrypted media.
func (c *Client) GetUploadURL(ctx context.Context, baseURL, token string, req GetUploadURLRequest) (*GetUploadURLResponse, error) {
	body := map[string]any{
		"filekey":       req.FileKey,
		"media_type":    req.MediaType,
		"to_user_id":    req.ToUserID,
		"rawsize":       req.RawSize,
		"rawfilemd5":    req.RawFileMD5,
		"filesize":      req.FileSize,
		"no_need_thumb": req.NoNeedThumb,
		"aeskey":        req.AESKey,
		"base_info":     c.baseInfo(),
	}
	raw, err := c.apiPost(ctx, baseURL, "/ilink/bot/getuploadurl", token, body, apiTimeout)
	if err != nil {
		return nil, err
	}
	var result GetUploadURLResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("getuploadurl decode: %w", err)
	}
	return &result, nil
}

// UploadToCDN uploads encrypted bytes to the CDN with retry (up to 3
// attempts). Client errors (4xx) abort immediately; server errors retry.
// Returns the download encrypted_query_param from the x-encrypted-param
// header.
func (c *Client) UploadToCDN(ctx context.Context, cdnURL string, ciphertext []byte) (string, error) {
	const maxRetries = 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cdnURL, bytes.NewReader(ciphertext))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/octet-stream")

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("cdn upload attempt %d: %w", attempt, err)
			continue
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			errMsg := resp.Header.Get("x-error-message")
			if errMsg == "" {
				errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			resp.Body.Close()
			return "", fmt.Errorf("cdn upload client error %d: %s", resp.StatusCode, errMsg)
		}
		if resp.StatusCode != http.StatusOK {
			errMsg := resp.Header.Get("x-error-message")
			lastErr = fmt.Errorf("cdn upload server error %d: %s", resp.StatusCode, errMsg)
			resp.Body.Close()
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		downloadParam := resp.Header.Get("x-encrypted-param")
		if downloadParam == "" {
			lastErr = fmt.Errorf("cdn upload response missing x-encrypted-param header")
			continue
		}
		return downloadParam, nil
	}
	return "", fmt.Errorf("cdn upload failed after %d attempts: %w", maxRetries, lastErr)
}
