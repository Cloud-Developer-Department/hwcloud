package wecom

import "encoding/json"

// WS endpoint for the smart-robot long connection.
const wsEndpoint = "wss://openws.work.weixin.qq.com"

// Wire commands.
const (
	cmdSubscribe         = "aibot_subscribe"
	cmdMsgCallback       = "aibot_msg_callback"
	cmdEventCallback     = "aibot_event_callback"
	cmdRespondMsg        = "aibot_respond_msg"
	cmdRespondWelcomeMsg = "aibot_respond_welcome_msg"
	cmdRespondUpdateMsg  = "aibot_respond_update_msg"
	cmdSendMsg           = "aibot_send_msg"
	cmdUploadMediaInit   = "aibot_upload_media_init"
	cmdUploadMediaChunk  = "aibot_upload_media_chunk"
	cmdUploadMediaFinish = "aibot_upload_media_finish"
	cmdPing              = "ping"
	cmdPong              = "pong"
)

// Frame is the envelope for every WS message (requests, callbacks, and
// responses). Response frames (acks) carry ErrCode/ErrMsg instead of Cmd.
type Frame struct {
	Cmd     string          `json:"cmd,omitempty"`
	Headers FrameHeaders    `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode int             `json:"errcode,omitempty"`
	ErrMsg  string          `json:"errmsg,omitempty"`
}

// FrameHeaders carries the request id used to correlate callbacks and
// replies: EVERY reply to a callback must echo the callback's req_id.
type FrameHeaders struct {
	ReqID string `json:"req_id"`
}

// SubscribeBody is the aibot_subscribe request payload.
type SubscribeBody struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

// SubscribeResponse is the aibot_subscribe acknowledgment.
type SubscribeResponse struct {
	Headers FrameHeaders `json:"headers"`
	ErrCode int          `json:"errcode"`
	ErrMsg  string       `json:"errmsg"`
}

// MsgCallbackBody is the aibot_msg_callback payload (user message).
type MsgCallbackBody struct {
	MsgID    string    `json:"msgid"`
	BotID    string    `json:"aibotid"`
	ChatID   string    `json:"chatid"`
	ChatType string    `json:"chattype"` // "single" | "group"
	From     MsgFrom   `json:"from"`
	MsgType  string    `json:"msgtype"` // text | image | mixed | voice | file | video
	Text     *TextBody `json:"text,omitempty"`
}

// MsgFrom identifies the sender.
type MsgFrom struct {
	UserID string `json:"userid"`
}

// TextBody is the text message content.
type TextBody struct {
	Content string `json:"content"`
}

// EventCallbackBody is the aibot_event_callback payload (interaction
// events: enter_chat, template_card clicks, ...).
type EventCallbackBody struct {
	MsgID      string    `json:"msgid"`
	BotID      string    `json:"aibotid"`
	ChatID     string    `json:"chatid,omitempty"`
	ChatType   string    `json:"chattype,omitempty"`
	CreateTime int64     `json:"create_time,omitempty"`
	From       MsgFrom   `json:"from"`
	MsgType    string    `json:"msgtype"`
	Event      EventObj  `json:"event"`
}

// EventObj carries the event discriminator and optional template card
// event payload.
type EventObj struct {
	EventType         string               `json:"eventtype"`
	TemplateCardEvent *TemplateCardEvent   `json:"template_card_event,omitempty"`
}

// TemplateCardEvent is the payload carried by a template_card_event
// callback — fired when a user clicks a button on a button_interaction
// (or vote/multiple_interaction) template card.
type TemplateCardEvent struct {
	CardType string `json:"card_type"`
	EventKey string `json:"event_key"`
	TaskID   string `json:"task_id"`
}

// StreamReplyBody is the aibot_respond_msg payload for a streaming text
// reply. The same stream.id refreshes one message; finish=true ends it.
type StreamReplyBody struct {
	MsgType string     `json:"msgtype"`
	Stream  StreamItem `json:"stream"`
}

// StreamItem is the stream message content.
type StreamItem struct {
	ID      string `json:"id"`
	Finish  bool   `json:"finish"`
	Content string `json:"content"`
}

// Ack is the common response envelope for subscribe and replies.
type Ack struct {
	Headers FrameHeaders `json:"headers"`
	ErrCode int          `json:"errcode"`
	ErrMsg  string       `json:"errmsg"`
}

// ── Media upload (three-step chunked upload via WS) ──

// UploadMediaInitBody is the aibot_upload_media_init request payload.
type UploadMediaInitBody struct {
	Type        string `json:"type"`           // "file" | "image" | "voice" | "video"
	Filename    string `json:"filename"`       // display name with extension
	TotalSize   int    `json:"total_size"`     // file size in bytes
	TotalChunks int    `json:"total_chunks"`   // number of chunks
	MD5         string `json:"md5,omitempty"`  // file MD5 hex digest
}

// UploadMediaInitResult is the init response body.
type UploadMediaInitResult struct {
	UploadID string `json:"upload_id"`
}

// UploadMediaChunkBody is the aibot_upload_media_chunk request payload.
type UploadMediaChunkBody struct {
	UploadID   string `json:"upload_id"`
	ChunkIndex int    `json:"chunk_index"` // 0-based
	Base64Data string `json:"base64_data"`
}

// UploadMediaFinishBody is the aibot_upload_media_finish request payload.
type UploadMediaFinishBody struct {
	UploadID string `json:"upload_id"`
}

// UploadMediaFinishResult is the finish response body.
type UploadMediaFinishResult struct {
	Type      string `json:"type"`
	MediaID   string `json:"media_id"`
	CreatedAt int64  `json:"created_at"` // Unix timestamp
}

// ── Media message bodies ──

// MediaContent carries a media_id reference.
type MediaContent struct {
	MediaID string `json:"media_id"`
}

// VideoContent carries a media_id plus optional title/description.
type VideoContent struct {
	MediaID     string `json:"media_id"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// SendMediaMsgBody is the aibot_send_msg payload for a media message.
type SendMediaMsgBody struct {
	ChatID  string        `json:"chatid"`
	MsgType string        `json:"msgtype"`
	File    *MediaContent `json:"file,omitempty"`
	Image   *MediaContent `json:"image,omitempty"`
	Voice   *MediaContent `json:"voice,omitempty"`
	Video   *VideoContent `json:"video,omitempty"`
}

// SendTemplateCardMsgBody is the aibot_send_msg payload for a
// template_card message (e.g. button_interaction approval card).
type SendTemplateCardMsgBody struct {
	ChatID       string          `json:"chatid"`
	MsgType      string          `json:"msgtype"`
	TemplateCard json.RawMessage `json:"template_card"`
}

// UpdateTemplateCardBody is the aibot_respond_update_msg payload for
// updating a template card after a user interaction. The req_id of the
// frame must echo the event callback's req_id. Per the WeCom long-
// connection docs (path/101463), the body uses response_type (not
// msgtype) and does NOT carry userids.
type UpdateTemplateCardBody struct {
	ResponseType string          `json:"response_type"`
	TemplateCard json.RawMessage `json:"template_card"`
}
