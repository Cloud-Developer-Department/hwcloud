package huaweicloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	opentool "github.com/Cloud-Developer-Department/hwcloud/tool"
	"github.com/Cloud-Developer-Department/hwcloud/utils"
)

// maxInlineBody is the maximum response body size returned inline to the LLM.
// Larger bodies are saved to the artifact directory and a file path is
// returned instead, so the LLM can use read/grep to inspect on demand
// without consuming context window.
const maxInlineBody = 8 * 1024

// HTTPRequest is an hwcloud.Tool that lets the server-side LLM make
// authenticated HTTP requests to HuaweiCloud APIs. The tool handles
// SDK-HMAC-SHA256 signing automatically — the LLM never sees AK/SK.
//
// The LLM provides method, url, optional headers, and optional body.
// The tool signs the request with the configured credentials, sends it,
// and returns the response status, headers, and body as JSON.
type HTTPRequest struct {
	ak            string
	sk            string
	securityToken string
	client        *http.Client
}

// NewHTTPRequest creates an HTTPRequest tool with the given credentials.
// Credentials are read from the environment at construction time (by the
// caller, typically HuaweiCloud.HTTPRequest()) and never exposed to the LLM.
//
// SSRF defense (the LLM's input originates from user natural language and
// is prompt-injectable — a crafted URL could reach 169.254.169.254 cloud
// metadata, localhost, or private networks, and the response would flow
// back into the model context). Four layers, mirroring utils/webhttp:
//  1. ValidateRequestURL at Execute entry (scheme/host/userinfo policy)
//  2. ResolveAndCheck on the parsed host (every resolved IP must be public)
//  3. DialContext re-validates the dial-time IP (defeats DNS rebinding)
//  4. Redirects disabled entirely — a signed BSS request must never hop.
func NewHTTPRequest(ak, sk, securityToken string) *HTTPRequest {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &HTTPRequest{
		ak:            ak,
		sk:            sk,
		securityToken: securityToken,
		client: &http.Client{
			// No client-level Timeout — the per-request deadline comes from
			// context.WithTimeout in Execute (default 30s, clamped to [1,120]s
			// via the timeout parameter). A fixed client timeout would cap the
			// parameter silently.
			// Disable redirects — BSS API should respond directly. Following
			// redirects could leak signed requests to unexpected hosts.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					// Re-validate the address actually being dialed — the
					// DNS-rebinding defense (validation-time lookup can see a
					// public IP while the dial-time lookup returns metadata).
					host, _, err := net.SplitHostPort(addr)
					if err != nil {
						host = addr
					}
					if err := utils.ResolveAndCheck(host); err != nil {
						return nil, err
					}
					return dialer.DialContext(ctx, network, addr)
				},
			},
		},
	}
}

// Definition returns the tool's function definition.
func (t *HTTPRequest) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "http_request",
		Description: "Send an HTTP request to a HuaweiCloud API. " +
			"SDK-HMAC-SHA256 authentication is handled automatically — do NOT pass credentials. " +
			"Returns {status, headers, body} as JSON. " +
			"Use this to call BSS pricing APIs and other HuaweiCloud service APIs.\n" +
			"\nPagination: for LIST APIs always pass a small page size first (e.g. limit=20) to confirm " +
			"the response structure, then page through with the API's pagination parameters. " +
			"HuaweiCloud OpenAPI uses mainly two pagination styles: offset/limit and marker/limit; " +
			"a few services use pageNo/pageLimit. Check the API reference (references/*.json) for the exact parameter names.\n" +
			"\nLarge responses: bodies over 8KB are saved to a file and a path is returned — use read/grep " +
			"on that file instead of re-querying, and read the structure first before grepping.",
		Parameters: hwcloud.SchemaOf[HwParams](),
	}
}

