package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/utils"
	"github.com/Cloud-Developer-Department/hwcloud/version"
)

// Persistent browser-use session tools.
//
// Unlike the one-shot browser_* tools, the browser_use_* tools share one
// long-lived Chrome instance (per profile) so cookies, localStorage, and
// rendered DOM survive across calls. This is the right tool family for
// multi-step automation: login → navigate → click → type → read.
//
// The snapshot model tags interactive elements with [ref] indexes so the
// model can click/type by index without writing CSS selectors — far more
// reliable on dynamic pages where selectors shift.

const (
	browserUseDefaultTimeout = 30 * time.Second
	browserUseMaxElements    = 120
)

// ── session manager ──

// get returns the session for the default profile, creating a new one if
// absent. All browser_use_* tools share a single profile (and thus a single
// Chrome instance) per process — matching the one-user, one-session server
// deployment.
func (m *browserUseManager) get() *browserUseSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := defaultBrowserUseDataDir() + "|chrome-for-testing"
	s, ok := m.sessions[key]
	if !ok {
		s = &browserUseSession{
			userDataDir: defaultBrowserUseDataDir(),
			mode:        "Chrome for Testing",
		}
		m.sessions[key] = s
	}
	return s
}

// run executes chromedp actions against the session's controlled tab under a
// per-action timeout. The session is lazily started (ensureLocked) and reused.
func (m *browserUseManager) run(actions ...chromedp.Action) error {
	session := m.get()
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.ensureLocked(); err != nil {
		return err
	}
	timeoutCtx, cancel := context.WithTimeout(session.ctx, browserUseDefaultTimeout)
	defer cancel()
	return chromedp.Run(timeoutCtx, actions...)
}

// runSession runs fn with the session held under its lock, after ensuring the
// browser is started. Used by tools that need direct session access (tab
// switching, target inspection).
func (m *browserUseManager) runSession(fn func(session *browserUseSession) error) error {
	session := m.get()
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.ensureLocked(); err != nil {
		return err
	}
	return fn(session)
}

// ensureLocked starts (or reuses) the Chrome instance. A CDP probe (Targets)
// detects a dead browser so it can be restarted transparently.
func (s *browserUseSession) ensureLocked() error {
	if s.browserCtx != nil {
		probeCtx, cancel := context.WithTimeout(s.browserCtx, 2*time.Second)
		defer cancel()
		if _, err := chromedp.Targets(probeCtx); err == nil {
			if s.ctx == nil {
				s.ctx = s.browserCtx
			}
			return nil
		}
		s.closeLocked()
	}

	if err := os.MkdirAll(s.userDataDir, 0o755); err != nil {
		return fmt.Errorf("create browser profile dir %s: %w", s.userDataDir, err)
	}

	// Resolve Chrome binary (system Chrome or CfT download).
	execPath, err := resolveChromePath(context.Background())
	if err != nil {
		return err
	}
	s.execPath = execPath

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(execPath),
		// headless="new": the server has no display. New headless renders
		// identically to headful (unlike legacy --headless=old).
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("mute-audio", false),
		chromedp.Flag("hide-scrollbars", false),
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.UserDataDir(s.userDataDir),
		chromedp.UserAgent(webUserAgent),
		chromedp.WindowSize(1280, 900),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(func(string, ...interface{}) {}),
		chromedp.WithErrorf(func(string, ...interface{}) {}),
	)
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		return fmt.Errorf("start browser use session: %w", err)
	}
	s.allocCtx, s.allocCancel = allocCtx, allocCancel
	s.browserCtx, s.browserCxl = browserCtx, browserCancel
	s.ctx = browserCtx
	if c := chromedp.FromContext(browserCtx); c != nil && c.Target != nil {
		s.targetID = c.Target.TargetID
	}
	return nil
}

func (s *browserUseSession) closeLocked() {
	if s.ctxCancel != nil {
		s.ctxCancel()
	}
	if s.browserCxl != nil {
		s.browserCxl()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}
	s.ctx = nil
	s.browserCtx = nil
	s.ctxCancel = nil
	s.browserCxl = nil
	s.allocCancel = nil
	s.targetID = ""
}

// pageTargetsLocked lists open page-type tabs (excluding devtools://).
func (s *browserUseSession) pageTargetsLocked() ([]*target.Info, error) {
	timeoutCtx, cancel := context.WithTimeout(s.browserCtx, browserUseDefaultTimeout)
	defer cancel()
	infos, err := chromedp.Targets(timeoutCtx)
	if err != nil {
		return nil, err
	}
	var tabs []*target.Info
	for _, info := range infos {
		if info.Type != "page" || strings.HasPrefix(info.URL, "devtools://") {
			continue
		}
		tabs = append(tabs, info)
	}
	return tabs, nil
}

