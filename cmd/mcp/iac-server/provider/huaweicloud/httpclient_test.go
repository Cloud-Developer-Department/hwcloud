package huaweicloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// TestHTTPRequest_SSRFDefense verifies that http_request rejects URLs targeting
// localhost, private networks, and cloud metadata endpoints — the LLM's input
// is prompt-injectable and must not reach internal services. It also verifies
// the credential-exfiltration defenses: HTTPS-only and HuaweiCloud host allowlist.
func TestHTTPRequest_SSRFDefense(t *testing.T) {
	tool := NewHTTPRequest("AK", "SK", "")

	malicious := []string{
		"http://127.0.0.1/",
		"http://localhost/",
		"http://169.254.169.254/", // cloud metadata
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"file:///etc/passwd",
		"ftp://example.com/",
	}
	for _, u := range malicious {
		args, _ := json.Marshal(map[string]string{"url": u})
		result := tool.Execute(context.Background(), args)
		if result.Error == nil {
			t.Errorf("URL %q should be rejected, got content: %s", u, result.Content)
		}
		// The error should mention the rejection reason — not a network error
		// (which would mean it actually tried to connect).
		msg := ""
		if result.Error != nil {
			msg = result.Error.Message
		}
		if !strings.Contains(msg, "reject") && !strings.Contains(msg, "url") && !strings.Contains(msg, "SSRF") &&
			!strings.Contains(msg, "not allowed") && !strings.Contains(msg, "not a HuaweiCloud") {
			t.Errorf("URL %q rejected with unexpected message: %s", u, msg)
		}
	}
}

// TestHTTPRequest_HTTPSOnlyAndHostAllowlist verifies the credential-
// exfiltration defenses: the signed Authorization header (carrying the
// plaintext AK) must only be sent over HTTPS to a HuaweiCloud API domain.
func TestHTTPRequest_HTTPSOnlyAndHostAllowlist(t *testing.T) {
	tool := NewHTTPRequest("AK", "SK", "")

	// HTTP to a valid HuaweiCloud host must be rejected (cleartext leaks AK).
	args, _ := json.Marshal(map[string]string{"url": "http://bss.myhuaweicloud.com/v2/"})
	result := tool.Execute(context.Background(), args)
	if result.Error == nil {
		t.Fatal("HTTP to HuaweiCloud should be rejected — cleartext leaks the signed Authorization header")
	}
	if !strings.Contains(result.Error.Message, "HTTPS") {
		t.Fatalf("expected HTTPS-only message, got: %s", result.Error.Message)
	}

	// HTTPS to a non-HuaweiCloud host must be rejected (AK exfiltration).
	args, _ = json.Marshal(map[string]string{"url": "https://evil.example.com/v2/"})
	result = tool.Execute(context.Background(), args)
	if result.Error == nil {
		t.Fatal("HTTPS to non-HuaweiCloud host should be rejected — AK would be exfiltrated")
	}
	if !strings.Contains(result.Error.Message, "not a HuaweiCloud") {
		t.Fatalf("expected host-allowlist message, got: %s", result.Error.Message)
	}

	// HTTPS to a HuaweiCloud host passes the entry checks (it will fail at
	// ResolveAndCheck or network since it's not a real endpoint, but the
	// rejection must NOT be the scheme/host guard).
	args, _ = json.Marshal(map[string]string{"url": "https://bss.myhuaweicloud.com/v2/bills"})
	result = tool.Execute(context.Background(), args)
	if result.Error != nil {
		msg := result.Error.Message
		if strings.Contains(msg, "not allowed") || strings.Contains(msg, "not a HuaweiCloud") {
			t.Fatalf("valid HuaweiCloud HTTPS URL rejected by scheme/host guard: %s", msg)
		}
	}
}

// TestHTTPRequest_NoCredentials verifies the credential guard.
func TestHTTPRequest_NoCredentials(t *testing.T) {
	tool := NewHTTPRequest("", "", "")
	args, _ := json.Marshal(map[string]string{"url": "https://bss.myhuaweicloud.com/v2/"})
	result := tool.Execute(context.Background(), args)
	if result.Error == nil {
		t.Fatal("expected error for missing credentials")
	}
	if !strings.Contains(result.Error.Message, "credentials not configured") {
		t.Fatalf("unexpected error: %s", result.Error.Message)
	}
}

