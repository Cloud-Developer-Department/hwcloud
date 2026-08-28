package wecom

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Cloud-Developer-Department/hwcloud/channel"
)

func cryptoRandRead(b []byte) (int, error) { return rand.Read(b) }

// Channel implements channel.Channel for WeCom smart robots over the
// official WebSocket long connection.
//
// Connection: dial wss://openws.work.weixin.qq.com, send aibot_subscribe
// (bot_id + secret) — the connection then stays open; the server pushes
// aibot_msg_callback / aibot_event_callback frames and answers ping.
// The client must ping every ~30s to keep the connection alive.
//
// Replies use the streaming mechanism: one stream.id = one message that
// can be refreshed until finish=true (the message the user sees grows in
// place — WeCom supports true streaming, unlike personal WeChat).
type Channel struct {
	botID  string
	secret string

	conn   *websocket.Conn
	mu     sync.Mutex // guards conn writes
	closed bool

	onReady        func()
	onReconnecting func()
	onError        func(err error)

	pending   map[string]chan Frame // req_id → response (upload acks)
	pendingMu sync.Mutex

	approver *wecomApprover
}

// New returns a WeCom Channel bound to the robot credentials. Must be
// started via Start().
func New(botID, secret string) *Channel {
	ch := &Channel{botID: botID, secret: secret, pending: make(map[string]chan Frame)}
	ch.approver = newWecomApprover(ch)
	return ch
}

// SetOnReady registers the ready callback (nil clears it). Fired once
// the subscribe handshake succeeds.
func (c *Channel) SetOnReady(f func()) { c.onReady = f }

// SetOnReconnecting registers the reconnecting callback (nil clears it).
// Fired when the connection drops and the loop reconnects.
func (c *Channel) SetOnReconnecting(f func()) { c.onReconnecting = f }

// SetOnError registers the error callback (nil clears it). Fired on
// connection/subscribe failures; used by the manager to surface
// first-connect failures (bad credentials).
func (c *Channel) SetOnError(f func(err error)) { c.onError = f }

// Name implements channel.Channel.
func (c *Channel) Name() string { return "wecom" }

// Stop implements channel.Channel. Closes the underlying WebSocket —
// this wakes a ReadMessage blocked in Start (gorilla's ReadMessage does
// NOT respond to context cancellation, so cancelling the Start context
// alone would leave the connection goroutine stuck forever, holding the
// machine lock).
func (c *Channel) Stop() error {
	c.closeConn()
	return nil
}

// Start implements channel.Channel. Connects, subscribes, and runs the
// read loop (plus heartbeats) until ctx is cancelled or the connection
// is permanently lost. Reconnects automatically with backoff on drops.
func (c *Channel) Start(ctx context.Context, handler channel.MessageHandler) error {
	retryDelay := time.Second
	everReady := false
	for {
		err := c.runOnce(ctx, handler, &everReady)
		if err == nil || ctx.Err() != nil {
			return nil // clean shutdown
		}
		if !everReady && c.onError != nil {
			c.onError(err)
		}
		if everReady && c.onReconnecting != nil {
			c.onReconnecting()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retryDelay):
		}
		retryDelay = min(retryDelay*2, 10*time.Second)
	}
}

