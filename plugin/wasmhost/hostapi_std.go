package wasmhost

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// NewHostAPI constructs a HostAPI with the given keyring and sensible
// defaults for HTTP (net/http), logging (standard log adapter), and
// command execution (os/exec child process). Substitute any field to
// override a capability (e.g. a sandboxed Executor).
func NewHostAPI(kr Keyring) *HostAPI {
	return &HostAPI{
		Keyring:  kr,
		HTTP:     NewHTTPClient(),
		Logger:   &logAdapter{},
		Executor: NewStdExecutor(),
	}
}

// NewHTTPClient returns an HTTPClient with a bounded timeout and response
// size. Plugins are untrusted code — an unbounded request could hang the
// host goroutine or exhaust memory. Exported so non-HostAPI callers (e.g.
// the CLI plugin runtime) share one implementation.
func NewHTTPClient() HTTPClient {
	return &defaultHTTPClient{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// maxPluginResponseBytes caps a plugin's HTTP response body.
const maxPluginResponseBytes = 10 << 20 // 10 MiB

// defaultHTTPClient implements HTTPClient via net/http.
type defaultHTTPClient struct{ client *http.Client }

func (c *defaultHTTPClient) Do(method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxPluginResponseBytes+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response body: %w", err)
	}
	if len(respBody) > maxPluginResponseBytes {
		return resp.StatusCode, nil, fmt.Errorf("response exceeds %d bytes", maxPluginResponseBytes)
	}
	return resp.StatusCode, respBody, nil
}

// logAdapter implements Logger by forwarding to the standard log package.
type logAdapter struct{}

func (l *logAdapter) Info(msg string)  { slog.Info(msg, "source", "plugin") }
func (l *logAdapter) Warn(msg string)  { slog.Warn(msg, "source", "plugin") }
func (l *logAdapter) Error(msg string) { slog.Error(msg, "source", "plugin") }
