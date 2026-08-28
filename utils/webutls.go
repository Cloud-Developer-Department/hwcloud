package utils

// webutls.go — utls (uTLS) transport that mimics Chrome's TLS ClientHello
// to pass WAFs that fingerprint at the TLS layer (e.g. Tencent EdgeOne on
// support.huaweicloud.com). Reuses the SSRF dial guard from webhttp.go.
//
// Why this exists: EdgeOne checks JA3/JA4 at the TLS handshake. Go's
// crypto/tls ClientHello is not Chrome's, so EdgeOne returns a 200
// "Security Verification" JS-challenge page instead of real content.
// utls rewrites the ClientHello (cipher suite order, extensions, GREASE)
// to match Chrome; the WAF lets us through. This is a TLS-layer fix —
// no JS execution, no headless browser, ~0 added latency.
//
// Why h2 + h1 fallback: utls negotiates h2 via ALPN, but Go's http.Transport
// doesn't recognize utls's *UConn as a TLS conn for h2 upgrade (it expects
// *tls.Conn), producing "malformed HTTP response" on h2 servers. We dial
// via http2.Transport.DialTLSContext (which accepts an already-handshaked
// conn) and fall back to http.Transport for h1-only servers.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// utlsHelloID is the ClientHello fingerprint to mimic. HelloChrome_Auto
// tracks the latest Chrome profile bundled with utls — update by bumping
// the utls module version, not by hard-coding a version-specific constant
// (those become stale as Chrome releases).
var utlsHelloID = utls.HelloChrome_Auto

// utlsDial dials a TCP connection, re-validates the IP (SSRF / DNS-rebinding
// defense — same ResolveAndCheck as safeDialContext), then performs a utls
// handshake with the Chrome ClientHello. The SSRF check runs at dial time,
// not just at request setup, so a DNS rebinding attack (first lookup
// returns public IP, dial-time lookup returns 169.254.169.254) is caught
// here exactly as it is in safeDialContext.
func utlsDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if err := ResolveAndCheck(host); err != nil {
		return nil, err
	}
	tcpConn, err := safeDialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	// utls.Config zero value: InsecureSkipVerify=false, so cert verification
	// is on (utls reuses crypto/tls's verify). No explicit TLSHandshakeTimeout
	// — the h1 transport sets it for h1, and h2 has no such field; the
	// per-request context (WebTimeout / caller timeout) bounds the handshake.
	tlsConn := utls.UClient(tcpConn, &utls.Config{ServerName: host}, utlsHelloID)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("utls handshake: %w", err)
	}
	return tlsConn, nil
}

// utlsTransport implements http.RoundTripper with Chrome-mimicking TLS.
// It tries h2 first (most modern sites negotiate h2 via ALPN); if h2
// fails it falls back to h1.
//
// Fallback triggers only on h2 *connection-layer* failure (h1-only server,
// h2 transport error). HTTP-level non-2xx (404, 500, …) is returned as-is
// and does NOT trigger a retry — that is correct, a 404 on h2 should not
// be re-fetched on h1.
//
// NOTE: fallback re-issues the request; safe for GET (Body==nil). If
// WebFetch ever gains a body, req.GetBody must be set so the h1 retry
// can rewind it.
type utlsTransport struct {
	h2 *http2.Transport
	h1 *http.Transport
}

func newUTLSTransport() *utlsTransport {
	return &utlsTransport{
		h2: &http2.Transport{
			AllowHTTP: false,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return utlsDial(ctx, network, addr)
			},
		},
		h1: &http.Transport{
			DialContext: safeDialContext, // plain http:// — SSRF guard, no TLS
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return utlsDial(ctx, network, addr)
			},
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func (t *utlsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.h2.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	return t.h1.RoundTrip(req)
}

// ── shared singleton utls HTTP client ──
//
// Parallel to SharedClient() but with the utls transport. Used by WebFetch
// (user-supplied URLs that may sit behind TLS-fingerprinting WAFs). WebSearch
// keeps using SharedClient() — search APIs (api.tavily.com, api.bochaai.com)
// don't fingerprint, and isolating the two means a utls regression can't
// break search.

var (
	sharedUTLSClientOnce sync.Once
	sharedUTLSClient     *http.Client
)

// SharedClientUTLS returns the process-wide utls HTTP client, creating it
// once. Same redirect policy (SafeCheckRedirect) and timeout as SharedClient;
// only the transport differs (utls Chrome fingerprint vs Go crypto/tls).
func SharedClientUTLS() *http.Client {
	sharedUTLSClientOnce.Do(func() {
		sharedUTLSClient = &http.Client{
			Timeout:       WebTimeout,
			CheckRedirect: SafeCheckRedirect,
			Transport:     newUTLSTransport(),
		}
	})
	return sharedUTLSClient
}