// runOnce is one connection lifetime: dial → subscribe → read loop →
// heartbeat until the connection drops or ctx is cancelled.
func (c *Channel) runOnce(ctx context.Context, handler channel.MessageHandler, everReady *bool) error {
	c.mu.Lock()
	c.closed = false
	c.mu.Unlock()
	defer c.closeConn()

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsEndpoint, nil)
	if err != nil {
		return fmt.Errorf("wecom dial: %w", err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	// Subscribe with a fresh req_id.
	subID := newReqID()
	sub := Frame{Cmd: cmdSubscribe, Headers: FrameHeaders{ReqID: subID},
		Body: mustJSON(SubscribeBody{BotID: c.botID, Secret: c.secret})}
	if err := c.writeJSON(sub); err != nil {
		return fmt.Errorf("wecom subscribe: %w", err)
	}

	// The first frame must be the subscribe ack (errcode 0). The ack
	// frame has NO "cmd" field (only headers/errcode/errmsg) — it is
	// simply the first frame after subscribe, so we parse it directly
	// instead of matching on cmd.
	if err := c.expectSubscribeAck(); err != nil {
		return err
	}
	if !*everReady {
		*everReady = true
		if c.onReady != nil {
			c.onReady()
		}
	}

	// Heartbeat every 30s (server drops silent connections).
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go c.heartbeat(hbCtx)

	// Context watcher: cancelling the Start context must terminate the
	// read loop — gorilla's ReadMessage blocks forever otherwise (a
	// disconnected manager would leave the connection goroutine stuck,
	// holding the machine lock). Closing the connection wakes it.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			c.closeConn()
		case <-watchDone:
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("wecom read: %w", err)
		}
		// Check if this is a response to a pending request (upload acks).
		// Response frames carry the same req_id as the request and may
		// have no "cmd" field — match by req_id before dispatching.
		var peek Frame
		if json.Unmarshal(raw, &peek) == nil {
			c.pendingMu.Lock()
			if ch, ok := c.pending[peek.Headers.ReqID]; ok {
				delete(c.pending, peek.Headers.ReqID)
				c.pendingMu.Unlock()
				ch <- peek
				continue
			}
			c.pendingMu.Unlock()
		}
		if err := c.handleFrame(raw, handler); err != nil {
			return err
		}
	}
}

// expectSubscribeAck waits for the subscribe ack — the first frame after
// aibot_subscribe. The ack has no "cmd" field, so the frame is parsed as
// an Ack directly (a subscribe rejection surfaces as errcode != 0).
func (c *Channel) expectSubscribeAck() error {
	_, raw, err := c.readMessage()
	if err != nil {
		return fmt.Errorf("wecom subscribe ack: %w", err)
	}
	slog.Debug("wecom: subscribe ack frame", "raw", string(raw))
	var ack Ack
	if err := json.Unmarshal(raw, &ack); err != nil {
		return fmt.Errorf("wecom subscribe ack decode: %w (raw=%s)", err, raw)
	}
	if ack.ErrCode != 0 {
		return fmt.Errorf("wecom subscribe rejected: errcode=%d errmsg=%s", ack.ErrCode, ack.ErrMsg)
	}
	return nil
}

// handleFrame dispatches one server frame.
func (c *Channel) handleFrame(raw []byte, handler channel.MessageHandler) error {
	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil {
		slog.Warn("wecom: malformed frame", "raw", string(raw))
		return nil // malformed frame — ignore
	}
	switch f.Cmd {
	case cmdMsgCallback:
		var body MsgCallbackBody
		if err := json.Unmarshal(f.Body, &body); err != nil {
			slog.Warn("wecom: msg_callback decode failed", "body", string(f.Body))
			return nil
		}
		slog.Info("wecom: message received", "msgid", body.MsgID, "msgtype", body.MsgType,
			"chattype", body.ChatType, "from", body.From.UserID, "req_id", f.Headers.ReqID)
		msg := toIncoming(&body)
		if msg == nil {
			slog.Warn("wecom: message dropped by toIncoming",
				"msgtype", body.MsgType, "has_text", body.Text != nil, "content", textPreview(&body))
			return nil
		}
		handler(context.Background(), *msg, c.buildReply(f.Headers.ReqID, &body))
		return nil

	case cmdEventCallback:
		// Always ack with pong first (keeps the server's state clean).
		_ = c.writeJSON(Frame{Cmd: cmdPong, Headers: FrameHeaders{ReqID: f.Headers.ReqID}})

		var body EventCallbackBody
		if err := json.Unmarshal(f.Body, &body); err != nil {
			slog.Warn("wecom: event_callback decode failed", "body", string(f.Body))
			return nil
		}
		if body.Event.EventType == "template_card_event" && body.Event.TemplateCardEvent != nil {
			ev := body.Event.TemplateCardEvent
			slog.Info("wecom: template card event",
				"task_id", ev.TaskID, "event_key", ev.EventKey, "card_type", ev.CardType,
				"from", body.From.UserID, "req_id", f.Headers.ReqID)
			if c.approver != nil {
				// Card update goes via aibot_respond_update_msg (WS).
				c.approver.handleCardEvent(f.Headers.ReqID, ev)
			}
		} else {
			slog.Debug("wecom: event callback (non-card)", "eventtype", body.Event.EventType, "req_id", f.Headers.ReqID)
		}
		return nil

	case cmdPing:
		// Answer pings immediately (keep-alive from the server side).
		return c.writeJSON(Frame{Cmd: cmdPong, Headers: FrameHeaders{ReqID: f.Headers.ReqID}})

	default:
		if f.Cmd == "" {
			slog.Debug("wecom: ack response", "req_id", f.Headers.ReqID, "errcode", f.ErrCode, "errmsg", f.ErrMsg)
			return nil
		}
		slog.Debug("wecom: unhandled frame", "cmd", f.Cmd)
		return nil
	}
}