func (s *browserUseSession) currentTargetIDLocked() target.ID {
	if s.targetID != "" {
		return s.targetID
	}
	if c := chromedp.FromContext(s.ctx); c != nil && c.Target != nil {
		return c.Target.TargetID
	}
	return ""
}

// switchToTargetLocked activates targetID and re-derives the chromedp context
// so subsequent actions run against the new tab.
func (s *browserUseSession) switchToTargetLocked(targetID target.ID) error {
	if targetID == "" {
		return fmt.Errorf("missing tab target id")
	}
	found := false
	tabs, err := s.pageTargetsLocked()
	if err != nil {
		return err
	}
	for _, tab := range tabs {
		if tab.TargetID == targetID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("tab target %s not found", targetID)
	}
	targetCtx, cancel := chromedp.NewContext(s.browserCtx, chromedp.WithTargetID(targetID))
	err = chromedp.Run(targetCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		c := chromedp.FromContext(ctx)
		if c == nil || c.Browser == nil {
			return fmt.Errorf("browser context is not ready")
		}
		return target.ActivateTarget(targetID).Do(cdp.WithExecutor(ctx, c.Browser))
	}))
	if err != nil {
		cancel()
		return err
	}
	if s.ctxCancel != nil {
		s.ctxCancel()
	}
	s.ctx = targetCtx
	s.ctxCancel = cancel
	s.targetID = targetID
	return nil
}

