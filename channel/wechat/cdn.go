package wechat

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/channel/wechat/crypto"
	"github.com/Cloud-Developer-Department/hwcloud/channel/wechat/protocol"
)

// cdnClient is a dedicated client for CDN transfers: larger files need
// more time than the API client's 45s cap.
var cdnClient = &http.Client{Timeout: 120 * time.Second}

// cdnDownload fetches and decrypts a media file from the WeChat CDN.
// keyOverride wins when non-empty (some items carry the AES key outside
// the media reference).
func cdnDownload(ctx context.Context, media *protocol.CDNMedia, keyOverride string) ([]byte, error) {
	if media == nil {
		return nil, fmt.Errorf("cdn download: no media reference")
	}
	downloadURL := media.FullURL
	if downloadURL == "" {
		downloadURL = protocol.BuildCDNDownloadURL(media.EncryptQueryParam)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdn download request: %w", err)
	}
	resp, err := cdnClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdn download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cdn download failed: HTTP %d", resp.StatusCode)
	}

	ciphertext, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cdn download read: %w", err)
	}

	keySource := keyOverride
	if keySource == "" {
		keySource = media.AESKey
	}
	if keySource == "" {
		return nil, fmt.Errorf("cdn download: no AES key available for decryption")
	}
	aesKey, err := crypto.DecodeAESKey(keySource)
	if err != nil {
		return nil, fmt.Errorf("cdn download: decode aes key: %w", err)
	}
	return crypto.DecryptAESECB(ciphertext, aesKey)
}

// cdnUpload encrypts and uploads data to the WeChat CDN, returning the
// media reference to embed in a message item.
func cdnUpload(ctx context.Context, client *protocol.Client, creds *protocol.Credentials, data []byte, userID string, mediaType protocol.MediaType) (*protocol.CDNMedia, error) {
	aesKey, err := crypto.GenerateAESKey()
	if err != nil {
		return nil, fmt.Errorf("cdn upload: generate aes key: %w", err)
	}
	ciphertext, err := crypto.EncryptAESECB(data, aesKey)
	if err != nil {
		return nil, fmt.Errorf("cdn upload: encrypt: %w", err)
	}

	fileKey, err := randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("cdn upload: generate file key: %w", err)
	}
	rawMD5 := md5.Sum(data)

	uploadResp, err := client.GetUploadURL(ctx, creds.BaseURL, creds.Token, protocol.GetUploadURLRequest{
		FileKey:     fileKey,
		MediaType:   int(mediaType),
		ToUserID:    userID,
		RawSize:     len(data),
		RawFileMD5:  hex.EncodeToString(rawMD5[:]),
		FileSize:    len(ciphertext),
		NoNeedThumb: true,
		AESKey:      crypto.EncodeAESKeyHex(aesKey),
	})
	if err != nil {
		return nil, fmt.Errorf("cdn upload: getuploadurl: %w", err)
	}
	if uploadResp.UploadParam == "" && uploadResp.UploadFullURL == "" {
		return nil, fmt.Errorf("cdn upload: getuploadurl did not return an upload target")
	}

	uploadURL := uploadResp.UploadFullURL
	if uploadURL == "" {
		uploadURL = protocol.BuildCDNUploadURL(uploadResp.UploadParam, fileKey)
	}
	downloadParam, err := client.UploadToCDN(ctx, uploadURL, ciphertext)
	if err != nil {
		return nil, err
	}

	return &protocol.CDNMedia{
		EncryptQueryParam: downloadParam,
		AESKey:            crypto.EncodeAESKeyBase64(aesKey),
		EncryptType:       1,
	}, nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
