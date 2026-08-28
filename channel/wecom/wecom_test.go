package wecom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/channel"
)

// The QR flow is exercised against a fake server: generate returns
// scode+auth_url, query_result reports waiting then success with the
// created bot's credentials.
func TestQRFlow(t *testing.T) {
	oldGen, oldQuery := qrGenerateURL, qrQueryURL
	defer func() { qrGenerateURL, qrQueryURL = oldGen, oldQuery }()

	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "generate"):
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"scode": "sc-1", "auth_url": "https://auth.example/x"}})
		case strings.Contains(r.URL.Path, "query_result"):
			polls++
			if polls < 2 {
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"status": "waiting"}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"status": "success",
				"bot_info": map[string]any{"botid": "bot-1", "secret": "sec-1"},
			}})
		}
	}))
	defer srv.Close()
	qrGenerateURL, qrQueryURL = srv.URL+"/generate", srv.URL+"/query_result"

	oldInterval := pollInterval
	pollInterval = 10 * time.Millisecond
	defer func() { pollInterval = oldInterval }()

	qr, err := GenerateQR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if qr.SCode != "sc-1" || qr.AuthURL != "https://auth.example/x" {
		t.Fatalf("qr = %+v", qr)
	}
	if !strings.Contains(qr.PageURL, "scode=sc-1") {
		t.Fatalf("page url = %q", qr.PageURL)
	}

	var statuses []string
	creds, err := PollQRResult(context.Background(), qr.SCode, func(s string) { statuses = append(statuses, s) })
	if err != nil {
		t.Fatal(err)
	}
	if creds.BotID != "bot-1" || creds.Secret != "sec-1" {
		t.Fatalf("creds = %+v", creds)
	}
	if len(statuses) != 1 || statuses[0] != "waiting" {
		t.Fatalf("statuses = %v", statuses)
	}
}

// PollQRResult must abort on context cancel, not wait out the timeout.
func TestPollQRResultCancels(t *testing.T) {
	oldQuery := qrQueryURL
	defer func() { qrQueryURL = oldQuery }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"status": "waiting"}})
	}))
	defer srv.Close()
	qrQueryURL = srv.URL + "/query_result"
	oldInterval := pollInterval
	pollInterval = time.Hour // never poll again — cancel must win
	defer func() { pollInterval = oldInterval }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := PollQRResult(ctx, "sc-1", nil)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error on cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("PollQRResult did not abort on cancel")
	}
}

func TestToIncoming(t *testing.T) {
	body := &MsgCallbackBody{
		MsgID:    "m-1",
		ChatID:   "chat-1",
		ChatType: "single",
		From:     MsgFrom{UserID: "user-1"},
		MsgType:  "text",
		Text:     &TextBody{Content: " 你好 "},
	}
	msg := toIncoming(body)
	if msg == nil {
		t.Fatal("nil message")
	}
	if msg.Text != " 你好 " || msg.UserID != "user-1" || msg.ChatType != "private" || msg.ID != "m-1" {
		t.Fatalf("msg = %+v", msg)
	}

	// Non-text messages are ignored for now (media is a later iteration).
	if toIncoming(&MsgCallbackBody{MsgType: "image"}) != nil {
		t.Fatal("image message not ignored")
	}
	// Group chat type maps to "group".
	body.ChatType = "group"
	msg = toIncoming(body)
	if msg.ChatType != "group" {
		t.Fatal("group type not mapped")
	}
	if msg.ChatID != "chat-1" {
		t.Fatalf("group chatid = %q", msg.ChatID)
	}
}

// Single chats carry NO chatid — the conversation must key on the
// sender, or every single-chat user would share one session and their
// histories would bleed into each other.
func TestToIncomingSingleChatKeysOnSender(t *testing.T) {
	body := &MsgCallbackBody{
		MsgID:    "m-1",
		ChatType: "single", // chatid omitted — as the protocol documents
		From:     MsgFrom{UserID: "user-1"},
		MsgType:  "text",
		Text:     &TextBody{Content: "hi"},
	}
	msg := toIncoming(body)
	if msg == nil {
		t.Fatal("nil message")
	}
	if msg.ChatID != "user-1" {
		t.Fatalf("single chat chatid = %q, want sender", msg.ChatID)
	}
}

// Group mentions are stripped so the agent sees the question, not
// "@RobotA hello".
func TestToIncomingStripsGroupMention(t *testing.T) {
	body := &MsgCallbackBody{
		ChatType: "group",
		From:     MsgFrom{UserID: "user-1"},
		MsgType:  "text",
		Text:     &TextBody{Content: "@RobotA 你好世界"},
	}
	msg := toIncoming(body)
	if msg == nil || msg.Text != "你好世界" {
		t.Fatalf("text = %q, want 你好世界", msg.Text)
	}
	// Plain text without a mention passes through untouched.
	body.Text.Content = "没有艾特"
	if msg := toIncoming(body); msg == nil || msg.Text != "没有艾特" {
		t.Fatalf("plain text mangled: %+v", msg)
	}
}

func TestFrameEncoding(t *testing.T) {
	// Subscribe frame shape.
	sub := Frame{Cmd: cmdSubscribe, Headers: FrameHeaders{ReqID: "r1"},
		Body: mustJSON(SubscribeBody{BotID: "b", Secret: "s"})}
	raw, _ := json.Marshal(sub)
	var parsed struct {
		Cmd     string `json:"cmd"`
		Headers struct {
			ReqID string `json:"req_id"`
		} `json:"headers"`
		Body struct {
			BotID  string `json:"bot_id"`
			Secret string `json:"secret"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Cmd != "aibot_subscribe" || parsed.Headers.ReqID != "r1" || parsed.Body.BotID != "b" || parsed.Body.Secret != "s" {
		t.Fatalf("subscribe frame = %+v", parsed)
	}

	// Stream reply frame shape (the streaming contract: stream.id + finish).
	rep := Frame{Cmd: cmdRespondMsg, Headers: FrameHeaders{ReqID: "r2"},
		Body: mustJSON(StreamReplyBody{MsgType: "stream", Stream: StreamItem{ID: "s1", Finish: false, Content: "hi"}})}
	raw, _ = json.Marshal(rep)
	var repParsed struct {
		Cmd     string `json:"cmd"`
		Headers struct {
			ReqID string `json:"req_id"`
		} `json:"headers"`
		Body struct {
			MsgType string `json:"msgtype"`
			Stream  struct {
				ID      string `json:"id"`
				Finish  bool   `json:"finish"`
				Content string `json:"content"`
			} `json:"stream"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &repParsed); err != nil {
		t.Fatal(err)
	}
	if repParsed.Cmd != "aibot_respond_msg" || repParsed.Headers.ReqID != "r2" ||
		repParsed.Body.MsgType != "stream" || repParsed.Body.Stream.ID != "s1" ||
		repParsed.Body.Stream.Finish || repParsed.Body.Stream.Content != "hi" {
		t.Fatalf("stream reply frame = %+v", repParsed)
	}
}

var _ channel.Channel = (*Channel)(nil)