// switchToNewTargetLocked detects a tab opened by a click/keypress and
// switches to it. Returns whether a switch happened.
func (s *browserUseSession) switchToNewTargetLocked(before map[target.ID]bool, previousURL string) (bool, error) {
	var fallback target.ID
	for attempt := 0; attempt < 20; attempt++ {
		tabs, err := s.pageTargetsLocked()
		if err != nil {
			return false, err
		}
		for _, tab := range tabs {
			if before[tab.TargetID] {
				continue
			}
			if fallback == "" {
				fallback = tab.TargetID
			}
			if strings.TrimSpace(tab.URL) == "" || tab.URL == "about:blank" {
				continue
			}
			if previousURL != "" && tab.URL == previousURL && attempt < 19 {
				continue
			}
			if err := s.switchToTargetLocked(tab.TargetID); err != nil {
				return false, err
			}
			return true, nil
		}
		if fallback != "" {
			time.Sleep(250 * time.Millisecond)
		}
	}
	if fallback != "" {
		if err := s.switchToTargetLocked(fallback); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// defaultBrowserUseDataDir is the persistent Chrome profile directory.
// Cookies and localStorage survive here across sessions.
func defaultBrowserUseDataDir() string {
	if configDir, err := os.UserConfigDir(); err == nil && configDir != "" {
		return filepath.Join(configDir, version.Name, "browser-use")
	}
	return filepath.Join(os.TempDir(), "hwcloud-browser-use")
}

// ── snapshot JavaScript ──

func browserUseSnapshotScript() string {
	return fmt.Sprintf(`(() => {
  const maxElements = %d;
  const isVisible = (el) => {
    if (el === document.documentElement || el === document.body) {
      return false;
    }
    const style = window.getComputedStyle(el);
    if (!style || style.visibility === 'hidden' || style.display === 'none' || Number(style.opacity) === 0) {
      return false;
    }
    const rect = el.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
  };
  const isInteractive = (el) => {
    const tag = el.tagName.toLowerCase();
    if (['a', 'button', 'input', 'textarea', 'select', 'summary', 'audio', 'video', 'label'].includes(tag)) {
      return true;
    }
    const role = el.getAttribute('role') || '';
    if (['button', 'link', 'menuitem', 'option', 'tab', 'checkbox', 'radio', 'switch'].includes(role)) {
      return true;
    }
    if (el.hasAttribute('onclick') || el.hasAttribute('contenteditable')) {
      return true;
    }
    if (el.tabIndex >= 0) {
      return true;
    }
    const style = window.getComputedStyle(el);
    return style && style.cursor === 'pointer';
  };
  const priorityOf = (el) => {
    const tag = el.tagName.toLowerCase();
    if (tag === 'button' || tag === 'input' || tag === 'textarea' || tag === 'select') {
      return 0;
    }
    const role = el.getAttribute('role') || '';
    if (role === 'button' || role === 'menuitem') {
      return 1;
    }
    if (tag === 'audio' || tag === 'video') {
      return 2;
    }
    return 3;
  };
  const textOf = (el) => {
    const parts = [
      el.innerText,
      el.getAttribute('aria-label'),
      el.getAttribute('title'),
      el.getAttribute('placeholder'),
      el.value
    ].filter(Boolean);
    return parts.join(' ').replace(/\s+/g, ' ').trim().slice(0, 180);
  };
  const nodes = Array.from(document.querySelectorAll('*'))
    .filter(isVisible)
    .filter(isInteractive)
    .filter((el) => {
      const tag = el.tagName.toLowerCase();
      return textOf(el) || tag === 'input' || tag === 'textarea' || tag === 'audio' || tag === 'video';
    })
    .map((el, order) => ({el, order}))
    .sort((a, b) => priorityOf(a.el) - priorityOf(b.el) || a.order - b.order)
    .map((item) => item.el)
    .slice(0, maxElements);
  document.querySelectorAll('[data-hwcloud-browser-use-ref]').forEach((el) => {
    el.removeAttribute('data-hwcloud-browser-use-ref');
  });
  return nodes.map((el, index) => {
    const ref = String(index + 1);
    el.setAttribute('data-hwcloud-browser-use-ref', ref);
    const rect = el.getBoundingClientRect();
    return {
      index: index + 1,
      tag: el.tagName.toLowerCase(),
      text: textOf(el),
      ariaLabel: el.getAttribute('aria-label') || '',
      placeholder: el.getAttribute('placeholder') || '',
      value: el.value || '',
      options: el.tagName.toLowerCase() === 'select'
        ? Array.from(el.options).map((option) => (option.text || option.value || '').replace(/\s+/g, ' ').trim()).filter(Boolean).slice(0, 20)
        : [],
      href: (el.href || '').slice(0, 240),
      role: el.getAttribute('role') || '',
      x: Math.round(rect.x),
      y: Math.round(rect.y),
      width: Math.round(rect.width),
      height: Math.round(rect.height)
    };
  });
})()`, browserUseMaxElements)
}

func browserUseVisibleTextScript() string {
	return `(() => {
  const text = (document.body && document.body.innerText) || '';
  return text.replace(/\s+/g, ' ').trim().slice(0, 4000);
})()`
}

func browserUsePlayMediaScript() string {
	return `(() => {
  const media = Array.from(document.querySelectorAll('video,audio'));
  if (media.length === 0) {
    return 'No audio or video elements found on the current tab.';
  }
  const visibleMedia = media.filter((item) => {
    const rect = item.getBoundingClientRect();
    const style = window.getComputedStyle(item);
    return rect.width > 0 && rect.height > 0 && style.display !== 'none' && style.visibility !== 'hidden';
  });
  const candidates = visibleMedia.length ? visibleMedia : media;
  const reports = [];
  for (const item of candidates) {
    item.muted = false;
    item.volume = 1;
    try {
      const promise = item.play();
      if (promise && typeof promise.catch === 'function') {
        promise.catch(() => {});
      }
      reports.push(item.tagName.toLowerCase() + ': playing=' + (!item.paused) + ' muted=' + item.muted + ' volume=' + item.volume + ' currentTime=' + Math.round(item.currentTime || 0));
    } catch (error) {
      reports.push(item.tagName.toLowerCase() + ': play failed: ' + (error && error.message ? error.message : String(error)));
    }
  }
  return reports.join('\n');
})()`
}

// browserUseJSONLiteral marshals a string for safe embedding into JS.
func browserUseJSONLiteral(value string) string {
	data, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(data)
}

func browserUseSelectAllModifier() input.Modifier {
	if runtime.GOOS == "darwin" {
		return input.ModifierMeta
	}
	return input.ModifierCtrl
}

func browserUseKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "enter", "return":
		return kb.Enter
	case "tab":
		return kb.Tab
	case "escape", "esc":
		return kb.Escape
	case "space", "spacebar":
		return " "
	case "backspace":
		return kb.Backspace
	case "delete", "del":
		return kb.Delete
	case "arrowup", "up":
		return kb.ArrowUp
	case "arrowdown", "down":
		return kb.ArrowDown
	case "arrowleft", "left":
		return kb.ArrowLeft
	case "arrowright", "right":
		return kb.ArrowRight
	case "home":
		return kb.Home
	case "end":
		return kb.End
	case "pageup":
		return kb.PageUp
	case "pagedown":
		return kb.PageDown
	default:
		return key
	}
}

// browserUseSelector resolves an index (from a snapshot) or a raw CSS selector
// into a CSS selector string. Index → [data-hwcloud-browser-use-ref="N"].
func browserUseSelector(index int, selector string) (string, error) {
	if index > 0 {
		return fmt.Sprintf(`[data-hwcloud-browser-use-ref="%d"]`, index), nil
	}
	if s := strings.TrimSpace(selector); s != "" {
		return s, nil
	}
	return "", fmt.Errorf("missing required parameter: index or selector")
}

// ── formatting ──

