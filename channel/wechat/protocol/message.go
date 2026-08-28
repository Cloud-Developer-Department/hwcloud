package protocol

import (
	"crypto/rand"
	"fmt"
	"net/url"
	"strings"
)

// BuildTextMessage creates a text message payload (bot → user).
func BuildTextMessage(userID, contextToken, text string) map[string]any {
	return map[string]any{
		"from_user_id":  "",
		"to_user_id":    userID,
		"client_id":     newUUID(),
		"message_type":  MessageTypeBot,
		"message_state": 2, // finished — the message is complete when sent
		"context_token": contextToken,
		"item_list": []map[string]any{
			{"type": ItemText, "text_item": map[string]string{"text": text}},
		},
	}
}

// BuildMediaMessage creates a media message payload with the given item
// list (image_item / file_item / video_item, optionally preceded by a
// text_item caption).
func BuildMediaMessage(userID, contextToken string, itemList []map[string]any) map[string]any {
	return map[string]any{
		"from_user_id":  "",
		"to_user_id":    userID,
		"client_id":     newUUID(),
		"message_type":  MessageTypeBot,
		"message_state": 2,
		"context_token": contextToken,
		"item_list":     itemList,
	}
}

// BuildCDNUploadURL constructs a CDN upload URL from getuploadurl params.
func BuildCDNUploadURL(uploadParam, filekey string) string {
	return CDNBaseURL + "/upload?encrypted_query_param=" + url.QueryEscape(uploadParam) +
		"&filekey=" + url.QueryEscape(filekey)
}

// BuildCDNDownloadURL constructs a CDN download URL from a media's
// encrypted query param.
func BuildCDNDownloadURL(encryptedQueryParam string) string {
	return CDNBaseURL + "/download?encrypted_query_param=" + url.QueryEscape(encryptedQueryParam)
}

// ChunkText splits text into ≤limit-byte chunks, breaking at paragraph
// (blank line) → newline → space boundaries near the limit so words and
// code blocks survive intact. WeChat messages are limited to ~4000 bytes.
func ChunkText(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= limit {
			chunks = append(chunks, text)
			break
		}
		cut := limit
		if idx := strings.LastIndex(text[:limit], "\n\n"); idx > limit*3/10 {
			cut = idx + 2
		} else if idx := strings.LastIndex(text[:limit], "\n"); idx > limit*3/10 {
			cut = idx + 1
		} else if idx := strings.LastIndex(text[:limit], " "); idx > limit*3/10 {
			cut = idx + 1
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if len(chunks) == 0 {
		return []string{""}
	}
	return chunks
}

func newUUID() string {
	// UUID v4.
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}
