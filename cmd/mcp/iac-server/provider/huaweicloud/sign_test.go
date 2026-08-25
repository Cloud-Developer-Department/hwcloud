package huaweicloud

import (
	"net/url"
	"strings"
	"testing"
)

// TestSign_HeaderFormat verifies the Authorization header has the correct
// SDK-HMAC-SHA256 scheme with Access, SignedHeaders, and Signature fields.
func TestSign_HeaderFormat(t *testing.T) {
	h := Sign("GET", "https://ecs.cn-east-3.myhuaweicloud.com", "/v1/cloudservers", nil, nil,
		"AKTEST", "SKTEST", "")
	auth := h["Authorization"]
	if !strings.HasPrefix(auth, "SDK-HMAC-SHA256 Access=AKTEST, SignedHeaders=") {
		t.Fatalf("bad auth prefix: %s", auth)
	}
	if !strings.Contains(auth, "Signature=") {
		t.Fatalf("missing Signature in: %s", auth)
	}
	if h["host"] != "ecs.cn-east-3.myhuaweicloud.com" {
		t.Fatalf("bad host: %s", h["host"])
	}
	if h["x-sdk-date"] == "" {
		t.Fatal("x-sdk-date should be set")
	}
	if _, ok := h["x-security-token"]; ok {
		t.Fatal("permanent creds: no security token expected")
	}
}

// TestSign_SecurityToken verifies the temporary credential token is included
// in both the signature and the headers.
func TestSign_SecurityToken(t *testing.T) {
	h := Sign("GET", "https://ecs.cn-east-3.myhuaweicloud.com", "/", nil, nil,
		"AK", "SK", "TOKEN123")
	auth := h["Authorization"]
	if !strings.Contains(auth, "x-security-token") {
		t.Fatalf("security token should be in signed headers: %s", auth)
	}
	if h["x-security-token"] != "TOKEN123" {
		t.Fatalf("security token header = %q, want TOKEN123", h["x-security-token"])
	}
}

// TestSign_Deterministic verifies that the signature is a pure function of
// its inputs. We verify determinism at the canonical-string level where
// timestamp is excluded.
func TestSign_Deterministic(t *testing.T) {
	// canonicalQueryString must produce identical output regardless of map
	// iteration order — Go maps randomize iteration, so two calls with the
	// same key-value pairs must yield the same string.
	q := url.Values{"z": []string{"9"}, "a": []string{"1"}, "m": []string{"5"}}
	c1 := canonicalQueryString(q)
	c2 := canonicalQueryString(q)
	if c1 != c2 {
		t.Fatalf("canonicalQueryString not deterministic: %q vs %q", c1, c2)
	}
	// Same pairs, different declaration order — must match.
	qA := url.Values{"b": []string{"2"}, "a": []string{"1"}, "c": []string{"3"}}
	qB := url.Values{"c": []string{"3"}, "a": []string{"1"}, "b": []string{"2"}}
	if canonicalQueryString(qA) != canonicalQueryString(qB) {
		t.Fatal("canonicalQueryString should be order-independent (sorted)")
	}
}

// TestSign_QuerySorting verifies that query parameter order does not affect
// the signature — the canonical query string sorts keys.
func TestSign_QuerySorting(t *testing.T) {
	q1 := url.Values{"b": []string{"2"}, "a": []string{"1"}, "c": []string{"3"}}
	q2 := url.Values{"a": []string{"1"}, "c": []string{"3"}, "b": []string{"2"}}
	// Directly compare canonical strings (timestamp-independent).
	if canonicalQueryString(q1) != canonicalQueryString(q2) {
		t.Fatalf("canonical query differs: %q vs %q", canonicalQueryString(q1), canonicalQueryString(q2))
	}
	// Also verify via Sign when timestamps match (same-second).
	h1 := Sign("GET", "https://x.myhuaweicloud.com", "/", q1, nil, "AK", "SK", "")
	h2 := Sign("GET", "https://x.myhuaweicloud.com", "/", q2, nil, "AK", "SK", "")
	if h1["x-sdk-date"] == h2["x-sdk-date"] && h1["Authorization"] != h2["Authorization"] {
		t.Fatal("same timestamp + same canonical query → different signature")
	}
}

