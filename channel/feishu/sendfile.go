package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/channel"
	"github.com/Cloud-Developer-Department/hwcloud/tool"
)

const (
	maxFileSize = 30 * 1024 * 1024 // 30MB — Feishu file upload limit
	maxImgSize  = 10 * 1024 * 1024 // 10MB — Feishu image upload limit
)

// metaReceiveIDType / metaReceiveID are the Session.Metadata keys that
// carry the Feishu receive_id so channel-specific tools can send messages
// back to the originating chat.
const (
	metaReceiveIDType = "feishu_receive_id_type"
	metaReceiveID     = "feishu_receive_id"
)

// imageExts maps file extensions to whether the file is an image.
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
	".gif": true, ".tiff": true, ".bmp": true, ".ico": true,
}

// nativeFileTypeByExt maps extensions to their dedicated Feishu file_type.
// Extensions not listed here are NOT rejected — they use the generic
// "stream" type via fileTypeForExt. This is an optimization map, not a
// whitelist.
var nativeFileTypeByExt = map[string]string{
	".pdf":  "pdf",
	".doc":  "doc",
	".docx": "doc",
	".xls":  "xls",
	".xlsx": "xls",
	".ppt":  "ppt",
	".pptx": "ppt",
	".opus": "opus",
	".mp4":  "mp4",
}

// fileTypeForExt returns the Feishu file_type for a file extension.
// Known extensions get their dedicated type; all others get "stream".
func fileTypeForExt(ext string) string {
	if t, ok := nativeFileTypeByExt[ext]; ok {
		return t
	}
	return "stream"
}

// SendFile is a Tool that sends a file from the workspace to the current
// Feishu chat. It uploads the file via the Feishu IM API and sends a
// file/image message to the user.
type SendFile struct {
	ch      *Channel
	workDir string
}

// NewSendFile creates a SendFile tool bound to a Channel (for API access)
// and a workspace root (for path resolution).
func NewSendFile(ch *Channel, workDir string) *SendFile {
	abs, _ := filepath.Abs(workDir)
	return &SendFile{ch: ch, workDir: abs}
}

func (t *SendFile) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "feishu_sendfile",
		Description: "Deliver a file to the user in the current Feishu chat. Only use this tool when you need to send a file to the Feishu user (e.g. delivering generated output, reports, images). This uploads the file and sends it as a Feishu message — not for file manipulation. Supports images (jpg/png/gif/etc) and documents (pdf/doc/xls/ppt/mp4/opus/etc). The file must already exist on disk.",
		Parameters:  hwcloud.SchemaOf[SendFileParams](),
	}
}

func (t *SendFile) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[SendFileParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: %w", err), false, "")
	}

	abs, err := tool.ValidatePath(t.workDir, params.Path)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: %w", err), false, "")
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: file not found: %s", params.Path), false, "")
		}
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: %w", err), false, "")
	}
	if info.IsDir() {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: not a file: %s", params.Path), false, "")
	}
	if info.Size() == 0 {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: empty file: %s", params.Path), false, "")
	}

	receiveIDType, receiveID, ok := receiveFromContext(ctx)
	if !ok {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: no Feishu chat context (this tool only works within a Feishu channel session)"), false, "")
	}

	client := t.ch.Client()
	if client == nil {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: Feishu client not initialized"), false, "")
	}

	fileName := params.FileName
	if fileName == "" {
		fileName = filepath.Base(abs)
	}

	ext := strings.ToLower(filepath.Ext(abs))
	isImage := imageExts[ext]

	if isImage {
		if info.Size() > maxImgSize {
			return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: image too large (%d bytes, max %d)", info.Size(), maxImgSize), false, "")
		}
		return t.sendImage(ctx, client, abs, fileName, receiveIDType, receiveID)
	}

	if info.Size() > maxFileSize {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: file too large (%d bytes, max %d)", info.Size(), maxFileSize), false, "")
	}
	fileType := fileTypeForExt(ext)
	return t.sendFile(ctx, client, abs, fileName, fileType, receiveIDType, receiveID)
}