func browserUseFormatSnapshot(rawURL, title, visibleText string, elements []browserUseElement) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("URL: %s\nTitle: %s\n\n", rawURL, title))
	if strings.TrimSpace(visibleText) != "" {
		b.WriteString("Visible text:\n")
		b.WriteString(visibleText)
		b.WriteString("\n\n")
	}
	b.WriteString("Interactive elements:\n")
	if len(elements) == 0 {
		b.WriteString("No visible interactive elements found.\n")
		return b.String()
	}
	for _, e := range elements {
		label := strings.TrimSpace(e.Text)
		if label == "" {
			label = strings.TrimSpace(e.AriaLabel)
		}
		if label == "" {
			label = strings.TrimSpace(e.Placeholder)
		}
		if label == "" {
			label = strings.TrimSpace(e.Value)
		}
		line := fmt.Sprintf("[%d] <%s", e.Index, e.Tag)
		if e.Role != "" {
			line += fmt.Sprintf(` role=%q`, e.Role)
		}
		if e.Href != "" {
			line += fmt.Sprintf(` href=%q`, e.Href)
		}
		line += fmt.Sprintf("> %s", label)
		if len(e.Options) > 0 {
			line += fmt.Sprintf(" options=%q", e.Options)
		}
		line += fmt.Sprintf(" (x=%d y=%d w=%d h=%d)", e.X, e.Y, e.Width, e.Height)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func browserUseFormatTabs(tabs []browserUseTab) string {
	if len(tabs) == 0 {
		return "No browser tabs found."
	}
	var b strings.Builder
	b.WriteString("Browser tabs:\n")
	for _, tab := range tabs {
		var markers []string
		if tab.Active {
			markers = append(markers, "active")
		}
		if tab.Controlled {
			markers = append(markers, "controlled")
		}
		if tab.Protected {
			markers = append(markers, "protected")
		}
		markerText := ""
		if len(markers) > 0 {
			markerText = " " + strings.Join(markers, " ")
		}
		b.WriteString(fmt.Sprintf("[%d]%s %s\n", tab.Index, markerText, strings.TrimSpace(tab.Title)))
		if strings.TrimSpace(tab.URL) != "" {
			b.WriteString(fmt.Sprintf("    %s\n", utils.ScrubURL(tab.URL)))
		}
	}
	return b.String()
}

// ── snapshot + state helpers ──

func browserUseSnapshot() (string, error) {
	var elements []browserUseElement
	var title, rawURL, visibleText string
	err := globalBrowserUseMgr.run(
		chromedp.Title(&title),
		chromedp.Location(&rawURL),
		chromedp.Evaluate(browserUseVisibleTextScript(), &visibleText),
		chromedp.Evaluate(browserUseSnapshotScript(), &elements),
	)
	if err != nil {
		return "", err
	}
	return browserUseFormatSnapshot(utils.ScrubURL(rawURL), title, visibleText, elements), nil
}

func browserUseCurrentState() (string, error) {
	var state strings.Builder
	state.WriteString("Current browser state:\n")
	err := globalBrowserUseMgr.runSession(func(session *browserUseSession) error {
		activeID := session.currentTargetIDLocked()
		tabs, err := session.pageTargetsLocked()
		if err != nil {
			return err
		}
		activeText := "unknown"
		for i, tab := range tabs {
			if tab.TargetID == activeID {
				activeText = fmt.Sprintf("%d/%d", i+1, len(tabs))
				break
			}
		}
		state.WriteString(fmt.Sprintf("- Controlled tab: %s\n", activeText))
		return nil
	})
	if err != nil {
		return "", err
	}
	return state.String(), nil
}

// browserUseTextWithState appends current browser state to a success message.
func browserUseTextWithState(text string) *hwcloud.ToolResult {
	state, err := browserUseCurrentState()
	if err != nil {
		return &hwcloud.ToolResult{Content: text + "\n\nCurrent browser state: unavailable: " + err.Error()}
	}
	return &hwcloud.ToolResult{Content: text + "\n\n" + state}
}

// browserUseErrorWithState appends current browser state to an error message.
func browserUseErrorWithState(toolName, text string) *hwcloud.ToolResult {
	state, err := browserUseCurrentState()
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("%s: %s\n\nCurrent browser state: unavailable: %s", toolName, text, err.Error()), false, "")
	}
	return hwcloud.ErrorResult(fmt.Errorf("%s: %s\n\n%s", toolName, text, state), false, "")
}

// ── tool 1: browser_use_open ──

type browserUseOpenTool struct{}

type browserUseOpenParams struct {
	URL string `json:"url" jsonschema:"description=The URL to open in the controlled tab"`
}

func (t *browserUseOpenTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "browser_use_open",
		Description: "Navigate the persistent controlled browser tab to a URL, wait for render, and return a snapshot (URL, title, visible text, indexed interactive elements). " +
			"The browser instance persists across calls (cookies/localStorage kept). " +
			"Use browser_use_open for multi-step automation (login → click → type → read across pages); use browser_navigate for a single one-shot fetch that needs no interaction. " +
			"Prefer webfetch first for static content — only use browser tools when webfetch fails or interaction is needed.",
		Parameters: hwcloud.SchemaOf[browserUseOpenParams](),
	}
}

