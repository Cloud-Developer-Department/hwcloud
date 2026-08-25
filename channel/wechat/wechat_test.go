package wechat

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cloud-Developer-Department/hwcloud/channel"
	"github.com/Cloud-Developer-Department/hwcloud/channel/wechat/crypto"
	"github.com/Cloud-Developer-Department/hwcloud/channel/wechat/protocol"
)

func TestToIncomingText(t *testing.T) {
	ch := New(&protocol.Credentials{}, "")
	wire := &protocol.WireMessage{
		MessageID:    42,
		FromUserID:   "user-1",
		CreateTimeMs: 1700000000000,
		MessageType:  protocol.MessageTypeUser,
		ContextToken: "ctx-1",
		ItemList: []protocol.MessageItem{
			{Type: protocol.ItemText, TextItem: &protocol.TextItem{Text: " 你好 "}},
		},
	}
	msg := ch.toIncoming(context.Background(), wire)
	if msg == nil {
		t.Fatal("nil message")
	}
	if msg.Text != " 你好 " {
		t.Fatalf("text = %q", msg.Text)
	}
	if msg.ChatID != "user-1" || msg.UserID != "user-1" || msg.ChatType != "private" {
		t.Fatalf("identity wrong: %+v", msg)
	}
	if msg.ID != "42" {
		t.Fatalf("id = %q", msg.ID)
	}
	if msg.Timestamp.UnixMilli() != 1700000000000 {
		t.Fatalf("timestamp wrong: %v", msg.Timestamp)
	}
}

func TestToIncomingEmptyIgnored(t *testing.T) {
	ch := New(&protocol.Credentials{}, "")
	wire := &protocol.WireMessage{FromUserID: "u", MessageType: protocol.MessageTypeUser}
	if msg := ch.toIncoming(context.Background(), wire); msg != nil {
		t.Fatalf("empty message not ignored: %+v", msg)
	}
	// Bot echoes are filtered at the Start loop level; toIncoming itself
	// does not filter.
	wire.ItemList = []protocol.MessageItem{{Type: protocol.ItemText, TextItem: &protocol.TextItem{Text: "x"}}}
	if msg := ch.toIncoming(context.Background(), wire); msg == nil {
		t.Fatal("text message dropped")
	}
}

// Videos are NOT downloaded (they would stall the long-poll loop) —
// marker only, like voice.
func TestToIncomingVideoMarkerOnly(t *testing.T) {
	ch := New(&protocol.Credentials{}, t.TempDir())
	wire := &protocol.WireMessage{
		MessageID:   9,
		FromUserID:  "u",
		CreateTimeMs: 1,
		MessageType: protocol.MessageTypeUser,
		ContextToken: "c",
		ItemList: []protocol.MessageItem{
			{Type: protocol.ItemVideo, VideoItem: &protocol.VideoItem{Media: &protocol.CDNMedia{EncryptQueryParam: "x", AESKey: "k"}}},
		},
	}
	msg := ch.toIncoming(context.Background(), wire)
	if msg == nil {
		t.Fatal("nil message")
	}
	if msg.Text != "[video]" {
		t.Fatalf("text = %q, want [video]", msg.Text)
	}
}

