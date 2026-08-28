package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/Cloud-Developer-Department/hwcloud/plugin/wasmhost"
)

// ── cli:http plugin dispatcher ──
//
// A cli:http plugin declares routes in its metadata; the host registers
// them under /api/plugins/<plugin-name>/... and dispatches matching HTTP
// requests to the plugin's handle_request export. The HTTP protocol
// layer (matching, params, concurrency, timeouts, error codes) lives
// entirely in this host-side dispatcher; the guest only implements
// request→response logic.
//
// Request input (host → guest):
//
//	{
//	  "method": "GET",
//	  "path": "/skills/terraform",     // host-stripped, matches a declared route
//	  "params": {"id": "terraform"},   // {param} segments
//	  "query": "a=1&b=2",              // raw query string
//	  "headers": {"authorization": "..."},  // full set, first value per key
//	  "body": ""                       // raw request body (≤ maxPluginRequestBody)
//	}
//
// Response output (guest → host):
//
//	{
//	  "status": 200,
//	  "headers": {"content-type": "application/json"},  // optional
//	  "body": "{\"ok\":true}"
//	}

// maxPluginRequestBody bounds a cli:http request body before it is
// serialized into the guest heap (the guest shares its linear memory,
// grown on demand via dlmalloc + memory.grow up to 512 MiB, between
// request, logic, and response).
const maxPluginRequestBody = 1 << 20 // 1 MiB

// maxPluginResponseBody bounds the response body a plugin may return.
// The guest must allocate the full JSON to serialize it; a body near
// the 512 MiB cap would make alloc fail — reject earlier with a
// specific error instead.
const maxPluginResponseBody = 3 << 20 // 3 MiB

// handlerTimeout bounds one guest invocation. A stuck plugin must never
// wedge the HTTP server — wazero interrupts the call when the context
// expires. Sized to cover a plugin's exec_command default (120s): a
// handler that runs a command must not be cut off mid-command.
const handlerTimeout = 120 * time.Second

// HTTPRequest is the JSON payload sent to a plugin's handle_request.
type HTTPRequest struct {
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	Params      map[string]string `json:"params,omitempty"`
	Query       string            `json:"query,omitempty"`
	QueryParams map[string]string `json:"query_params,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body,omitempty"`
}

// HTTPResponse is the JSON payload a plugin's handle_request returns.
type HTTPResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// ── registration ──

// httpModule is one cli:http plugin's registered routes.
type httpModule struct {
	name   string
	mod    api.Module
	mu     sync.Mutex // per-plugin serialization — the module is single-threaded
	routes []*httpRoute
}

// httpRoute is one compiled route template.
type httpRoute struct {
	method   string
	segments []string // literal segments; "{...}" = param
	params   []string // param names, aligned with segments ("" for literal)
	path     string   // declared path template, for logging
}

// httpPlugins is the process-level registration table for cli:http
// plugins. Registered at load time (loadPlugins), served by the HTTP
// dispatcher mounted in serve.
var httpPlugins sync.Map // plugin name → *httpModule

// RegisterHTTPRoutes registers a cli:http plugin's declared routes.
// Returns an error when the metadata is invalid (bad method, path, a
// missing handle_request export, or a duplicate route within the
// plugin) — the caller skips the plugin.
func RegisterHTTPRoutes(m *Module, meta CLIPluginMeta) error {
	// Metadata is validated FIRST (independent of the module) so a bad
	// declaration is rejected even before export inspection.
	if len(meta.Routes) == 0 {
		return fmt.Errorf("cli:http plugin %q declares no routes", meta.Name)
	}
	// A second plugin claiming the same name would silently shadow the
	// first — reject instead (re-registration after Unregister is fine).
	if _, exists := httpPlugins.Load(meta.Name); exists {
		return fmt.Errorf("cli:http plugin %q already registered", meta.Name)
	}
	hm := &httpModule{name: meta.Name}
	seen := make(map[string]bool, len(meta.Routes))
	for _, r := range meta.Routes {
		if !validMethod(r.Method) {
			return fmt.Errorf("cli:http plugin %q: invalid method %q", meta.Name, r.Method)
		}
		if !strings.HasPrefix(r.Path, "/") || strings.Contains(r.Path, "..") || strings.Contains(r.Path, "?") {
			// '?' never belongs in a declared path — query strings arrive
			// on the request, not in the template.
			return fmt.Errorf("cli:http plugin %q: invalid path %q", meta.Name, r.Path)
		}
		key := r.Method + " " + r.Path
		if seen[key] {
			return fmt.Errorf("cli:http plugin %q: duplicate route %s", meta.Name, key)
		}
		seen[key] = true
		route, err := compileRoute(r)
		if err != nil {
			return fmt.Errorf("cli:http plugin %q: route %s: %w", meta.Name, key, err)
		}
		hm.routes = append(hm.routes, route)
	}
	if m == nil || m.Mod.ExportedFunction("handle_request") == nil {
		return fmt.Errorf("cli:http plugin %q has no handle_request export", meta.Name)
	}
	hm.mod = m.Mod
	httpPlugins.Store(meta.Name, hm)
	return nil
}

func validMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

// compileRoute splits a path template into segments, marking {param}
// segments and collecting their names.
func compileRoute(r Route) (*httpRoute, error) {
	raw := strings.Split(strings.TrimPrefix(r.Path, "/"), "/")
	route := &httpRoute{method: r.Method, path: r.Path}
	for _, seg := range raw {
		if seg == "" {
			continue
		}
		if strings.ContainsAny(seg, "{}") {
			// Braces are reserved for {param} — a segment that mixes them
			// with anything else (e.g. "{/x}" split by the slash, or a
			// literal "{foo}bar") is a declaration error, not a template.
			if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
				return nil, fmt.Errorf("bad segment %q: braces only allowed as {param}", seg)
			}
			name := seg[1 : len(seg)-1]
			if name == "" || strings.ContainsAny(name, "{}") {
				return nil, fmt.Errorf("bad param segment %q", seg)
			}
			route.segments = append(route.segments, seg)
			route.params = append(route.params, name)
		} else {
			route.segments = append(route.segments, seg)
			route.params = append(route.params, "")
		}
	}
	return route, nil
}

// match checks a request path against the template, returning the
// extracted params when it matches.
//
// A {param} segment rejects an empty value: "/skills/" must not match
// "/skills/{name}" with name="". Without this guard, a trailing slash
// would route a list request to the detail handler with an empty name.
func (rt *httpRoute) match(method, path string) (map[string]string, bool) {
	if rt.method != method {
		return nil, false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != len(rt.segments) {
		return nil, false
	}
	var params map[string]string
	for i, seg := range parts {
		if rt.params[i] != "" {
			// A param segment must capture a non-empty value.
			if seg == "" {
				return nil, false
			}
			if params == nil {
				params = make(map[string]string)
			}
			params[rt.params[i]] = seg
			continue
		}
		if seg != rt.segments[i] {
			return nil, false
		}
	}
	return params, true
}

// UnregisterHTTPRoutes removes a plugin's routes (process exit / reload).
func UnregisterHTTPRoutes(name string) {
	httpPlugins.Delete(name)
}

// HTTPHandler returns the dispatcher that serves every cli:http plugin
// under /api/plugins/... Returns nil when no cli:http plugin is loaded.
func HTTPHandler() http.Handler {
	return &httpDispatcher{}
}

// httpDispatcher routes /api/plugins/<name>/<path> to the registered
// plugin and its matching route template.
type httpDispatcher struct{}

func (d *httpDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Path is /api/plugins/<name>/<rest>; the name is a single segment.
	rest := strings.TrimPrefix(r.URL.Path, "/api/plugins/")
	name, restPath, ok := strings.Cut(rest, "/")
	if !ok || name == "" {
		writeJSONError(w, http.StatusNotFound, "expected /api/plugins/<plugin>/<path>")
		return
	}
	v, ok := httpPlugins.Load(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "plugin not found")
		return
	}
	m := v.(*httpModule)

	var params map[string]string
	matched := false
	for _, route := range m.routes {
		if p, ok := route.match(r.Method, "/"+restPath); ok {
			params = p // nil for a param-less route — still a match
			matched = true
			break
		}
	}
	if !matched {
		writeJSONError(w, http.StatusNotFound, "no matching route")
		return
	}

	// Read the body under the cap BEFORE touching the guest. The read is
	// also time-bounded: MaxBytesReader caps SIZE, not rate — a client
	// dribbling bytes would otherwise hold the connection and its HTTP
	// goroutine indefinitely (slow-body DoS).
	body, err := readBody(r, maxPluginRequestBody, bodyReadTimeout)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeJSONError(w, http.StatusRequestTimeout, "request body read timed out")
		}
		return
	}

	req := HTTPRequest{
		Method:      r.Method,
		Path:        "/" + restPath,
		Params:      params,
		Query:       r.URL.RawQuery,
		QueryParams: firstValueMap(r.URL.Query()),
		Headers:     firstHeaders(r.Header),
		Body:        string(body),
	}
	input, err := json.Marshal(req)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), handlerTimeout)
	defer cancel()

	// Per-plugin serialization: the wasm module is single-threaded and
	// concurrent invocations would interleave. One slow plugin blocks
	// only its own routes.
	m.mu.Lock()
	defer m.mu.Unlock()

	output, err := wasmhost.CallWithInput(ctx, m.mod, "handle_request", input)
	if err != nil {
		// trap (plugin panic / stuck call killed by the timeout).
		if ctx.Err() != nil {
			log.Printf("cli:http plugin %s: handler timed out after %s", name, handlerTimeout)
			writeJSONError(w, http.StatusGatewayTimeout, "plugin handler timed out")
			return
		}
		log.Printf("cli:http plugin %s: handler error: %v", name, err)
		writeJSONError(w, http.StatusInternalServerError, "plugin handler failed")
		return
	}

	var resp HTTPResponse
	if err := json.Unmarshal(output, &resp); err != nil {
		log.Printf("cli:http plugin %s: unparseable response: %v", name, err)
		writeJSONError(w, http.StatusInternalServerError, "plugin returned an invalid response")
		return
	}
	if len(resp.Body) > maxPluginResponseBody {
		log.Printf("cli:http plugin %s: response body too large (%d bytes)", name, len(resp.Body))
		writeJSONError(w, http.StatusInternalServerError,
			fmt.Sprintf("plugin response too large (%d bytes)", len(resp.Body)))
		return
	}
	if resp.Status == 0 {
		resp.Status = http.StatusOK
	}
	// Default to JSON when the plugin did not set a content type — Go's
	// sniffer would otherwise label JSON bodies text/plain.
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.Status)
	w.Write([]byte(resp.Body))
}

