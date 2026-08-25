package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChunkText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int // expected chunk count (exact for deterministic inputs)
	}{
		{"short", "hello", 1},
		{"empty", "", 1},
		{"exact limit", strings.Repeat("a", 4000), 1},
		{"over limit", strings.Repeat("a", 4001), 2},
		{"long unbroken word", strings.Repeat("x", 10000), 3},
	}
	for _, c := range cases {
		chunks := ChunkText(c.in, 4000)
		if len(chunks) != c.want {
			t.Errorf("%s: got %d chunks, want %d", c.name, len(chunks), c.want)
			continue
		}
		joined := strings.Join(chunks, "")
		if joined != c.in {
			t.Errorf("%s: chunk join mismatch (len %d vs %d)", c.name, len(joined), len(c.in))
		}
		for i, ch := range chunks {
			if len(ch) > 4000 {
				t.Errorf("%s: chunk %d exceeds limit (%d)", c.name, i, len(ch))
			}
		}
	}

	// Break points (paragraphs/newlines) shift the cut away from the hard
	// limit, so only the general properties hold here: every chunk ≤ limit,
	// join reconstructs the original.
	withBreaks := strings.Repeat("para\n\n", 2000)
	chunks := ChunkText(withBreaks, 4000)
	if len(chunks) < 2 {
		t.Errorf("breaks input produced %d chunks", len(chunks))
	}
	if strings.Join(chunks, "") != withBreaks {
		t.Error("breaks input join mismatch")
	}
	for i, ch := range chunks {
		if len(ch) > 4000 {
			t.Errorf("breaks: chunk %d exceeds limit (%d)", i, len(ch))
		}
	}
}

func TestChunkTextPrefersBreakPoints(t *testing.T) {
	// A long text with a paragraph break at 3000 bytes must cut there,
	// not at the hard 4000 limit.
	text := strings.Repeat("a", 3000) + "\n\n" + strings.Repeat("b", 3000)
	chunks := ChunkText(text, 4000)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	if !strings.HasSuffix(chunks[0], "\n\n") {
		t.Errorf("first chunk did not break at paragraph: %q...", chunks[0][len(chunks[0])-10:])
	}
}

func TestBuildTextMessageShape(t *testing.T) {
	msg := BuildTextMessage("user1", "ctx-1", "你好")
	raw, _ := json.Marshal(msg)
	var m struct {
		FromUserID   string `json:"from_user_id"`
		ToUserID     string `json:"to_user_id"`
		ClientID     string `json:"client_id"`
		MessageType  int    `json:"message_type"`
		MessageState int    `json:"message_state"`
		ContextToken string `json:"context_token"`
		ItemList     []struct {
			Type     int    `json:"type"`
			TextItem struct {
				Text string `json:"text"`
			} `json:"text_item"`
		} `json:"item_list"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ToUserID != "user1" || m.ContextToken != "ctx-1" || m.ClientID == "" {
		t.Fatalf("bad message: %+v", m)
	}
	if m.MessageType != 2 || m.MessageState != 2 {
		t.Fatalf("message_type/state wrong: %+v", m)
	}
	if len(m.ItemList) != 1 || m.ItemList[0].Type != 1 || m.ItemList[0].TextItem.Text != "你好" {
		t.Fatalf("item list wrong: %+v", m.ItemList)
	}
}

func TestBuildMediaMessageShape(t *testing.T) {
	msg := BuildMediaMessage("user1", "ctx-1", []map[string]any{
		{"type": 2, "image_item": map[string]any{"media": map[string]any{"encrypt_query_param": "p", "aes_key": "k"}}},
	})
	raw, _ := json.Marshal(msg)
	var m struct {
		ItemList []struct {
			Type      int `json:"type"`
			ImageItem struct {
				Media struct {
					EncryptQueryParam string `json:"encrypt_query_param"`
					AESKey            string `json:"aes_key"`
				} `json:"media"`
			} `json:"image_item"`
		} `json:"item_list"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.ItemList) != 1 || m.ItemList[0].Type != 2 {
		t.Fatalf("item list wrong: %+v", m.ItemList)
	}
	if m.ItemList[0].ImageItem.Media.EncryptQueryParam != "p" || m.ItemList[0].ImageItem.Media.AESKey != "k" {
		t.Fatalf("media wrong: %+v", m.ItemList[0].ImageItem.Media)
	}
}

func TestBuildCDNURLs(t *testing.T) {
	u := BuildCDNUploadURL("a b&c", "file-1")
	if !strings.Contains(u, "encrypted_query_param=a+b%26c") && !strings.Contains(u, "encrypted_query_param=a%20b%26c") {
		t.Errorf("upload URL not escaped: %s", u)
	}
	if !strings.Contains(u, "filekey=file-1") {
		t.Errorf("upload URL missing filekey: %s", u)
	}
	d := BuildCDNDownloadURL("x/y")
	if !strings.Contains(d, "/download?encrypted_query_param=") {
		t.Errorf("download URL wrong: %s", d)
	}
}

func TestSanitizeBotAgent(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", DefaultBotAgent},
		{"   ", DefaultBotAgent},
		{"MyApp/1.2 (prod)", "MyApp/1.2 (prod)"},
		{"my-app_1/0.1.2", "my-app_1/0.1.2"},
		{"A/1 B/2", "A/1 B/2"},
		{"no-version", DefaultBotAgent},
		{"Bad/1 <script>", DefaultBotAgent},
	}
	for _, c := range cases {
		if got := SanitizeBotAgent(c.in); got != c.want {
			t.Errorf("SanitizeBotAgent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAPIErrorSessionExpired(t *testing.T) {
	if !(&APIError{ErrCode: -14}).IsSessionExpired() {
		t.Error("-14 not session expired")
	}
	if (&APIError{ErrCode: 500}).IsSessionExpired() {
		t.Error("500 marked session expired")
	}
	if (&APIError{Message: "x"}).IsSessionExpired() {
		t.Error("zero errcode marked session expired")
	}
}