// Execute sends the HTTP request with automatic signing and returns the response.
func (t *HTTPRequest) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[HwParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("http_request: %w", err), false, "")
	}
	if t.ak == "" || t.sk == "" {
		return hwcloud.ErrorResult(fmt.Errorf("http_request: credentials not configured — set HW_ACCESS_KEY and HW_SECRET_KEY"), false, "")
	}
	method := strings.ToUpper(params.Method)
	if method == "" {
		method = "GET"
	}

	// SSRF entry checks: URL policy (scheme/host/userinfo) + every resolved
	// IP must be public. The DialContext in NewHTTPRequest re-validates at
	// dial time (DNS-rebinding defense).
	if _, err := utils.ValidateRequestURL(params.URL); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("http_request: url rejected: %w", err), false, "")
	}

	// Credential-exfiltration defense: the auto-signed Authorization header
	// carries the plaintext AK and (when present) the temporary security
	// token. Without a host allowlist, a prompt-injected LLM could direct
	// http_request to an attacker-controlled server and exfiltrate the AK.
	// Restrict to HuaweiCloud API domains. Also enforce HTTPS — sending the
	// signed Authorization over cleartext HTTP leaks credentials to any
	// network observer.
	parsed, err := url.Parse(params.URL)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("http_request: parse url: %w", err), false, "")
	}
	if parsed.Scheme != "https" {
		return hwcloud.ErrorResult(fmt.Errorf("http_request: scheme %q not allowed — HuaweiCloud API requests must use HTTPS to protect the signed Authorization header", parsed.Scheme), false, "")
	}
	if !isHuaweiCloudHost(parsed.Hostname()) {
		return hwcloud.ErrorResult(fmt.Errorf("http_request: host %q is not a HuaweiCloud API domain — the signed Authorization header would exfiltrate credentials to an untrusted host", parsed.Hostname()), false, "")
	}

	// Parse the URL into endpoint, path, and query for signing.
	// (parsed already validated above; reuse it.)
	if err := utils.ResolveAndCheck(parsed.Hostname()); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("http_request: %w", err), false, "")
	}

	// Query params for signing — pass url.Values directly so repeated keys
	// (e.g. ?tag=prod&tag=cn-east-3) are all included in the signature per
	// the SDK-HMAC-SHA256 spec, not just the first value.
	query := parsed.Query()

	// Endpoint is scheme://host (no path/query).
	endpoint := fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)

	// Request body bytes.
	var bodyBytes []byte
	if params.Body != "" {
		bodyBytes = []byte(params.Body)
	}

	// Sign the request.
	signedHeaders := Sign(method, endpoint, parsed.Path, query, bodyBytes, t.ak, t.sk, t.securityToken)

	// Clamp timeout.
	timeout := 30 * time.Second
	if params.Timeout > 0 {
		timeout = time.Duration(params.Timeout) * time.Second
		if timeout < 1*time.Second {
			timeout = 1 * time.Second
		}
		if timeout > 120*time.Second {
			timeout = 120 * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build the request.
	var bodyReader io.Reader
	if len(bodyBytes) > 0 {
		bodyReader = strings.NewReader(params.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, params.URL, bodyReader)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("http_request: create request: %w", err), false, "")
	}

	// Apply signed headers.
	for k, v := range signedHeaders {
		req.Header.Set(k, v)
	}

	// Apply user-provided headers (they win for non-signing headers).
	// Signing headers (Authorization, host, x-sdk-date, x-security-token)
	// are kept from Sign() — user cannot override them.
	signingKeys := map[string]bool{
		"Authorization":    true,
		"Host":             true,
		"X-Sdk-Date":       true,
		"X-Security-Token": true,
	}
	for k, v := range params.Headers {
		if signingKeys[http.CanonicalHeaderKey(k)] {
			continue // skip — signing headers are managed by Sign()
		}
		req.Header.Set(k, v)
	}

	// Default Content-Type for requests with a body.
	if len(bodyBytes) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	// Send.
	resp, err := t.client.Do(req)
	if err != nil {
		// http.Client.Do wraps errors as *url.Error containing the full URL
		// (including query string — which may carry tokens the LLM injected).
		// Scrub query+fragment before returning to the model context.
		return hwcloud.ErrorResult(scrubURLError(err), false, "")
	}
	defer utils.DrainAndClose(resp.Body)

	// Read response body (bounded to a generous limit for safety).
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024+1))
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("http_request: read response: %w", err), false, "")
	}

	// Collect response headers.
	respHeaders := make(map[string]string)
	for k, v := range resp.Header {
		if len(v) > 0 {
			respHeaders[k] = v[0]
		}
	}

	result := map[string]any{
		"status":  resp.StatusCode,
		"headers": respHeaders,
	}

	// Small body: return inline. Large body: save to artifact file and
	// return a path so the LLM can use read/grep to inspect on demand.
	// JSON bodies are parsed into nested structures (with number precision
	// preserved via UseNumber) so the final result marshals as readable
	// multi-line JSON — a raw string would be escaped to one long line.
	if len(respBody) <= maxInlineBody {
		result["body"] = parseBody(respBody)
	} else {
		dir := filepath.Join(opentool.ArtifactRoot(), "iac-server")
		_ = os.MkdirAll(dir, 0755)
		name := fmt.Sprintf("http_%d.json", time.Now().UnixNano())
		path := filepath.Join(dir, name)
		formatted, err := json.MarshalIndent(parseBody(respBody), "", "  ")
		if err != nil {
			return hwcloud.ErrorResult(fmt.Errorf("http_request: format body: %w", err), false, "")
		}
		if err := os.WriteFile(path, formatted, 0644); err != nil {
			// Fallback: return inline truncated.
			result["body"] = parseBody(respBody[:maxInlineBody])
			result["truncated"] = true
		} else {
			sizeKB := (len(respBody) + 1023) / 1024
			result["body"] = fmt.Sprintf("(response body saved to %s, %d KB — use read or grep to inspect)", path, sizeKB)
			result["body_path"] = path
		}
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("http_request: marshal result: %w", err), false, "")
	}
	return &hwcloud.ToolResult{Content: string(data)}
}

