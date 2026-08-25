// Package wechat implements channel.Channel for personal WeChat via the
// Tencent official ilinkai channel (ilinkai.weixin.qq.com) — HTTP
// long-poll, no SDK. Login is QR-based with a pairing-code step; message
// traffic is a getupdates long-poll loop with sendmessage replies.
//
// The protocol has no card/message-edit API, so replies are plain text
// (4000-byte chunks); media references ([file: /path]) in reply text are
// uploaded to the CDN and sent as media items.
package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/channel"
	"github.com/Cloud-Developer-Department/hwcloud/channel/wechat/protocol"
)

// ErrSessionExpired is returned by Start when the bot session timed out
// server-side (errcode -14): the caller must clear credentials and re-run
// the QR login flow.
var ErrSessionExpired = fmt.Errorf("wechat: session expired — re-login required")

// Channel implements channel.Channel for WeChat via the ilinkai
// long-poll protocol.
type Channel struct {
	creds    *protocol.Credentials
	client   *protocol.Client
	mediaDir string // base dir for downloaded media ($profile/channel/wechat/media)

	// contextTokens remembers the per-user context_token seen in
	// incoming messages — replies MUST carry the latest one.
	contextTokens sync.Map // userID → contextToken
	cursor        string

	onReady        func()
	onReconnecting func()
	onError        func(err error)
}

// New returns a WeChat Channel bound to creds. mediaDir is the base
// directory for downloaded media; created on demand.
func New(creds *protocol.Credentials, mediaDir string) *Channel {
	return &Channel{
		creds:    creds,
		client:   protocol.NewClient(),
		mediaDir: mediaDir,
	}
}

// SetOnReady registers the ready callback (nil clears it). Fired on the
// first successful getupdates poll — the equivalent of a WebSocket
// "connected" event (the long-poll loop being live means the server
// accepts our session).
func (c *Channel) SetOnReady(f func()) { c.onReady = f }

// SetOnReconnecting registers the reconnecting callback (nil clears it).
// Fired when a poll fails and the loop backs off.
func (c *Channel) SetOnReconnecting(f func()) { c.onReconnecting = f }

// SetOnError registers the error callback (nil clears it). Fired on
// poll failures; used by the manager to surface first-connect failures.
func (c *Channel) SetOnError(f func(err error)) { c.onError = f }

// Name implements channel.Channel.
func (c *Channel) Name() string { return "wechat" }

// Start implements channel.Channel. Runs the getupdates long-poll loop
// until ctx is cancelled; returns ErrSessionExpired when the bot session
// timed out (caller must re-login).
func (c *Channel) Start(ctx context.Context, handler channel.MessageHandler) error {
	creds := c.creds

	// Coming online — non-fatal; the server keeps delivering queued
	// messages either way.
	if err := c.client.NotifyStart(ctx, creds.BaseURL, creds.Token); err != nil {
		c.reportError(fmt.Errorf("notifystart (ignored): %w", err))
	}
	defer func() {
		// Going offline. Fresh context: ctx may already be cancelled here.
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.client.NotifyStop(stopCtx, creds.BaseURL, creds.Token); err != nil {
			c.reportError(fmt.Errorf("notifystop (ignored): %w", err))
		}
	}()

	retryDelay := time.Second
	ready := false
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		updates, err := c.client.GetUpdates(ctx, creds.BaseURL, creds.Token, c.cursor)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var apiErr *protocol.APIError
			if errors.As(err, &apiErr) && apiErr.IsSessionExpired() {
				return ErrSessionExpired
			}
			if ready {
				if c.onReconnecting != nil {
					c.onReconnecting()
				}
			} else if c.onError != nil {
				c.onError(err)
			}
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, 10*time.Second)
			continue
		}
		retryDelay = time.Second
		if !ready {
			ready = true
			if c.onReady != nil {
				c.onReady()
			}
		}

		if updates.GetUpdatesBuf != "" {
			c.cursor = updates.GetUpdatesBuf
		}
		for _, raw := range updates.Msgs {
			var wire protocol.WireMessage
			if err := json.Unmarshal(raw, &wire); err != nil {
				continue // malformed message — skip, keep the cursor advancing
			}
			c.rememberContext(&wire)
			if wire.MessageType != protocol.MessageTypeUser {
				continue // bot echoes and system messages
			}
			msg := c.toIncoming(ctx, &wire)
			if msg == nil {
				continue
			}
			handler(ctx, *msg, c.buildReply(&wire))
		}
	}
}

// Stop implements channel.Channel.
func (c *Channel) Stop() error { return nil }