// textPreview extracts a short content preview for diagnostics.
func textPreview(body *MsgCallbackBody) string {
	if body.Text != nil {
		s := body.Text.Content
		if len(s) > 40 {
			return s[:40] + "..."
		}
		return s
	}
	return ""
}

// heartbeat sends a ping every 30s until ctx is cancelled.
func (c *Channel) heartbeat(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.writeJSON(Frame{Cmd: cmdPing, Headers: FrameHeaders{ReqID: newReqID()}})
		}
	}
}

// ── Reply ──

// FinishMarker is the UpdateID sentinel that ends a streaming message:
// reply(ReplyMessage{UpdateID: FinishMarker, Text: final}) sends
// finish=true so the message can no longer be refreshed. Any other
// UpdateID refreshes the stream created by the first call.
const FinishMarker = "~finish"

// buildReply returns a channel.ReplyFunc using the WeCom streaming
// mechanism — one stream.id is one message that grows in place:
//
//   - first call (no UpdateID): creates the stream message (finish=false)
//   - calls with msg.UpdateID == the returned id: refresh that message
//   - call with msg.UpdateID == FinishMarker: ends it (finish=true)
//
// reqID is the callback's req_id — echoed verbatim in every reply.
func (c *Channel) buildReply(reqID string, cb *MsgCallbackBody) channel.ReplyFunc {
	var streamID string
	var mu sync.Mutex

	return func(ctx context.Context, msg channel.ReplyMessage) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if msg.UpdateID == FinishMarker {
			// Terminal: finish=true. A stream that never had an update is
			// created-and-finished in one shot (content is complete).
			if streamID == "" {
				streamID = newReqID()
			}
			body := StreamReplyBody{
				MsgType: "stream",
				Stream:  StreamItem{ID: streamID, Finish: true, Content: msg.Text},
			}
			if err := c.writeJSON(Frame{Cmd: cmdRespondMsg, Headers: FrameHeaders{ReqID: reqID}, Body: mustJSON(body)}); err != nil {
				return "", err
			}
			finishedID := streamID
			streamID = "" // reset: next call creates a fresh stream
			return finishedID, nil
		}
		if streamID == "" {
			streamID = newReqID()
		}
		if msg.UpdateID != "" && msg.UpdateID != streamID {
			streamID = msg.UpdateID
		}
		body := StreamReplyBody{
			MsgType: "stream",
			Stream: StreamItem{
				ID:      streamID,
				Finish:  false,
				Content: msg.Text,
			},
		}
		if err := c.writeJSON(Frame{Cmd: cmdRespondMsg, Headers: FrameHeaders{ReqID: reqID}, Body: mustJSON(body)}); err != nil {
			return "", err
		}
		return streamID, nil
	}
}

// ── Media upload + send ──

const (
	uploadChunkSize      = 512 * 1024     // 512KB before base64
	uploadMaxChunks      = 100            // ~50MB limit
	uploadRetryMax       = 2              // per-chunk retry count
	uploadRetryBaseDelay = 500 * time.Millisecond
	uploadTimeout        = 30 * time.Second
)