// writeJSONError writes an error response as JSON with the right
// Content-Type (http.Error emits text/plain; plugin consumers parse
// the {"error": ...} body as JSON).
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(`{"error":` + strconv.Quote(msg) + `}`))
}

// firstValueMap flattens url.Values to one value per key (the first).
func firstValueMap(v map[string][]string) map[string]string {
	if len(v) == 0 {
		return nil
	}
	out := make(map[string]string, len(v))
	for k, vs := range v {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

// bodyReadTimeout bounds one request-body read. MaxBytesReader caps
// size, not rate: without a time bound a client dribbling bytes holds
// the connection (and its HTTP goroutine) indefinitely.
const bodyReadTimeout = 90 * time.Second

// errBodyTooLarge distinguishes an over-cap body from a timed-out read.
var errBodyTooLarge = errors.New("request body too large")

// readBody reads the request body under the cap with a time bound. On
// timeout the body is closed so the underlying read goroutine unblocks
// and exits (no leak — closing the connection aborts the read).
func readBody(r *http.Request, cap int64, timeout time.Duration) ([]byte, error) {
	// w=nil: MaxBytesReader only calls the writer on over-cap (to write
	// the 413); the dispatcher writes the JSON error response instead.
	r.Body = http.MaxBytesReader(nil, r.Body, cap)
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r.Body)
		ch <- result{data, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			var mbe *http.MaxBytesError
			if errors.As(res.err, &mbe) {
				return nil, errBodyTooLarge
			}
			return nil, res.err
		}
		return res.data, nil
	case <-time.After(timeout):
		r.Body.Close() // abort the read; the goroutine exits on the error
		return nil, errors.New("request body read timed out")
	}
}

// firstHeaders flattens a request's headers to one value per key (the
// first), lowercased — the guest contract is "headers keys are
// lowercase" (http.Header canonicalizes to "User-Agent", which a guest
// looking up "user-agent" would miss).
func firstHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}
