package wasm

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCompileRoute(t *testing.T) {
	r, err := compileRoute(Route{Method: "GET", Path: "/skills/{id}/run"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"skills", "{id}", "run"}
	if !reflect.DeepEqual(r.segments, want) {
		t.Fatalf("segments = %v, want %v", r.segments, want)
	}
	wantParams := []string{"", "id", ""}
	if !reflect.DeepEqual(r.params, wantParams) {
		t.Fatalf("params = %v, want %v", r.params, wantParams)
	}
}

func TestCompileRouteRejectsBadSegments(t *testing.T) {
	for _, p := range []string{"/skills/{/x}", "/a/{}", "/a/{x/y}"} {
		if _, err := compileRoute(Route{Method: "GET", Path: p}); err == nil {
			t.Fatalf("compileRoute(%q) must error", p)
		}
	}
}

func TestRouteMatch(t *testing.T) {
	r, err := compileRoute(Route{Method: "GET", Path: "/skills/{id}"})
	if err != nil {
		t.Fatal(err)
	}
	// exact method + shape
	params, ok := r.match("GET", "/skills/terraform")
	if !ok || params["id"] != "terraform" {
		t.Fatalf("match = %v, %v; want id=terraform", params, ok)
	}
	// wrong method
	if _, ok := r.match("POST", "/skills/terraform"); ok {
		t.Fatal("POST must not match a GET route")
	}
	// wrong segment count
	if _, ok := r.match("GET", "/skills"); ok {
		t.Fatal("short path must not match")
	}
	if _, ok := r.match("GET", "/skills/a/b"); ok {
		t.Fatal("long path must not match")
	}
	// literal mismatch
	if _, ok := r.match("GET", "/other/terraform"); ok {
		t.Fatal("literal segment mismatch must not match")
	}
}

func TestRouteMatchMultipleParams(t *testing.T) {
	r, err := compileRoute(Route{Method: "POST", Path: "/logs/{file}/tail/{n}"})
	if err != nil {
		t.Fatal(err)
	}
	params, ok := r.match("POST", "/logs/app.log/tail/50")
	if !ok {
		t.Fatal("must match")
	}
	if params["file"] != "app.log" || params["n"] != "50" {
		t.Fatalf("params = %v, want file=app.log n=50", params)
	}
}

func TestRouteMatchTrailingSlash(t *testing.T) {
	// Declared "/skills" must not match "/skills/" (segment count differs)
	// and "/skills//x" is never a match.
	r, _ := compileRoute(Route{Method: "GET", Path: "/skills"})
	if _, ok := r.match("GET", "/skills/"); ok {
		t.Fatal("/skills/ must not match /skills")
	}
}

func TestRegisterHTTPRoutesValidation(t *testing.T) {
	// Bad method is rejected before any module inspection.
	if err := RegisterHTTPRoutes(nil, CLIPluginMeta{
		Name:   "p",
		Routes: []Route{{Method: "TRACE", Path: "/x"}},
	}); err == nil {
		t.Fatal("TRACE method must be rejected")
	}
	// Duplicate route is rejected.
	if err := RegisterHTTPRoutes(nil, CLIPluginMeta{
		Name: "p",
		Routes: []Route{
			{Method: "GET", Path: "/x"},
			{Method: "GET", Path: "/x"},
		},
	}); err == nil {
		t.Fatal("duplicate route must be rejected")
	}
	// No routes is rejected.
	if err := RegisterHTTPRoutes(nil, CLIPluginMeta{Name: "p"}); err == nil {
		t.Fatal("empty routes must be rejected")
	}
	// Dot-dot path is rejected.
	if err := RegisterHTTPRoutes(nil, CLIPluginMeta{
		Name:   "p",
		Routes: []Route{{Method: "GET", Path: "/../x"}},
	}); err == nil {
		t.Fatal("dot-dot path must be rejected")
	}
}

func TestFirstHeadersLowercasesKeys(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "test-ua")
	h.Set("X-Custom", "v1")
	h.Add("X-Custom", "v2") // second value dropped
	out := firstHeaders(h)
	if out["user-agent"] != "test-ua" {
		t.Fatalf("user-agent = %q, want test-ua (key must be lowercase)", out["user-agent"])
	}
	if out["x-custom"] != "v1" {
		t.Fatalf("x-custom = %q, want v1 (first value)", out["x-custom"])
	}
	if _, ok := out["User-Agent"]; ok {
		t.Fatal("canonical-case key must not leak")
	}
}

func TestRegisterHTTPRoutesRejectsQueryInPath(t *testing.T) {
	if err := RegisterHTTPRoutes(nil, CLIPluginMeta{
		Name:   "p",
		Routes: []Route{{Method: "GET", Path: "/skills?x=1"}},
	}); err == nil {
		t.Fatal("path with '?' must be rejected")
	}
}

func TestFirstValueMap(t *testing.T) {
	out := firstValueMap(map[string][]string{
		"a": {"1", "2"},
		"b": {"3"},
	})
	if out["a"] != "1" || out["b"] != "3" {
		t.Fatalf("firstValueMap = %v, want a=1 b=3", out)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
}

// slowReader dribbles one byte at a time with a pause — simulates the
// slow-body attack a time-bound read must cut off.
type slowReader struct {
	remaining int
	pause     time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.remaining == 0 {
		return 0, io.EOF
	}
	time.Sleep(s.pause)
	p[0] = 'x'
	s.remaining--
	return 1, nil
}

func (s *slowReader) Close() error { return nil }

func TestReadBodyNormal(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader("hello"))
	body, err := readBody(req, 1<<20, time.Second)
	if err != nil || string(body) != "hello" {
		t.Fatalf("readBody = %q, %v; want hello, nil", body, err)
	}
}

func TestReadBodyOverCap(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader("12345"))
	_, err := readBody(req, 4, time.Second)
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("err = %v, want errBodyTooLarge", err)
	}
}

func TestReadBodyTimeout(t *testing.T) {
	req := httptest.NewRequest("POST", "/", &slowReader{remaining: 10, pause: 50 * time.Millisecond})
	start := time.Now()
	_, err := readBody(req, 1<<20, 100*time.Millisecond)
	if err == nil || strings.Contains(err.Error(), "timed out") == false {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took %v, want ~100ms", elapsed)
	}
	// The reader goroutine must have exited after the body close — it
	// finishes the remaining bytes in the background; nothing to assert
	// beyond the bounded return.
}