// sendAndWait writes a frame and blocks until the matching response
// (same req_id) arrives from the read loop. Used by the three-step
// upload protocol where each step's ack carries data (upload_id,
// media_id) that the caller needs.
func (c *Channel) sendAndWait(frame Frame) (Frame, error) {
	reqID := frame.Headers.ReqID
	ch := make(chan Frame, 1)
	c.pendingMu.Lock()
	c.pending[reqID] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, reqID)
		c.pendingMu.Unlock()
	}()
	if err := c.writeJSON(frame); err != nil {
		return Frame{}, err
	}
	select {
	case resp := <-ch:
		if resp.ErrCode != 0 {
			return resp, fmt.Errorf("wecom: errcode=%d errmsg=%s", resp.ErrCode, resp.ErrMsg)
		}
		return resp, nil
	case <-time.After(uploadTimeout):
		return Frame{}, fmt.Errorf("wecom: request timeout (req_id=%s)", reqID)
	}
}

// UploadMedia performs the three-step chunked upload (init → chunks →
// finish) over the WebSocket long connection and returns the media_id.
// The media_id is valid for 3 days.
func (c *Channel) UploadMedia(data []byte, mediaType, filename string) (string, error) {
	totalChunks := (len(data) + uploadChunkSize - 1) / uploadChunkSize
	if totalChunks == 0 {
		totalChunks = 1
	}
	if totalChunks > uploadMaxChunks {
		return "", fmt.Errorf("wecom: file too large (%d chunks, max %d)", totalChunks, uploadMaxChunks)
	}
	md5sum := md5.Sum(data)

	// Step 1: init.
	initReqID := newReqID()
	initFrame := Frame{Cmd: cmdUploadMediaInit, Headers: FrameHeaders{ReqID: initReqID},
		Body: mustJSON(UploadMediaInitBody{
			Type:        mediaType,
			Filename:    filename,
			TotalSize:   len(data),
			TotalChunks: totalChunks,
			MD5:         fmt.Sprintf("%x", md5sum),
		})}
	initResp, err := c.sendAndWait(initFrame)
	if err != nil {
		return "", fmt.Errorf("wecom upload init: %w", err)
	}
	var initResult UploadMediaInitResult
	if err := json.Unmarshal(initResp.Body, &initResult); err != nil {
		return "", fmt.Errorf("wecom upload init decode: %w", err)
	}
	if initResult.UploadID == "" {
		return "", fmt.Errorf("wecom upload init: no upload_id returned")
	}
	uploadID := initResult.UploadID

	// Step 2: chunks (serial with retry).
	for i := 0; i < totalChunks; i++ {
		start := i * uploadChunkSize
		end := start + uploadChunkSize
		if end > len(data) {
			end = len(data)
		}
		b64 := base64.StdEncoding.EncodeToString(data[start:end])
		var lastErr error
		for attempt := 0; attempt <= uploadRetryMax; attempt++ {
			chunkReqID := newReqID()
			chunkFrame := Frame{Cmd: cmdUploadMediaChunk, Headers: FrameHeaders{ReqID: chunkReqID},
				Body: mustJSON(UploadMediaChunkBody{
					UploadID:   uploadID,
					ChunkIndex: i,
					Base64Data: b64,
				})}
			if _, err := c.sendAndWait(chunkFrame); err != nil {
				lastErr = err
				if attempt < uploadRetryMax {
					time.Sleep(time.Duration(attempt+1) * uploadRetryBaseDelay)
					continue
				}
			} else {
				lastErr = nil
				break
			}
		}
		if lastErr != nil {
			return "", fmt.Errorf("wecom upload chunk %d: %w", i, lastErr)
		}
	}

	// Step 3: finish.
	finishReqID := newReqID()
	finishFrame := Frame{Cmd: cmdUploadMediaFinish, Headers: FrameHeaders{ReqID: finishReqID},
		Body: mustJSON(UploadMediaFinishBody{UploadID: uploadID})}
	finishResp, err := c.sendAndWait(finishFrame)
	if err != nil {
		return "", fmt.Errorf("wecom upload finish: %w", err)
	}
	var finishResult UploadMediaFinishResult
	if err := json.Unmarshal(finishResp.Body, &finishResult); err != nil {
		return "", fmt.Errorf("wecom upload finish decode: %w", err)
	}
	if finishResult.MediaID == "" {
		return "", fmt.Errorf("wecom upload finish: no media_id returned")
	}
	return finishResult.MediaID, nil
}