func (t *browserUseOpenTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[browserUseOpenParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_use_open: %w", err), false, "")
	}
	if _, err := utils.ValidateRequestURL(p.URL); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_use_open: %w", err), false, "")
	}
	if err := globalBrowserUseMgr.run(chromedp.Navigate(p.URL), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return browserUseErrorWithState("browser_use_open", fmt.Sprintf("navigate failed for %s: %s", utils.ScrubURL(p.URL), err.Error()))
	}
	snap, err := browserUseSnapshot()
	if err != nil {
		return browserUseErrorWithState("browser_use_open", fmt.Sprintf("snapshot failed after opening %s: %s", utils.ScrubURL(p.URL), err.Error()))
	}
	return &hwcloud.ToolResult{Content: utils.WrapUntrusted(snap)}
}

// ── tool 2: browser_use_snapshot ──

type browserUseSnapshotTool struct{}

func (t *browserUseSnapshotTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "browser_use_snapshot",
		Description: "Read the controlled browser tab and return URL, title, visible text, and up to 120 indexed interactive elements. Treat this as the source of truth before acting. Call it after every navigation, click, type, or key press before reusing element indexes.",
		Parameters:  hwcloud.SchemaOf[struct{}](),
	}
}

func (t *browserUseSnapshotTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	snap, err := browserUseSnapshot()
	if err != nil {
		return browserUseErrorWithState("browser_use_snapshot", "snapshot failed: "+err.Error())
	}
	return &hwcloud.ToolResult{Content: utils.WrapUntrusted(snap)}
}

// ── tool 3: browser_use_click ──

type browserUseClickTool struct{}

type browserUseClickParams struct {
	Index    int    `json:"index,omitempty" jsonschema:"description=Element index from the latest browser_use_snapshot"`
	Selector string `json:"selector,omitempty" jsonschema:"description=CSS selector when no index is available"`
}

func (t *browserUseClickTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "browser_use_click",
		Description: "Click an indexed element from the latest browser_use_snapshot, or a CSS selector when no index is available. " +
			"The click may navigate or open a new tab (auto-switched). Old indexes are stale afterward — call browser_use_snapshot before the next indexed action.",
		Parameters: hwcloud.SchemaOf[browserUseClickParams](),
	}
}

func (t *browserUseClickTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[browserUseClickParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_use_click: %w", err), false, "")
	}
	selector, err := browserUseSelector(p.Index, p.Selector)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_use_click: %w", err), false, "")
	}
	switchedTab := false
	err = globalBrowserUseMgr.runSession(func(session *browserUseSession) error {
		var previousURL string
		beforeTargets, terr := session.pageTargetsLocked()
		if terr != nil {
			return terr
		}
		before := map[target.ID]bool{}
		for _, item := range beforeTargets {
			before[item.TargetID] = true
			if item.TargetID == session.currentTargetIDLocked() {
				previousURL = item.URL
			}
		}
		timeoutCtx, cancel := context.WithTimeout(session.ctx, browserUseDefaultTimeout)
		defer cancel()
		if runErr := chromedp.Run(timeoutCtx,
			chromedp.ScrollIntoView(selector, chromedp.ByQuery),
			chromedp.Click(selector, chromedp.ByQuery),
			chromedp.Sleep(800*time.Millisecond),
		); runErr != nil {
			return runErr
		}
		var switchErr error
		switchedTab, switchErr = session.switchToNewTargetLocked(before, previousURL)
		return switchErr
	})
	if err != nil {
		return browserUseErrorWithState("browser_use_click", fmt.Sprintf("click failed for %s: %s", selector, err.Error()))
	}
	if switchedTab {
		return browserUseTextWithState("Clicked and switched to the new tab. Call browser_use_snapshot before the next indexed action.")
	}
	return browserUseTextWithState("Clicked. Call browser_use_snapshot before the next indexed action.")
}

// ── tool 4: browser_use_type ──

type browserUseTypeTool struct{}

type browserUseTypeParams struct {
	Index    int    `json:"index,omitempty" jsonschema:"description=Element index from the latest browser_use_snapshot"`
	Selector string `json:"selector,omitempty" jsonschema:"description=CSS selector when no index is available"`
	Text     string `json:"text" jsonschema:"description=Text to type"`
	Clear    bool   `json:"clear,omitempty" jsonschema:"description=Whether to clear the current field value before typing (default true)"`
}

func (t *browserUseTypeTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "browser_use_type",
		Description: "Type text into an indexed input/textarea/select element or a CSS selector from the latest snapshot. " +
			"Handles select options and contenteditable. Set clear=true (default) to replace the field. Verify with browser_use_snapshot before relying on indexes.",
		Parameters: hwcloud.SchemaOf[browserUseTypeParams](),
	}
}