// SendTyping shows the typing indicator for a user (best-effort).
func (c *Channel) SendTyping(ctx context.Context, userID string) error {
	ct, ok := c.contextTokens.Load(userID)
	if !ok {
		return fmt.Errorf("no context_token for user %s", userID)
	}
	creds := c.creds
	config, err := c.client.GetConfig(ctx, creds.BaseURL, creds.Token, userID, ct.(string))
	if err != nil {
		return err
	}
	if config.TypingTicket == "" {
		return nil
	}
	return c.client.SendTyping(ctx, creds.BaseURL, creds.Token, userID, config.TypingTicket, 1)
}

// ── Normalization ──

// toIncoming converts a wire message to a channel.IncomingMessage. Media
// items are downloaded and saved under mediaDir; each becomes a marker in
// Text ([image: path] / [file: path], [video] / [voice] markers without
// download — videos are large and would stall the long-poll loop). A
// download failure degrades to a bare marker rather than blocking the
// message flow.
func (c *Channel) toIncoming(ctx context.Context, wire *protocol.WireMessage) *channel.IncomingMessage {
	text := c.renderText(ctx, wire)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	ts := time.UnixMilli(wire.CreateTimeMs)
	return &channel.IncomingMessage{
		ID:        strconv.FormatInt(wire.MessageID, 10),
		ChatID:    wire.FromUserID, // private chats only — no group support
		ChatType:  "private",
		UserID:    wire.FromUserID,
		UserName:  wire.FromUserID, // wire carries no display name
		Text:      text,
		Timestamp: ts,
		Raw:       wire,
	}
}

// renderText builds the message text: text items verbatim, media items as
// markers (with the downloaded file path when available).
func (c *Channel) renderText(ctx context.Context, wire *protocol.WireMessage) string {
	var parts []string
	for i, item := range wire.ItemList {
		switch item.Type {
		case protocol.ItemText:
			if item.TextItem != nil {
				parts = append(parts, item.TextItem.Text)
			}
		case protocol.ItemImage:
			parts = append(parts, c.mediaMarker(ctx, item.ImageItem.Media, item.ImageItem.AESKey, wire, i, "image", ".jpg", "image"))
		case protocol.ItemVoice:
			// v1: no silk transcode (needs ffmpeg/wasm) — marker only.
			parts = append(parts, "[voice]")
		case protocol.ItemFile:
			ext := ""
			if item.FileItem != nil && item.FileItem.FileName != "" {
				ext = filepath.Ext(item.FileItem.FileName)
			}
			parts = append(parts, c.mediaMarker(ctx, item.FileItem.Media, "", wire, i, "file", ext, "file"))
		case protocol.ItemVideo:
			// Videos are large (tens of MB) — downloading them inline
			// would stall the long-poll loop for seconds-to-minutes.
			// Marker only; the agent cannot consume video anyway.
			parts = append(parts, "[video]")
		}
	}
	return strings.Join(parts, "\n")
}

// mediaMarker downloads one media item and returns its marker
// ([kind: /abs/path] or bare [kind] when the download failed).
func (c *Channel) mediaMarker(ctx context.Context, media *protocol.CDNMedia, keyOverride string, wire *protocol.WireMessage, idx int, kind, ext, marker string) string {
	path, err := c.downloadMedia(ctx, media, keyOverride, wire, idx, kind, ext)
	if err != nil {
		return "[" + marker + "]"
	}
	return "[" + marker + ": " + path + "]"
}

