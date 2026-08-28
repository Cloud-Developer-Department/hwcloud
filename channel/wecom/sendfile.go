package wecom

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/channel"
	"github.com/Cloud-Developer-Department/hwcloud/tool"
)

const (
	wecomMaxFileSize  = 20 * 1024 * 1024 // 20MB — absolute limit
	wecomMaxImageSize = 10 * 1024 * 1024 // 10MB — image limit
	wecomMaxVideoSize = 10 * 1024 * 1024 // 10MB — video limit
	wecomMaxVoiceSize = 2 * 1024 * 1024  // 2MB — voice limit (AMR only)
)

// metaWecomChatID is the Session.Metadata key carrying the WeCom chat_id
// so channel-specific tools (e.g. SendFile) can send messages back to
// the originating chat.
const metaWecomChatID = "wecom_chat_id"

var wecomImageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
	".gif": true, ".tiff": true, ".bmp": true, ".ico": true,
}

var wecomVideoExts = map[string]bool{
	".mp4": true,
}

var wecomVoiceExts = map[string]bool{
	".amr": true,
}

// SendFile is a Tool that sends a file from the workspace to the current
// WeCom chat. It uploads the file via the WS three-step chunked upload
// and sends a media message via aibot_send_msg.
type SendFile struct {
	ch      *Channel
	workDir string
}

// NewSendFile creates a SendFile tool bound to a Channel (for WS access)
// and a workspace root (for path resolution).
func NewSendFile(ch *Channel, workDir string) *SendFile {
	abs, _ := filepath.Abs(workDir)
	return &SendFile{ch: ch, workDir: abs}
}

func (t *SendFile) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "wecom_sendfile",
		Description: "Deliver a file to the user in the current WeCom chat. Only use this tool when you need to send a file to the WeCom user (e.g. delivering generated output, reports, images). This uploads the file and sends it as a WeCom media message — not for file manipulation. Supports images (jpg/png/gif/etc), video (mp4), voice (amr), and documents (pdf/doc/xls/etc). The file must already exist on disk.",
		Parameters:  hwcloud.SchemaOf[SendFileParams](),
	}
}

func (t *SendFile) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[SendFileParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("wecom_sendfile: %w", err), false, "")
	}

	abs, err := tool.ValidatePath(t.workDir, params.Path)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("wecom_sendfile: %w", err), false, "")
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return hwcloud.ErrorResult(fmt.Errorf("wecom_sendfile: file not found: %s", params.Path), false, "")
		}
		return hwcloud.ErrorResult(fmt.Errorf("wecom_sendfile: %w", err), false, "")
	}
	if info.IsDir() {
		return hwcloud.ErrorResult(fmt.Errorf("wecom_sendfile: not a file: %s", params.Path), false, "")
	}
	if info.Size() == 0 {
		return hwcloud.ErrorResult(fmt.Errorf("wecom_sendfile: empty file: %s", params.Path), false, "")
	}

	chatID, ok := wecomChatIDFromContext(ctx)
	if !ok {
		return hwcloud.ErrorResult(fmt.Errorf("wecom_sendfile: no WeCom chat context (this tool only works within a WeCom channel session)"), false, "")
	}

	fileName := params.FileName
	if fileName == "" {
		fileName = filepath.Base(abs)
	}

	ext := strings.ToLower(filepath.Ext(abs))
	mediaType := "file"
	if wecomImageExts[ext] {
		mediaType = "image"
	} else if wecomVideoExts[ext] {
		mediaType = "video"
	} else if wecomVoiceExts[ext] {
		mediaType = "voice"
	}

	// Size checks with auto-downgrade (image/video/voice → file).
	if mediaType == "image" && info.Size() > wecomMaxImageSize {
		slog.Info("wecom_sendfile: image exceeds 10MB, downgrading to file", "size", info.Size())
		mediaType = "file"
	}
	if mediaType == "video" && info.Size() > wecomMaxVideoSize {
		slog.Info("wecom_sendfile: video exceeds 10MB, downgrading to file", "size", info.Size())
		mediaType = "file"
	}
	if mediaType == "voice" && info.Size() > wecomMaxVoiceSize {
		slog.Info("wecom_sendfile: voice exceeds 2MB, downgrading to file", "size", info.Size())
		mediaType = "file"
	}
	if mediaType == "file" && info.Size() > wecomMaxFileSize {
		return hwcloud.ErrorResult(fmt.Errorf("wecom_sendfile: file too large (%d bytes, max %d)", info.Size(), wecomMaxFileSize), false, "")
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("wecom_sendfile: read: %w", err), false, "")
	}

	mediaID, err := t.ch.UploadMedia(data, mediaType, fileName)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("wecom_sendfile: upload: %w", err), false, "")
	}

	if err := t.ch.SendMediaMessage(chatID, mediaType, mediaID); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("wecom_sendfile: send: %w", err), false, "")
	}

	slog.Info("wecom_sendfile: file sent", "fileName", fileName, "mediaType", mediaType, "mediaID", mediaID)
	return &hwcloud.ToolResult{Content: fmt.Sprintf("Sent %s %s (media_id=%s)", mediaType, fileName, mediaID)}
}

// SendFileParams are the arguments to wecom_sendfile.
type SendFileParams struct {
	Path     string `json:"path" jsonschema:"description=File path in the workspace to send"`
	FileName string `json:"file_name,omitempty" jsonschema:"description=Display name for the file (default: basename of path)"`
}

// wecomChatIDFromContext extracts the WeCom chat_id from the session
// metadata injected by wecomMessageHandler.
func wecomChatIDFromContext(ctx context.Context) (string, bool) {
	s, sessionOK := hwcloud.SessionFromContext(ctx)
	if !sessionOK || s.Metadata == nil {
		return "", false
	}
	cid, _ := s.Metadata[metaWecomChatID].(string)
	if cid == "" {
		return "", false
	}
	return cid, true
}

// ReceiveMetadata returns the session metadata map that carries the
// WeCom chat_id for an incoming message, so channel-specific tools
// (e.g. SendFile) can send messages back to the originating chat.
// Exported for wecomMessageHandler to use.
func ReceiveMetadata(msg channel.IncomingMessage) map[string]any {
	return map[string]any{metaWecomChatID: msg.ChatID}
}