func (t *browserUseTypeTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[browserUseTypeParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_use_type: %w", err), false, "")
	}
	selector, err := browserUseSelector(p.Index, p.Selector)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_use_type: %w", err), false, "")
	}
	clear := true
	// ParseArgs zero-initializes Clear to false; treat "absent" as true (the
	// default) by checking the raw args for the key.
	if len(args) > 0 {
		var raw map[string]json.RawMessage
		if json.Unmarshal(args, &raw) == nil {
			if _, ok := raw["clear"]; ok {
				clear = p.Clear
			}
		}
	}

	actions := []chromedp.Action{
		chromedp.ScrollIntoView(selector, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
		chromedp.Sleep(100 * time.Millisecond),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var tag string
			if err := chromedp.Evaluate(browserUseElementTagScript(selector), &tag).Do(ctx); err != nil {
				return err
			}
			if tag == "select" {
				var result string
				if err := chromedp.Evaluate(browserUseSelectOptionScript(selector, p.Text), &result).Do(ctx); err != nil {
					return err
				}
				if strings.HasPrefix(result, "select option not found") {
					return fmt.Errorf("%s", result)
				}
				return nil
			}
			var setValueResult string
			if err := chromedp.Evaluate(browserUseSetTextValueScript(selector, p.Text, clear), &setValueResult).Do(ctx); err != nil {
				return err
			}
			if setValueResult != "fallback" {
				if strings.HasPrefix(setValueResult, "element not found") {
					return fmt.Errorf("%s", setValueResult)
				}
				return nil
			}
			// Fallback: select-all + backspace + insert.
			if clear {
				if err := chromedp.KeyEvent("a", chromedp.KeyModifiers(browserUseSelectAllModifier())).Do(ctx); err != nil {
					return err
				}
				if err := chromedp.KeyEvent(kb.Backspace).Do(ctx); err != nil {
					return err
				}
			}
			return input.InsertText(p.Text).Do(ctx)
		}),
		chromedp.Sleep(300 * time.Millisecond),
	}
	if err := globalBrowserUseMgr.run(actions...); err != nil {
		return browserUseErrorWithState("browser_use_type", fmt.Sprintf("type failed for %s: %s", selector, err.Error()))
	}
	return browserUseTextWithState("Typed. Call browser_use_snapshot before the next indexed action or before claiming the page accepted the input.")
}

// ── tool 5: browser_use_press ──

type browserUsePressTool struct{}

type browserUsePressParams struct {
	Key string `json:"key" jsonschema:"description=Keyboard key to press (Enter, Tab, Escape, ArrowDown, Space, etc.)"`
}

func (t *browserUsePressTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "browser_use_press",
		Description: "Press a keyboard key in the controlled tab (Enter, Tab, Escape, ArrowDown, Space, etc.). May submit a form or navigate; call browser_use_snapshot before the next indexed action.",
		Parameters:  hwcloud.SchemaOf[browserUsePressParams](),
	}
}

func (t *browserUsePressTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[browserUsePressParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_use_press: %w", err), false, "")
	}
	key := browserUseKey(p.Key)
	switchedTab := false
	err = globalBrowserUseMgr.runSession(func(session *browserUseSession) error {
		var previousURL string
		beforeTargets, terr := session.pageTargetsLocked()
		if terr != nil {
			return terr
		}
		before := map[target.ID]bool{}
		for _, item := range beforeTargets {
			before[item.TargetID] = true
			if item.TargetID == session.currentTargetIDLocked() {
				previousURL = item.URL
			}
		}
		timeoutCtx, cancel := context.WithTimeout(session.ctx, browserUseDefaultTimeout)
		defer cancel()
		if runErr := chromedp.Run(timeoutCtx, chromedp.KeyEvent(key), chromedp.Sleep(800*time.Millisecond)); runErr != nil {
			return runErr
		}
		var switchErr error
		switchedTab, switchErr = session.switchToNewTargetLocked(before, previousURL)
		return switchErr
	})
	if err != nil {
		return browserUseErrorWithState("browser_use_press", fmt.Sprintf("press failed for %s: %s", p.Key, err.Error()))
	}
	if switchedTab {
		return browserUseTextWithState("Key pressed and switched to the new tab. Call browser_use_snapshot before the next indexed action.")
	}
	return browserUseTextWithState("Key pressed. Call browser_use_snapshot before the next indexed action.")
}

// ── tool 6: browser_use_play_media ──

type browserUsePlayMediaTool struct{}

func (t *browserUsePlayMediaTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "browser_use_play_media",
		Description: "Unmute and play visible audio/video elements on the controlled tab. Use after opening a page with media if playback is paused or muted.",
		Parameters:  hwcloud.SchemaOf[struct{}](),
	}
}