// downloadMedia fetches, decrypts, and stores a media file under
// mediaDir/YYYYMMDD/<messageID>_<idx>.<ext>.
func (c *Channel) downloadMedia(ctx context.Context, media *protocol.CDNMedia, keyOverride string, wire *protocol.WireMessage, idx int, kind, ext string) (string, error) {
	if c.mediaDir == "" {
		return "", fmt.Errorf("no media dir")
	}
	// Bounded by the connection context (a disconnect kills in-flight
	// downloads) with a 2-minute cap on top for the download itself.
	dlCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	data, err := cdnDownload(dlCtx, media, keyOverride)
	if err != nil {
		return "", err
	}

	day := time.Now().Format("20060102")
	dir := filepath.Join(c.mediaDir, day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if ext == "" {
		ext = ".bin"
	}
	// idx disambiguates multiple media items within one message.
	name := fmt.Sprintf("%d_%d_%s%s", wire.MessageID, idx, kind, ext)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ── Reply ──

// buildReply returns a channel.ReplyFunc sending a message back to the
// same user. Media markers in Text ([file: path] / [image: path] /
// [video: path]) are uploaded and sent as media items; the rest of the
// text rides along as a caption. Text over 4000 bytes is chunked.
func (c *Channel) buildReply(wire *protocol.WireMessage) channel.ReplyFunc {
	return func(replyCtx context.Context, msg channel.ReplyMessage) (string, error) {
		userID := wire.FromUserID
		c.contextTokens.Store(userID, wire.ContextToken)
		contextToken := wire.ContextToken

		// No card API in WeChat — render card content as plain text.
		text := msg.Text
		if msg.Card != nil {
			if text != "" {
				text += "\n"
			}
			text += cardToText(msg.Card)
		}
		if strings.TrimSpace(text) == "" {
			return "", nil
		}

		items, plainText := c.splitMediaMarkers(text)
		if plainText != "" {
			for _, chunk := range protocol.ChunkText(plainText, 4000) {
				if err := c.client.SendMessage(replyCtx, c.creds.BaseURL, c.creds.Token, protocol.BuildTextMessage(userID, contextToken, chunk)); err != nil {
					return "", err
				}
			}
		}
		if len(items) > 0 {
			if err := c.client.SendMessage(replyCtx, c.creds.BaseURL, c.creds.Token, protocol.BuildMediaMessage(userID, contextToken, items)); err != nil {
				return "", err
			}
		}
		return "", nil // ilinkai returns no message id
	}
}

// splitMediaMarkers splits reply text into CDN uploads and the remaining
// plain text (caption). Returns item list + caption; caption is empty
// when every line was a marker.
func (c *Channel) splitMediaMarkers(text string) (items []map[string]any, caption string) {
	var plain []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		kind, path, ok := parseMediaMarker(line)
		if !ok {
			if line != "" {
				plain = append(plain, line)
			}
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			plain = append(plain, fmt.Sprintf("[%s: unreadable %s]", kind, path))
			continue
		}
		item, err := c.buildMediaItem(context.Background(), kind, path, data)
		if err != nil {
			plain = append(plain, fmt.Sprintf("[%s: upload failed]", kind))
			continue
		}
		items = append(items, item)
	}
	return items, strings.Join(plain, "\n")
}

// buildMediaItem uploads data to the CDN and builds the wire item.
func (c *Channel) buildMediaItem(ctx context.Context, kind, path string, data []byte) (map[string]any, error) {
	creds := c.creds
	switch kind {
	case "image":
		media, err := cdnUpload(ctx, c.client, creds, data, creds.UserID, protocol.MediaImage)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": protocol.ItemImage, "image_item": map[string]any{
			"media": mediaToMap(media),
		}}, nil
	case "video":
		media, err := cdnUpload(ctx, c.client, creds, data, creds.UserID, protocol.MediaVideo)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": protocol.ItemVideo, "video_item": map[string]any{
			"media":       mediaToMap(media),
			"video_size":  len(data),
		}}, nil
	case "file":
		media, err := cdnUpload(ctx, c.client, creds, data, creds.UserID, protocol.MediaFile)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": protocol.ItemFile, "file_item": map[string]any{
			"media":     mediaToMap(media),
			"file_name": filepath.Base(path),
			"len":       strconv.Itoa(len(data)),
		}}, nil
	}
	return nil, fmt.Errorf("unknown media kind %q", kind)
}

// parseMediaMarker extracts [kind: path] — [image: /x/a.jpg], [file: /x/a.pdf],
// [video: /x/a.mp4] — from a line. The path must be absolute (the agent
// writes file locations it knows).
func parseMediaMarker(line string) (kind, path string, ok bool) {
	if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
		return "", "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
	kind, path, found := strings.Cut(inner, ": ")
	if !found {
		return "", "", false
	}
	switch kind {
	case "image", "file", "video":
	default:
		return "", "", false
	}
	if !filepath.IsAbs(path) {
		return "", "", false
	}
	return kind, path, true
}

func mediaToMap(m *protocol.CDNMedia) map[string]any {
	return map[string]any{
		"encrypt_query_param": m.EncryptQueryParam,
		"aes_key":             m.AESKey,
		"encrypt_type":        m.EncryptType,
	}
}

// cardToText renders a channel.Card as plain text (WeChat has no card
// API): title header, content, panels flattened with separators.
func cardToText(card *channel.Card) string {
	var b strings.Builder
	if card.Header.Title != "" {
		b.WriteString("【" + card.Header.Title + "】\n")
	}
	if card.Content != "" {
		b.WriteString(card.Content)
	}
	for _, p := range card.Panels {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(cardToText(&p))
	}
	return b.String()
}

// rememberContext stores the per-user context token (required for all
// subsequent replies to that user).
func (c *Channel) rememberContext(wire *protocol.WireMessage) {
	userID := wire.FromUserID
	if wire.MessageType == protocol.MessageTypeBot {
		userID = wire.ToUserID
	}
	if userID != "" && wire.ContextToken != "" {
		c.contextTokens.Store(userID, wire.ContextToken)
	}
}

func (c *Channel) reportError(err error) {
	if c.onError != nil {
		c.onError(err)
	}
}
