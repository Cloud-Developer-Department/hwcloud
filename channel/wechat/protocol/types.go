// Package protocol implements the raw iLink Bot API HTTP calls for the
// Tencent official WeChat AI channel (ilinkai.weixin.qq.com). The wire
// shapes mirror the reference implementations (openclaw-wechat plugin,
// and the community Go SDK that this project verified end-to-end).
package protocol

import "encoding/json"

// MessageType indicates who sent the message.
type MessageType int

const (
	// MessageTypeUser is an incoming user message.
	MessageTypeUser MessageType = 1
	// MessageTypeBot is a message sent by the bot (echo of our own sends).
	MessageTypeBot MessageType = 2
)

// MessageItemType indicates the content type of a message item.
type MessageItemType int

const (
	ItemText  MessageItemType = 1
	ItemImage MessageItemType = 2
	ItemVoice MessageItemType = 3
	ItemFile  MessageItemType = 4
	ItemVideo MessageItemType = 5
)

// MediaType is used in getuploadurl requests.
type MediaType int

const (
	MediaImage MediaType = 1
	MediaVideo MediaType = 2
	MediaFile  MediaType = 3
	MediaVoice MediaType = 4
)

// CDNMedia references an encrypted file on the WeChat CDN.
type CDNMedia struct {
	EncryptQueryParam string `json:"encrypt_query_param"`
	AESKey            string `json:"aes_key"`
	EncryptType       int    `json:"encrypt_type,omitempty"`
	FullURL           string `json:"full_url,omitempty"` // server-provided download URL; use directly when set
}

// TextItem holds text content.
type TextItem struct {
	Text string `json:"text"`
}

// ImageItem holds image content and CDN references.
type ImageItem struct {
	Media      *CDNMedia `json:"media,omitempty"`
	ThumbMedia *CDNMedia `json:"thumb_media,omitempty"`
	AESKey     string    `json:"aeskey,omitempty"`
	URL        string    `json:"url,omitempty"`
	ThumbWidth int       `json:"thumb_width,omitempty"`
}

// VoiceItem holds voice content. v1 does not transcode silk — the item
// is surfaced as a "[voice]" marker only.
type VoiceItem struct {
	Media      *CDNMedia `json:"media,omitempty"`
	EncodeType int       `json:"encode_type,omitempty"`
	Text       string    `json:"text,omitempty"`
	Playtime   int       `json:"playtime,omitempty"`
}

// FileItem holds file content.
type FileItem struct {
	Media    *CDNMedia `json:"media,omitempty"`
	FileName string    `json:"file_name,omitempty"`
	MD5      string    `json:"md5,omitempty"`
	Len      string    `json:"len,omitempty"`
}

// VideoItem holds video content.
type VideoItem struct {
	Media      *CDNMedia `json:"media,omitempty"`
	VideoSize  int64     `json:"video_size,omitempty"`
	PlayLength int       `json:"play_length,omitempty"`
	ThumbMedia *CDNMedia `json:"thumb_media,omitempty"`
}

// RefMessage represents a quoted/referenced message.
type RefMessage struct {
	Title       string       `json:"title,omitempty"`
	MessageItem *MessageItem `json:"message_item,omitempty"`
}

// MessageItem is a single content item within a message.
type MessageItem struct {
	Type      MessageItemType `json:"type"`
	TextItem  *TextItem       `json:"text_item,omitempty"`
	ImageItem *ImageItem      `json:"image_item,omitempty"`
	VoiceItem *VoiceItem      `json:"voice_item,omitempty"`
	FileItem  *FileItem       `json:"file_item,omitempty"`
	VideoItem *VideoItem      `json:"video_item,omitempty"`
	RefMsg    *RefMessage     `json:"ref_msg,omitempty"`
}

// WireMessage is the raw message from the iLink API.
type WireMessage struct {
	Seq          int64         `json:"seq,omitempty"`
	MessageID    int64         `json:"message_id,omitempty"`
	FromUserID   string        `json:"from_user_id"`
	ToUserID     string        `json:"to_user_id"`
	ClientID     string        `json:"client_id"`
	CreateTimeMs int64         `json:"create_time_ms"`
	MessageType  MessageType   `json:"message_type"`
	MessageState int           `json:"message_state"`
	ContextToken string        `json:"context_token"`
	ItemList     []MessageItem `json:"item_list"`
}

// Credentials holds bot authentication data (persisted in settings as
// channels.wechat).
type Credentials struct {
	Token     string `json:"token"`
	BaseURL   string `json:"base_url"`
	AccountID string `json:"account_id"`
	UserID    string `json:"user_id"`
}

// QRCodeResponse from get_bot_qrcode.
type QRCodeResponse struct {
	QRCode       string `json:"qrcode"`
	QRCodeImgURL string `json:"qrcode_img_content"`
}

// QRStatusResponse from get_qrcode_status.
//
// Status is one of: wait, scaned, confirmed, expired, scaned_but_redirect,
// binded_redirect, need_verifycode, verify_code_blocked.
type QRStatusResponse struct {
	Status       string `json:"status"`
	BotToken     string `json:"bot_token,omitempty"`
	BotID        string `json:"ilink_bot_id,omitempty"`
	UserID       string `json:"ilink_user_id,omitempty"`
	BaseURL      string `json:"baseurl,omitempty"`
	RedirectHost string `json:"redirect_host,omitempty"` // set when status is scaned_but_redirect
}

// GetUpdatesResponse from getupdates.
type GetUpdatesResponse struct {
	Ret           int               `json:"ret"`
	Msgs          []json.RawMessage `json:"msgs"`
	GetUpdatesBuf string            `json:"get_updates_buf"`
	ErrCode       int               `json:"errcode,omitempty"`
	ErrMsg        string            `json:"errmsg,omitempty"`
}

// GetUploadURLRequest holds parameters for getuploadurl.
type GetUploadURLRequest struct {
	FileKey     string `json:"filekey"`
	MediaType   int    `json:"media_type"`
	ToUserID    string `json:"to_user_id"`
	RawSize     int    `json:"rawsize"`
	RawFileMD5  string `json:"rawfilemd5"`
	FileSize    int    `json:"filesize"`
	NoNeedThumb bool   `json:"no_need_thumb,omitempty"`
	AESKey      string `json:"aeskey,omitempty"`
}

// GetUploadURLResponse from getuploadurl.
type GetUploadURLResponse struct {
	UploadParam   string `json:"upload_param"`
	UploadFullURL string `json:"upload_full_url,omitempty"` // use directly when set
}

// GetConfigResponse from getconfig (typing ticket for a user).
type GetConfigResponse struct {
	TypingTicket string `json:"typing_ticket,omitempty"`
	Ret          int    `json:"ret,omitempty"`
}
