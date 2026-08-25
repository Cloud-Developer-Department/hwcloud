package wechat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/channel/wechat/protocol"
)

// fakeILink simulates the ilinkai QR endpoints. The script is a queue of
// status responses consumed in order per poll; get_bot_qrcode returns a
// fresh qrcode each call.
type fakeILink struct {
	mu       sync.Mutex
	statuses []string           // remaining status queue ("" = error)
	statusesByVerify map[string]string // verify_code → status (verified code path)
	qrcodes  int
	baseURL  string // host to redirect to (scaned_but_redirect)
}

func newFakeILink(statuses ...string) *fakeILink {
	return &fakeILink{statuses: statuses, statusesByVerify: map[string]string{}}
}

func (f *fakeILink) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ilink/bot/get_bot_qrcode", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.qrcodes++
		qr := fmt.Sprintf("qr-%d", f.qrcodes)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"qrcode": qr, "qrcode_img_content": "https://qr/" + qr})
	})
	mux.HandleFunc("/ilink/bot/get_qrcode_status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		vc := r.URL.Query().Get("verify_code")
		if vc != "" {
			if status, ok := f.statusesByVerify[vc]; ok {
				json.NewEncoder(w).Encode(map[string]any{"status": status})
				return
			}
		}
		if len(f.statuses) == 0 {
			json.NewEncoder(w).Encode(map[string]any{"status": "wait"})
			return
		}
		status := f.statuses[0]
		f.statuses = f.statuses[1:]
		resp := map[string]any{"status": status}
		switch status {
		case "confirmed":
			resp["bot_token"] = "tok-1"
			resp["ilink_bot_id"] = "bot-1"
			resp["ilink_user_id"] = "user-1"
			resp["baseurl"] = ""
		case "scaned_but_redirect":
			resp["redirect_host"] = "redirect.example"
		}
		json.NewEncoder(w).Encode(resp)
	})
	return mux
}

func TestLoginHappyPath(t *testing.T) {
	old := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = old }()

	srv := httptest.NewServer(newFakeILink("wait", "scaned", "confirmed").handler())
	defer srv.Close()

	scanned := false
	creds, err := Login(context.Background(), protocol.NewClient(), LoginOptions{
		BaseURL:   srv.URL,
		OnQRURL:   func(string) {},
		OnScanned: func() { scanned = true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !scanned {
		t.Error("OnScanned not fired")
	}
	if creds.Token != "tok-1" || creds.AccountID != "bot-1" || creds.UserID != "user-1" {
		t.Fatalf("creds = %+v", creds)
	}
	if creds.BaseURL != srv.URL {
		t.Fatalf("baseURL = %q, want %q", creds.BaseURL, srv.URL)
	}
}

func TestLoginConfirmedBaseURL(t *testing.T) {
	old := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			json.NewEncoder(w).Encode(map[string]any{"qrcode": "qr-1", "qrcode_img_content": "u"})
		case "/ilink/bot/get_qrcode_status":
			json.NewEncoder(w).Encode(map[string]any{"status": "confirmed", "bot_token": "t", "ilink_bot_id": "b", "ilink_user_id": "u", "baseurl": "https://api.example"})
		}
	}))
	defer srv.Close()

	creds, err := Login(context.Background(), protocol.NewClient(), LoginOptions{BaseURL: srv.URL, OnQRURL: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if creds.BaseURL != "https://api.example" {
		t.Fatalf("baseURL = %q, want server-provided", creds.BaseURL)
	}
}

func TestLoginVerifyCodeFlow(t *testing.T) {
	old := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = old }()

	// Sequence: need_verifycode → (wrong code) need_verifycode again →
	// (right code) scaned → confirmed.
	f := newFakeILink("need_verifycode", "need_verifycode", "scaned", "confirmed")
	// First need_verifycode poll carries no code → queue status. The
	// re-poll carries the submitted code: wrong "111" → need_verifycode,
	// right "222" → scaned.
	f.mu.Lock()
	f.statusesByVerify["222"] = "scaned"
	f.mu.Unlock()

	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	var codes []string
	retries := 0
	_, err := Login(context.Background(), protocol.NewClient(), LoginOptions{
		BaseURL: srv.URL,
		OnQRURL: func(string) {},
		OnVerifyCode: func(_ context.Context, isRetry bool) (string, error) {
			if isRetry {
				retries++
			}
			code := "222"
			if retries == 0 {
				code = "111"
			}
			codes = append(codes, code)
			return code, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 2 || codes[0] != "111" || codes[1] != "222" {
		t.Fatalf("verify codes = %v", codes)
	}
	if retries != 1 {
		t.Fatalf("isRetry fired %d times, want 1", retries)
	}
}

func TestLoginExpiredAborts(t *testing.T) {
	old := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = old }()

	// QR expires — login aborts immediately (no refresh).
	srv := httptest.NewServer(newFakeILink("expired").handler())
	defer srv.Close()

	expired := 0
	_, err := Login(context.Background(), protocol.NewClient(), LoginOptions{
		BaseURL:   srv.URL,
		OnQRURL:   func(string) {},
		OnExpired: func() { expired++ },
	})
	if err == nil {
		t.Fatal("expected abort on expired")
	}
	if expired != 1 {
		t.Fatalf("OnExpired fired %d times, want 1", expired)
	}
}

func TestLoginVerifyCodeBlockedAborts(t *testing.T) {
	old := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = old }()

	// Too many wrong pairing codes — login aborts (no refresh).
	srv := httptest.NewServer(newFakeILink("verify_code_blocked").handler())
	defer srv.Close()

	_, err := Login(context.Background(), protocol.NewClient(), LoginOptions{
		BaseURL:      srv.URL,
		OnQRURL:      func(string) {},
		OnVerifyCode: func(context.Context, bool) (string, error) { return "999", nil },
	})
	if err == nil {
		t.Fatal("expected abort on verify_code_blocked")
	}
}

func TestLoginBindedRedirectReusesLocalCreds(t *testing.T) {
	old := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			json.NewEncoder(w).Encode(map[string]any{"qrcode": "qr-1", "qrcode_img_content": "u"})
		case "/ilink/bot/get_qrcode_status":
			json.NewEncoder(w).Encode(map[string]any{"status": "binded_redirect"})
		}
	}))
	defer srv.Close()

	local := &protocol.Credentials{Token: "old-tok", BaseURL: srv.URL, AccountID: "old-bot", UserID: "old-user"}
	creds, err := Login(context.Background(), protocol.NewClient(), LoginOptions{BaseURL: srv.URL, OnQRURL: func(string) {}, LocalCreds: local})
	if err != nil {
		t.Fatal(err)
	}
	if creds != local {
		t.Fatalf("expected local creds reused, got %+v", creds)
	}
}

func TestLoginBindedRedirectWithoutLocalCreds(t *testing.T) {
	old := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			json.NewEncoder(w).Encode(map[string]any{"qrcode": "qr-1", "qrcode_img_content": "u"})
		case "/ilink/bot/get_qrcode_status":
			json.NewEncoder(w).Encode(map[string]any{"status": "binded_redirect"})
		}
	}))
	defer srv.Close()

	_, err := Login(context.Background(), protocol.NewClient(), LoginOptions{BaseURL: srv.URL, OnQRURL: func(string) {}})
	if err == nil {
		t.Fatal("expected error without local creds")
	}
}