// SendMediaMessage proactively sends a media message via aibot_send_msg.
// chatID is the user ID (single chat) or chat ID (group).
func (c *Channel) SendMediaMessage(chatID, mediaType, mediaID string) error {
	body := SendMediaMsgBody{ChatID: chatID, MsgType: mediaType}
	switch mediaType {
	case "file":
		body.File = &MediaContent{MediaID: mediaID}
	case "image":
		body.Image = &MediaContent{MediaID: mediaID}
	case "voice":
		body.Voice = &MediaContent{MediaID: mediaID}
	case "video":
		body.Video = &VideoContent{MediaID: mediaID}
	default:
		return fmt.Errorf("wecom: unknown media type %q", mediaType)
	}
	return c.writeJSON(Frame{Cmd: cmdSendMsg, Headers: FrameHeaders{ReqID: newReqID()},
		Body: mustJSON(body)})
}

// SendTemplateCard proactively sends a template_card message via
// aibot_send_msg. Used by the approver to send the approval card.
// Blocks until the server acks the send — a failure (e.g. duplicate
// task_id, errcode 42014) is returned so Ask can fail fast instead of
// blocking forever.
func (c *Channel) SendTemplateCard(chatID string, cardJSON json.RawMessage) error {
	body := SendTemplateCardMsgBody{
		ChatID:       chatID,
		MsgType:      "template_card",
		TemplateCard: cardJSON,
	}
	_, err := c.sendAndWait(Frame{Cmd: cmdSendMsg, Headers: FrameHeaders{ReqID: newReqID()},
		Body: mustJSON(body)})
	return err
}

// UpdateTemplateCard updates an existing template card via
// aibot_respond_update_msg. reqID must echo the event callback's
// req_id. Used by the approver to show the resolved state after a
// button click.
func (c *Channel) UpdateTemplateCard(reqID string, cardJSON json.RawMessage) error {
	body := UpdateTemplateCardBody{
		ResponseType: "update_template_card",
		TemplateCard: cardJSON,
	}
	return c.writeJSON(Frame{Cmd: cmdRespondUpdateMsg, Headers: FrameHeaders{ReqID: reqID},
		Body: mustJSON(body)})
}

// ── Internal ──

func (c *Channel) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.closed {
		return fmt.Errorf("wecom: connection closed")
	}
	return c.conn.WriteJSON(v)
}

func (c *Channel) readMessage() (int, []byte, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return 0, nil, fmt.Errorf("wecom: no connection")
	}
	return conn.ReadMessage()
}

func (c *Channel) closeConn() {
	c.mu.Lock()
	c.closed = true
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()
}

// toIncoming converts a message callback to a channel.IncomingMessage.
// Text messages only for now (media handling is a later iteration).
func toIncoming(body *MsgCallbackBody) *channel.IncomingMessage {
	if body.MsgType != "text" || body.Text == nil {
		return nil
	}
	chatType := "private"
	if body.ChatType == "group" {
		chatType = "group"
	}
	// chatid is only present for GROUP chats — a single chat must key its
	// conversation on the sender (otherwise every single-chat user would
	// share one session and their histories would bleed into each other).
	chatID := body.ChatID
	if chatID == "" {
		chatID = body.From.UserID
	}
	text := body.Text.Content
	if chatType == "group" {
		// Group messages arrive with the @-mention prefix — strip it so
		// the agent sees the actual question.
		text = stripMention(text)
	}
	return &channel.IncomingMessage{
		ID:       body.MsgID,
		ChatID:   chatID,
		ChatType: chatType,
		UserID:   body.From.UserID,
		UserName: body.From.UserID, // wire carries no display name
		Text:     text,
		Raw:      body,
	}
}

// stripMention removes a leading "@<bot>" mention (e.g. "@RobotA hello").
func stripMention(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "@") {
		return text
	}
	rest := strings.TrimSpace(t[1:])
	if i := strings.IndexAny(rest, " \t\n"); i >= 0 {
		return strings.TrimSpace(rest[i+1:])
	}
	return "" // only a mention, no content
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // marshal of plain structs cannot fail
	}
	return b
}

func newReqID() string {
	var b [16]byte
	_, _ = cryptoRandRead(b[:])
	return fmt.Sprintf("%x", b)
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