func (t *browserUsePlayMediaTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	var result string
	if err := globalBrowserUseMgr.run(chromedp.Evaluate(browserUsePlayMediaScript(), &result)); err != nil {
		return browserUseErrorWithState("browser_use_play_media", "play media failed: "+err.Error())
	}
	// result is JS-returned page content — wrap as untrusted.
	return browserUseTextWithState(utils.WrapUntrusted(result))
}

// ── tool 7: browser_use_tabs ──

type browserUseTabsTool struct{}

func (t *browserUseTabsTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "browser_use_tabs",
		Description: "List open browser tabs with active/controlled markers, titles, and URLs. Use before switching tabs or when the current page does not match what the user sees.",
		Parameters:  hwcloud.SchemaOf[struct{}](),
	}
}

func (t *browserUseTabsTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	tabs, err := browserUseListTabs()
	if err != nil {
		return browserUseErrorWithState("browser_use_tabs", "tabs failed: "+err.Error())
	}
	return browserUseTextWithState(browserUseFormatTabs(tabs))
}

func browserUseListTabs() ([]browserUseTab, error) {
	var tabs []browserUseTab
	err := globalBrowserUseMgr.runSession(func(session *browserUseSession) error {
		activeID := session.currentTargetIDLocked()
		targets, err := session.pageTargetsLocked()
		if err != nil {
			return err
		}
		for i, info := range targets {
			tabs = append(tabs, browserUseTab{
				Index:      i + 1,
				ID:         info.TargetID,
				Title:      info.Title,
				URL:        info.URL,
				Active:     info.TargetID == activeID,
				Controlled: info.TargetID == activeID,
			})
		}
		return nil
	})
	return tabs, err
}

// ── tool 8: browser_use_switch_tab ──

type browserUseSwitchTabTool struct{}

type browserUseSwitchTabParams struct {
	Index int `json:"index" jsonschema:"description=Tab index returned by browser_use_tabs"`
}

func (t *browserUseSwitchTabTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "browser_use_switch_tab",
		Description: "Switch the controlled tab to one returned by browser_use_tabs. Returns a fresh snapshot for the selected tab.",
		Parameters:  hwcloud.SchemaOf[browserUseSwitchTabParams](),
	}
}

func (t *browserUseSwitchTabTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[browserUseSwitchTabParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_use_switch_tab: %w", err), false, "")
	}
	if p.Index < 1 {
		return hwcloud.ErrorResult(fmt.Errorf("browser_use_switch_tab: index must be a positive integer"), false, "")
	}
	err = globalBrowserUseMgr.runSession(func(session *browserUseSession) error {
		tabs, err := session.pageTargetsLocked()
		if err != nil {
			return err
		}
		if p.Index > len(tabs) {
			return fmt.Errorf("tab index %d is out of range; there are %d tabs", p.Index, len(tabs))
		}
		return session.switchToTargetLocked(tabs[p.Index-1].TargetID)
	})
	if err != nil {
		return browserUseErrorWithState("browser_use_switch_tab", "switch tab failed: "+err.Error())
	}
	snap, err := browserUseSnapshot()
	if err != nil {
		return browserUseErrorWithState("browser_use_switch_tab", "snapshot failed after switching: "+err.Error())
	}
	return &hwcloud.ToolResult{Content: utils.WrapUntrusted(snap)}
}

// ── tool 9: browser_use_close_tab ──

type browserUseCloseTabTool struct{}

type browserUseCloseTabParams struct {
	Index int `json:"index" jsonschema:"description=Tab index returned by browser_use_tabs"`
}

func (t *browserUseCloseTabTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "browser_use_close_tab",
		Description: "Close a tab returned by browser_use_tabs. If the active tab is closed, switches to the first remaining tab.",
		Parameters:  hwcloud.SchemaOf[browserUseCloseTabParams](),
	}
}

func (t *browserUseCloseTabTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[browserUseCloseTabParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_use_close_tab: %w", err), false, "")
	}
	if p.Index < 1 {
		return hwcloud.ErrorResult(fmt.Errorf("browser_use_close_tab: index must be a positive integer"), false, "")
	}
	err = globalBrowserUseMgr.runSession(func(session *browserUseSession) error {
		tabs, err := session.pageTargetsLocked()
		if err != nil {
			return err
		}
		if p.Index > len(tabs) {
			return fmt.Errorf("tab index %d is out of range; there are %d tabs", p.Index, len(tabs))
		}
		targetID := tabs[p.Index-1].TargetID
		active := targetID == session.currentTargetIDLocked()
		c := chromedp.FromContext(session.browserCtx)
		if c == nil || c.Browser == nil {
			return fmt.Errorf("browser context is not ready")
		}
		timeoutCtx, cancel := context.WithTimeout(session.browserCtx, browserUseDefaultTimeout)
		defer cancel()
		if err = target.CloseTarget(targetID).Do(cdp.WithExecutor(timeoutCtx, c.Browser)); err != nil {
			return err
		}
		if active {
			time.Sleep(300 * time.Millisecond)
			tabs, err = session.pageTargetsLocked()
			if err != nil {
				return err
			}
			if len(tabs) > 0 {
				return session.switchToTargetLocked(tabs[0].TargetID)
			}
			session.targetID = ""
			session.ctx = session.browserCtx
		}
		return nil
	})
	if err != nil {
		return browserUseErrorWithState("browser_use_close_tab", "close tab failed: "+err.Error())
	}
	return browserUseTextWithState(fmt.Sprintf("Closed tab %d.", p.Index))
}