func (t *SendFile) sendImage(ctx context.Context, client *lark.Client, path, fileName, receiveIDType, receiveID string) *hwcloud.ToolResult {
	f, err := os.Open(path)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: open: %w", err), false, "")
	}
	defer f.Close()

	resp, err := client.Im.Image.Create(ctx,
		larkim.NewCreateImageReqBuilder().
			Body(larkim.NewCreateImageReqBodyBuilder().
				ImageType(larkim.CreateImageImageTypeMessage).
				Image(f).
				Build()).
			Build())
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: upload image: %w", err), false, "")
	}
	if !resp.Success() {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: upload image failed: code=%d msg=%s", resp.Code, resp.Msg), false, "")
	}
	if resp.Data == nil || resp.Data.ImageKey == nil {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: upload image returned no image_key"), false, "")
	}

	imageKey := *resp.Data.ImageKey
	content, _ := json.Marshal(map[string]string{"image_key": imageKey})
	msgID, err := createMessage(ctx, client, receiveIDType, receiveID, "image", string(content))
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: send image message: %w", err), false, "")
	}
	slog.Info("feishu_sendfile: image sent", "fileName", fileName, "imageKey", imageKey, "msgID", msgID)
	return &hwcloud.ToolResult{Content: fmt.Sprintf("Sent image %s (image_key=%s, message_id=%s)", fileName, imageKey, msgID)}
}

func (t *SendFile) sendFile(ctx context.Context, client *lark.Client, path, fileName, fileType, receiveIDType, receiveID string) *hwcloud.ToolResult {
	f, err := os.Open(path)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: open: %w", err), false, "")
	}
	defer f.Close()

	resp, err := client.Im.File.Create(ctx,
		larkim.NewCreateFileReqBuilder().
			Body(larkim.NewCreateFileReqBodyBuilder().
				FileType(fileType).
				FileName(fileName).
				File(f).
				Build()).
			Build())
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: upload file: %w", err), false, "")
	}
	if !resp.Success() {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: upload file failed: code=%d msg=%s", resp.Code, resp.Msg), false, "")
	}
	if resp.Data == nil || resp.Data.FileKey == nil {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: upload file returned no file_key"), false, "")
	}

	fileKey := *resp.Data.FileKey
	content, _ := json.Marshal(map[string]string{"file_key": fileKey})
	msgID, err := createMessage(ctx, client, receiveIDType, receiveID, "file", string(content))
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("feishu_sendfile: send file message: %w", err), false, "")
	}
	slog.Info("feishu_sendfile: file sent", "fileName", fileName, "fileType", fileType, "fileKey", fileKey, "msgID", msgID)
	return &hwcloud.ToolResult{Content: fmt.Sprintf("Sent file %s (file_key=%s, message_id=%s)", fileName, fileKey, msgID)}
}

// SendFileParams are the arguments to feishu_sendfile.
type SendFileParams struct {
	Path     string `json:"path" jsonschema:"description=File path in the workspace to send"`
	FileName string `json:"file_name,omitempty" jsonschema:"description=Display name for the file (default: basename of path)"`
}

// receiveFromContext extracts the Feishu receive_id_type and receive_id
// from the session metadata injected by feishuMessageHandler.
func receiveFromContext(ctx context.Context) (receiveIDType, receiveID string, ok bool) {
	s, sessionOK := hwcloud.SessionFromContext(ctx)
	if !sessionOK || s.Metadata == nil {
		return "", "", false
	}
	rit, _ := s.Metadata[metaReceiveIDType].(string)
	rid, _ := s.Metadata[metaReceiveID].(string)
	if rit == "" || rid == "" {
		return "", "", false
	}
	return rit, rid, true
}

// ReceiveMetadata returns the session metadata map that carries the
// Feishu receive_id for an incoming message, so channel-specific tools
// (e.g. SendFile) can send messages back to the originating chat.
// Exported for feishuMessageHandler to use without duplicating the
// group/private resolve logic.
func ReceiveMetadata(msg channel.IncomingMessage) map[string]any {
	idType, id := "open_id", msg.UserID
	if msg.ChatType == "group" {
		idType, id = "chat_id", msg.ChatID
	}
	return map[string]any{
		metaReceiveIDType: idType,
		metaReceiveID:     id,
	}
}
