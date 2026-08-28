package tool

import (
	"context"
	"sync"

	"github.com/chromedp/cdproto/target"
)

// browserMaxTimeout caps a caller-supplied timeout for browser operations.
// A browser action (navigate + render + snapshot) is far slower than an HTTP
// GET, so the ceiling is higher than WebFetch's 120s.
const browserMaxTimeout = 120 // seconds

// browserDefaultTimeout is used when the caller supplies no/invalid timeout.
const browserDefaultTimeout = 60 // seconds

// browserMaxContentLen caps the visible text extracted from a rendered page,
// mirroring WebFetch's defaultMaxChars so browser output stays within the same
// token budget the model expects from web tools.
const browserMaxContentLen = 50000

// resolveBrowserTimeout clamps a caller-supplied seconds value into
// [1, browserMaxTimeout], falling back to browserDefaultTimeout when
// non-positive.
func resolveBrowserTimeout(secs int) int {
	if secs <= 0 {
		return browserDefaultTimeout
	}
	if secs > browserMaxTimeout {
		return browserMaxTimeout
	}
	return secs
}

// ── snapshot element ──

// browserUseElement is one indexed interactive element returned by a snapshot.
// The model references elements by Index (the [ref] stamped on the DOM) when
// calling browser_use_click / browser_use_type. Field names match the JSON
// keys emitted by browserUseSnapshotScript() exactly (camelCase from JS).
type browserUseElement struct {
	Index       int      `json:"index"`
	Tag         string   `json:"tag"`
	Text        string   `json:"text"`
	AriaLabel   string   `json:"ariaLabel"`
	Placeholder string   `json:"placeholder"`
	Value       string   `json:"value"`
	Options     []string `json:"options"`
	Href        string   `json:"href"`
	Role        string   `json:"role"`
	X           int      `json:"x"`
	Y           int      `json:"y"`
	Width       int      `json:"width"`
	Height      int      `json:"height"`
}

// browserUseTab describes one Chrome tab. Controlled marks the tab the agent
// drives; Protected marks tabs the agent must not touch (e.g. the OpenAgent UI
// in the extension mode — unused on headless servers but kept for parity).
type browserUseTab struct {
	Index      int       `json:"index"`
	ID         target.ID `json:"id"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	Active     bool      `json:"active"`
	Controlled bool      `json:"controlled"`
	Protected  bool      `json:"protected"`
}

// ── session management ──

// browserUseSession holds one persistent Chrome instance + its contexts. A
// session is reused across tool calls so cookies, localStorage, and the
// rendered DOM survive between actions — essential for multi-step browser
// automation (login → navigate → interact).
type browserUseSession struct {
	mu sync.Mutex

	// Context stack (cancel in reverse order of creation):
	//   allocCtx  → browserCtx → ctx (per-target, switchable)
	allocCtx    context.Context
	allocCancel context.CancelFunc
	browserCtx  context.Context
	browserCxl  context.CancelFunc
	// ctx is the current controlled-tab context; switchToTargetLocked
	// re-derives it when the agent switches tabs.
	ctx       context.Context
	ctxCancel context.CancelFunc

	// execPath is the Chrome binary used (CfT download or system Chrome).
	execPath string
	// userDataDir persists the profile (cookies, localStorage) across
	// sessions even after closeLocked tears down the browser process.
	userDataDir string
	// mode labels the session for logging ("Chrome for Testing").
	mode string

	// targetID is the active controlled tab. chromedp.NewContext targets
	// one tab; switching tabs re-derives a context for the new target.
	targetID target.ID
}

// browserUseManager maps a session key to a live browserUseSession. The key
// is derived from (userDataDir, mode) so two agents with different profiles
// get isolated Chrome instances while the same profile reuses one.
type browserUseManager struct {
	mu       sync.Mutex
	sessions map[string]*browserUseSession
}

// globalBrowserUseMgr is the process-wide singleton, mirroring
// utils.SharedClient's sharedHTTPClientOnce pattern. All browser_use_* tools
// share it so a multi-turn agent reuses the same Chrome instance.
var globalBrowserUseMgr = &browserUseManager{sessions: map[string]*browserUseSession{}}

// browserUsePositiveInt extracts a 1-based positive int from an untyped
// argument value (JSON numbers arrive as float64). Returns an error for
// non-numeric or non-positive values.
func browserUsePositiveInt(v any, name string) (int, error) {
	switch n := v.(type) {
	case float64:
		if n < 1 {
			return 0, &intParseError{name: name, reason: "must be a positive integer"}
		}
		return int(n), nil
	case int:
		if n < 1 {
			return 0, &intParseError{name: name, reason: "must be a positive integer"}
		}
		return n, nil
	default:
		return 0, &intParseError{name: name, reason: "must be a positive integer"}
	}
}

type intParseError struct {
	name   string
	reason string
}

func (e *intParseError) Error() string { return e.name + " " + e.reason }