// ── tool 10: browser_use_close ──

type browserUseCloseTool struct{}

func (t *browserUseCloseTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "browser_use_close",
		Description: "Close the persistent browser session (kills Chrome). Only use when the user explicitly asks to stop browser use; the profile dir persists for cookie reuse on next start.",
		Parameters:  hwcloud.SchemaOf[struct{}](),
	}
}

func (t *browserUseCloseTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	session := globalBrowserUseMgr.get()
	session.mu.Lock()
	session.closeLocked()
	session.mu.Unlock()
	return &hwcloud.ToolResult{Content: "Browser session closed. The profile directory was preserved for cookie reuse on next start."}
}

// ── type JS scripts (used by browser_use_type) ──

func browserUseElementTagScript(selector string) string {
	return fmt.Sprintf(`(() => {
  const el = document.querySelector(%s);
  return el ? el.tagName.toLowerCase() : '';
})()`, browserUseJSONLiteral(selector))
}

func browserUseSelectOptionScript(selector, text string) string {
	return fmt.Sprintf(`(() => {
  const el = document.querySelector(%s);
  if (!el) {
    return 'select element not found';
  }
  if (el.tagName.toLowerCase() !== 'select') {
    return 'target is not a select element';
  }
  const expected = %s;
  const normalizedExpected = expected.trim().toLowerCase();
  const options = Array.from(el.options);
  const option = options.find((item) => item.value === expected || (item.text || '').trim() === expected) ||
    options.find((item) => item.value.toLowerCase() === normalizedExpected || (item.text || '').trim().toLowerCase() === normalizedExpected);
  if (!option) {
    return 'select option not found: ' + expected + '. Options: ' + options.map((item) => item.text || item.value).join(', ');
  }
  el.value = option.value;
  option.selected = true;
  el.dispatchEvent(new Event('input', {bubbles: true}));
  el.dispatchEvent(new Event('change', {bubbles: true}));
  return 'Selected option: ' + (option.text || option.value);
})()`, browserUseJSONLiteral(selector), browserUseJSONLiteral(text))
}

func browserUseSetTextValueScript(selector, text string, clear bool) string {
	return fmt.Sprintf(`(() => {
  const el = document.querySelector(%s);
  if (!el) {
    return 'element not found';
  }
  if (!%t) {
    return 'fallback';
  }
  const tag = el.tagName.toLowerCase();
  if (tag !== 'input' && tag !== 'textarea') {
    return 'fallback';
  }
  if (tag === 'input') {
    const type = (el.getAttribute('type') || 'text').toLowerCase();
    if (['button', 'checkbox', 'color', 'file', 'hidden', 'image', 'radio', 'range', 'reset', 'submit'].includes(type)) {
      return 'fallback';
    }
  }
  const value = %s;
  const proto = tag === 'textarea' ? window.HTMLTextAreaElement.prototype : window.HTMLInputElement.prototype;
  const descriptor = Object.getOwnPropertyDescriptor(proto, 'value');
  if (descriptor && descriptor.set) {
    descriptor.set.call(el, value);
  } else {
    el.value = value;
  }
  try {
    el.dispatchEvent(new InputEvent('input', {bubbles: true, inputType: 'insertText', data: value}));
  } catch (e) {
    el.dispatchEvent(new Event('input', {bubbles: true}));
  }
  el.dispatchEvent(new Event('change', {bubbles: true}));
  return 'set text value';
})()`, browserUseJSONLiteral(selector), clear, browserUseJSONLiteral(text))
}

// NewBrowserUseTools returns the ten persistent-session browser tools.
func NewBrowserUseTools() []hwcloud.Tool {
	return []hwcloud.Tool{
		&browserUseOpenTool{},
		&browserUseSnapshotTool{},
		&browserUseClickTool{},
		&browserUseTypeTool{},
		&browserUsePressTool{},
		&browserUsePlayMediaTool{},
		&browserUseTabsTool{},
		&browserUseSwitchTabTool{},
		&browserUseCloseTabTool{},
		&browserUseCloseTool{},
	}
}