// parseBody decodes a JSON response body into a nested structure so it can
// be marshaled as readable multi-line JSON. Numbers are kept as json.Number
// (raw text) to avoid float64 precision loss on long cloud IDs. Non-JSON
// bodies (XML, plain text, ...) are returned as-is.
func parseBody(b []byte) any {
	if len(b) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(b)
	}
	// Reject trailing garbage after the first JSON value.
	if _, err := dec.Token(); err != io.EOF {
		return string(b)
	}
	return v
}

// HwParams are the arguments to http_request. URL is required; method
// defaults to GET.
type HwParams struct {
	Method  string            `json:"method,omitempty" jsonschema:"description=HTTP method (default: GET)"`
	URL     string            `json:"url" jsonschema:"description=Full URL, e.g. https://bss.myhuaweicloud.com/v2/bills/ratings/on-demand-resources"`
	Headers map[string]string `json:"headers,omitempty" jsonschema:"description=Optional extra headers (e.g. Content-Type). Do NOT pass Authorization or x-sdk-date — they are auto-signed."`
	Body    string            `json:"body,omitempty" jsonschema:"description=Optional request body (e.g. JSON string for POST requests)"`
	Timeout int               `json:"timeout,omitempty" jsonschema:"description=Request timeout in seconds (default: 30, min: 1, max: 120)"`
}

// scrubURLError strips the query string and fragment from the URL embedded in
// a *url.Error returned by http.Client.Do, so secrets the LLM injected into
// the query (e.g. ?token=xxx) don't echo back into the model context via the
// error message. Non-url.Error errors pass through unchanged.
func scrubURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		urlErr.URL = utils.ScrubURL(urlErr.URL)
		return urlErr
	}
	return fmt.Errorf("http_request: %w", err)
}

// isHuaweiCloudHost reports whether host is a HuaweiCloud API endpoint.
// The signed Authorization header (carrying the plaintext AK) is only safe
// to send to these domains — any other host would receive the credential.
// Covers the international (.com), Chinese (.cn), and European (.eu) domains.
func isHuaweiCloudHost(host string) bool {
	// Strip port if present.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	// Strip a trailing dot (FQDN root label) so "bss.myhuaweicloud.com." is
	// accepted — it is the same host as "bss.myhuaweicloud.com".
	host = strings.TrimSuffix(host, ".")
	// HuaweiCloud API domains end with a known suffix. We match on a leading-
	// dot suffix (not exact, not bare HasSuffix on the unsuffixed host) so
	// subdomains are allowed (bss.myhuaweicloud.com, ecs.cn-east-3.myhuaweicloud.com)
	// but prefix-confusers are not (evilmyhuaweicloud.com).
	suffixes := []string{
		".myhuaweicloud.com",
		".myhuaweicloud.cn",
		".myhuaweicloud.eu",
	}
	for _, sfx := range suffixes {
		if strings.HasSuffix(host, sfx) {
			return true
		}
	}
	return false
}