// TestCanonicalQueryString_MultiValue verifies that repeated query keys (e.g.
// ?tag=prod&tag=cn-east-3) are all included in the canonical query string,
// sorted by key then by value — per the SDK-HMAC-SHA256 spec. The old code
// took only the first value (map[string]string), causing 401 on multi-value
// queries.
func TestCanonicalQueryString_MultiValue(t *testing.T) {
	q := url.Values{"tag": []string{"prod", "cn-east-3"}}
	got := canonicalQueryString(q)
	// Both values must appear, sorted by value within the same key.
	want := "tag=cn-east-3&tag=prod"
	if got != want {
		t.Fatalf("multi-value canonical query = %q, want %q", got, want)
	}
}

// TestCanonicalQueryString_MultiKeyMultiValue verifies sorting across
// repeated keys with multiple values each.
func TestCanonicalQueryString_MultiKeyMultiValue(t *testing.T) {
	q := url.Values{
		"tag":   []string{"prod", "cn-east-3"},
		"scope": []string{"enterprise", "project"},
	}
	got := canonicalQueryString(q)
	// Sorted by key (scope < tag), then by value within each key.
	want := "scope=enterprise&scope=project&tag=cn-east-3&tag=prod"
	if got != want {
		t.Fatalf("multi-key multi-value canonical query = %q, want %q", got, want)
	}
}

// TestCanonicalURI verifies URI encoding per the SDK algorithm.
func TestCanonicalURI(t *testing.T) {
	if got := canonicalURI("/"); got != "/" {
		t.Fatalf("root path: got %q want /", got)
	}
	if got := canonicalURI("/v1/cloudservers/"); got != "/v1/cloudservers/" {
		t.Fatalf("simple path: got %q", got)
	}
	if got := canonicalURI(""); got != "/" {
		t.Fatalf("empty path: got %q want /", got)
	}
}

// TestURLEncode verifies RFC 3986 encoding (unreserved chars not encoded).
func TestURLEncode(t *testing.T) {
	if got := urlEncode("abc-_.~"); got != "abc-_.~" {
		t.Fatalf("unreserved chars should not be encoded: got %q", got)
	}
	if got := urlEncode(" "); got != "%20" {
		t.Fatalf("space should be %%20: got %q", got)
	}
	if got := urlEncode("/"); got != "%2F" {
		t.Fatalf("slash should be encoded: got %q", got)
	}
}

// ── benchmarks ──

// BenchmarkSign measures the full SDK-HMAC-SHA256 signing path — called on
// every HuaweiCloud API request, so it's the hottest hot path in estimate_cost
// and query_cloud.
func BenchmarkSign(b *testing.B) {
	query := url.Values{}
	query.Set("limit", "20")
	query.Set("offset", "0")
	body := []byte(`{"region":"cn-east-3","flavor":"s6.large.2"}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Sign("POST", "https://bss.myhuaweicloud.com", "/v2/bills/ratings/on-demand-resources",
			query, body, "AKIDxxx", "SKxxx", "")
	}
}

// BenchmarkSign_MultiValueQuery measures signing with repeated query keys
// (the bug-fix case: ?tag=prod&tag=cn-east-3).
func BenchmarkSign_MultiValueQuery(b *testing.B) {
	query := url.Values{}
	query.Add("tag", "prod")
	query.Add("tag", "cn-east-3")
	query.Add("tag", "team-alpha")
	query.Set("limit", "50")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Sign("GET", "https://ecs.myhuaweicloud.com", "/v1/instances",
			query, nil, "AKIDxxx", "SKxxx", "security-token-xxx")
	}
}

// BenchmarkCanonicalQueryString measures the canonical query string builder
// in isolation — useful for comparing single vs multi-value overhead.
func BenchmarkCanonicalQueryString_Single(b *testing.B) {
	query := url.Values{}
	for i := 0; i < 10; i++ {
		query.Set("k"+string(rune('a'+i)), "v")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = canonicalQueryString(query)
	}
}

func BenchmarkCanonicalQueryString_Multi(b *testing.B) {
	query := url.Values{}
	for i := 0; i < 10; i++ {
		query.Add("tag", "v"+string(rune('a'+i)))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = canonicalQueryString(query)
	}
}

// BenchmarkCanonicalURI measures path encoding — called once per request.
func BenchmarkCanonicalURI(b *testing.B) {
	path := "/v2/bills/ratings/on-demand-resources/regions/cn-east-3"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = canonicalURI(path)
	}
}

// BenchmarkURLEncode measures the custom RFC 3986 encoder.
func BenchmarkURLEncode(b *testing.B) {
	s := "cn-east-3/some path/with spaces&special=chars"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = urlEncode(s)
	}
}