// Media download is exercised end-to-end with a fake CDN: the media
// round-trip (encrypt → CDN serve → decrypt → file on disk).
func TestToIncomingMediaDownload(t *testing.T) {
	key, err := crypto.GenerateAESKey()
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("fake image bytes")
	ciphertext, err := crypto.EncryptAESECB(plain, key)
	if err != nil {
		t.Fatal(err)
	}

	// Fake CDN: /download serves the ciphertext for any query param.
	cdnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "download") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(ciphertext)
	}))
	defer cdnSrv.Close()
	// The channel builds download URLs from protocol.CDNBaseURL — reroute
	// it through the fake for this test.
	oldCDNBase := protocol.CDNBaseURL
	protocol.CDNBaseURL = cdnSrv.URL
	defer func() { protocol.CDNBaseURL = oldCDNBase }()

	mediaDir := t.TempDir()
	ch := New(&protocol.Credentials{}, mediaDir)

	wire := &protocol.WireMessage{
		MessageID:   7,
		FromUserID:  "user-1",
		CreateTimeMs: 1,
		MessageType: protocol.MessageTypeUser,
		ContextToken: "c",
		ItemList: []protocol.MessageItem{
			{Type: protocol.ItemImage, ImageItem: &protocol.ImageItem{
				Media: &protocol.CDNMedia{
					EncryptQueryParam: "param-x",
					AESKey:            crypto.EncodeAESKeyBase64(key),
					EncryptType:       1,
				},
			}},
		},
	}

	msg := ch.toIncoming(context.Background(), wire)
	if msg == nil {
		t.Fatal("nil message")
	}
	if !strings.HasPrefix(msg.Text, "[image: ") {
		t.Fatalf("text = %q", msg.Text)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(msg.Text, "[image: "), "]")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, plain) {
		t.Fatal("decrypted media mismatch")
	}
	// File lives under mediaDir with a dated subdirectory.
	rel, err := filepath.Rel(mediaDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("media escaped mediaDir: %q", path)
	}
}

func TestToIncomingMediaDownloadFailureDegrades(t *testing.T) {
	// No CDN server — the download fails and the marker has no path.
	ch := New(&protocol.Credentials{}, t.TempDir())
	wire := &protocol.WireMessage{
		MessageID:   1,
		FromUserID:  "u",
		CreateTimeMs: 1,
		MessageType: protocol.MessageTypeUser,
		ContextToken: "c",
		ItemList: []protocol.MessageItem{
			{Type: protocol.ItemImage, ImageItem: &protocol.ImageItem{
				Media: &protocol.CDNMedia{EncryptQueryParam: "dead", AESKey: crypto.EncodeAESKeyBase64(make([]byte, 16))},
			}},
			{Type: protocol.ItemVoice, VoiceItem: &protocol.VoiceItem{}},
		},
	}
	msg := ch.toIncoming(context.Background(), wire)
	if msg == nil {
		t.Fatal("nil message")
	}
	if msg.Text != "[image]\n[voice]" {
		t.Fatalf("text = %q", msg.Text)
	}
}

func TestParseMediaMarker(t *testing.T) {
	cases := []struct {
		line       string
		kind, path string
		ok         bool
	}{
		{"[image: /tmp/a.jpg]", "image", "/tmp/a.jpg", true},
		{"[file: /tmp/report.pdf]", "file", "/tmp/report.pdf", true},
		{"[video: /tmp/v.mp4]", "video", "/tmp/v.mp4", true},
		{"plain text", "", "", false},
		{"[voice]", "", "", false},
		{"[image: relative.jpg]", "", "", false},
		{"[weird: /tmp/x]", "", "", false},
		{"[image: /tmp/a.jpg] trailing", "", "", false},
	}
	for _, c := range cases {
		kind, path, ok := parseMediaMarker(c.line)
		if ok != c.ok || kind != c.kind || path != c.path {
			t.Errorf("parseMediaMarker(%q) = (%q, %q, %v), want (%q, %q, %v)", c.line, kind, path, ok, c.kind, c.path, c.ok)
		}
	}
}

func TestCardToText(t *testing.T) {
	card := &channel.Card{
		Header:  channel.CardHeader{Title: "📋 计划"},
		Content: "step 1",
		Panels:  []channel.Card{{Content: "detail"}},
	}
	text := cardToText(card)
	if !strings.Contains(text, "📋 计划") || !strings.Contains(text, "step 1") || !strings.Contains(text, "detail") {
		t.Fatalf("card text = %q", text)
	}
}

func TestRememberContext(t *testing.T) {
	ch := New(&protocol.Credentials{}, "")
	ch.rememberContext(&protocol.WireMessage{FromUserID: "u1", ContextToken: "t1", MessageType: protocol.MessageTypeUser})
	ch.rememberContext(&protocol.WireMessage{ToUserID: "u1", ContextToken: "t2", MessageType: protocol.MessageTypeBot})
	if v, ok := ch.contextTokens.Load("u1"); !ok || v != "t2" {
		t.Fatalf("context token = %v", v)
	}
}