// keep import used
var _ = hwcloud.ToolResult{}

// TestScrubURLError verifies that query string secrets are stripped from
// http.Client.Do errors before they reach the LLM context.
func TestScrubURLError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		contains   string
		notContains string
	}{
		{
			name:        "url.Error with query secret",
			err:        &url.Error{Op: "Get", URL: "https://bss.myhuaweicloud.com/v2/bills?token=SECRET&ak=LEAKED", Err: io.EOF},
			contains:   "https://bss.myhuaweicloud.com/v2/bills",
			notContains: "SECRET",
		},
		{
			name:        "url.Error with fragment",
			err:        &url.Error{Op: "Get", URL: "https://example.com/path#frag", Err: io.EOF},
			contains:   "https://example.com/path",
			notContains: "frag",
		},
		{
			name:        "non-url.Error passes through",
			err:        fmt.Errorf("some other error"),
			contains:   "http_request",
			notContains: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scrubbed := scrubURLError(c.err)
			msg := scrubbed.Error()
			if c.contains != "" && !strings.Contains(msg, c.contains) {
				t.Errorf("want %q in error, got: %s", c.contains, msg)
			}
			if c.notContains != "" && strings.Contains(msg, c.notContains) {
				t.Errorf("secret %q leaked into error: %s", c.notContains, msg)
			}
		})
	}
}

// ── security regression ──

// TestIsHuaweiCloudHost verifies the host allowlist that prevents AK
// exfiltration to attacker-controlled servers.
func TestIsHuaweiCloudHost(t *testing.T) {
	valid := []string{
		"bss.myhuaweicloud.com",
		"ecs.cn-east-3.myhuaweicloud.com",
		"bss.myhuaweicloud.cn",
		"bss.myhuaweicloud.eu",
		"bss.myhuaweicloud.com:443",  // with port
		"BSS.MYHUAWEICLOUD.COM",      // case-insensitive
		"bss.myhuaweicloud.com.",     // FQDN trailing dot
		"bss.myhuaweicloud.com.:8443", // FQDN trailing dot + port
	}
	for _, h := range valid {
		if !isHuaweiCloudHost(h) {
			t.Errorf("valid host rejected: %q", h)
		}
	}
	invalid := []string{
		"evil.example.com",
		"bss.myhuaweicloud.com.evil.com", // suffix trick attempt
		"evilmyhuaweicloud.com",          // prefix trick — no dot separator
		"169.254.169.254",
		"localhost",
		"myhuaweicloud.com", // bare apex — no leading dot, not a subdomain
	}
	for _, h := range invalid {
		if isHuaweiCloudHost(h) {
			t.Errorf("invalid host accepted: %q", h)
		}
	}
}

// TestHTTPRequest_RejectsNonHuaweiCloudHost is the regression test for the
// credential-exfiltration finding: the signed Authorization header (carrying
// the plaintext AK) must never be sent to a non-HuaweiCloud host.
func TestHTTPRequest_RejectsNonHuaweiCloudHost(t *testing.T) {
	tool := NewHTTPRequest("AKIDREAL", "SKREAL", "")
	args, _ := json.Marshal(map[string]string{"url": "https://evil.attacker.com/exfil"})
	result := tool.Execute(context.Background(), args)
	if result.Error == nil {
		t.Fatal("request to non-HuaweiCloud host must be rejected — AK would be exfiltrated via the signed Authorization header")
	}
	if !strings.Contains(result.Error.Message, "not a HuaweiCloud") {
		t.Fatalf("expected host-allowlist rejection, got: %s", result.Error.Message)
	}
}

// TestHTTPRequest_RejectsHTTP is the regression test for the cleartext-
// credential finding: the signed Authorization header must never be sent
// over HTTP, even to a valid HuaweiCloud host.
func TestHTTPRequest_RejectsHTTP(t *testing.T) {
	tool := NewHTTPRequest("AKIDREAL", "SKREAL", "")
	args, _ := json.Marshal(map[string]string{"url": "http://bss.myhuaweicloud.com/v2/bills"})
	result := tool.Execute(context.Background(), args)
	if result.Error == nil {
		t.Fatal("HTTP request must be rejected — cleartext leaks the signed Authorization header to network observers")
	}
	if !strings.Contains(result.Error.Message, "HTTPS") {
		t.Fatalf("expected HTTPS-only rejection, got: %s", result.Error.Message)
	}
}
