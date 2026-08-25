package tool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/chromedp"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/utils"
)

// One-shot headless browser tools.
//
// Each tool spins up a fresh headless Chrome, performs one action, and tears
// it down. This is simpler than the persistent browser_use session and is the
// right tool for single-page fetches where the agent does not need to keep
// state across calls (the common case for "fetch a page that blocks WebFetch").
//
// For multi-step automation (login → navigate → interact), use the
// browser_use_* tools instead — they reuse one Chrome instance.

// browserFetchWait is the fixed sleep after WaitReady so JS-rendered SPAs have
// time to populate the DOM before OuterHTML is captured. WaitReady("body")
// fires as soon as <body> exists, not when the SPA has finished rendering.
const browserFetchWait = 10 * time.Second

// newBrowserCtx creates an allocator + browser context with the given timeout.
// The Chrome binary is resolved via resolveChromePath (system Chrome or CfT
// download). The context is cancelled (Chrome killed) when the caller defers
// cancel. Returns an error if the Chrome binary cannot be resolved (so the
// caller can format it into an ErrorResult rather than a confusing
// context-cancelled symptom).
func newBrowserCtx(parent context.Context, timeoutSecs int) (context.Context, context.CancelFunc, error) {
	secs := resolveBrowserTimeout(timeoutSecs)
	timeout := time.Duration(secs) * time.Second

	// Resolve the Chrome binary once per process (the CfT download is slow).
	execPath, err := resolveChromePath(parent)
	if err != nil {
		return nil, func() {}, err
	}

	opts := chromeAllocatorOptions(execPath)
	allocCtx, allocCancel := chromedp.NewExecAllocator(parent, opts...)
	ctx, ctxCancel := chromedp.NewContext(allocCtx)
	ctx, timeoutCancel := context.WithTimeout(ctx, timeout)

	return ctx, func() {
		timeoutCancel()
		ctxCancel()
		allocCancel()
	}, nil
}

// ── browser_navigate ──

// browserNavigateTool navigates to a URL and returns the rendered visible
// text. Unlike WebFetch (plain HTTP GET), this drives a real Chrome so JS
// executes, SPAs render, and WAF fingerprint checks (Cloudflare) pass.
type browserNavigateTool struct{}

type browserNavigateParams struct {
	URL          string `json:"url" jsonschema:"description=URL to navigate to (rendered with JavaScript)"`
	WaitSelector string `json:"wait_selector,omitempty" jsonschema:"description=Optional CSS selector to wait for before extracting content (e.g. #main-content). If omitted, waits for body ready then a short render delay."`
	Timeout      int    `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds (default 60, max 120)"`
}

func (t *browserNavigateTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "browser_navigate",
		Description: "Navigate to a URL using a real headless browser (Chrome), execute JavaScript, wait for the page to fully render, and return the visible text content. " +
			"Use this as a FALLBACK when webfetch fails: returns empty/blocked content, a security challenge page (e.g. Cloudflare), or a JS-rendered SPA that needs execution. " +
			"Do NOT use browser_navigate as the first choice for static pages — prefer webfetch (faster, no browser process). " +
			"For multi-step interaction (login, click, type across pages), use browser_use_open instead — it keeps a persistent session.",
		Parameters: hwcloud.SchemaOf[browserNavigateParams](),
	}
}

func (t *browserNavigateTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[browserNavigateParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_navigate: %w", err), false, "")
	}
	if _, err := utils.ValidateRequestURL(p.URL); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_navigate: %w", err), false, "")
	}

	var extra chromedp.Action
	if ws := strings.TrimSpace(p.WaitSelector); ws != "" {
		extra = chromedp.WaitVisible(ws, chromedp.ByQuery)
	} else {
		extra = chromedp.Sleep(browserFetchWait)
	}

	text, title, finalURL, err := fetchBrowserPageContent(ctx, p.URL, p.Timeout, extra)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_navigate: %w", err), false, "")
	}
	result := formatBrowserHeader(utils.ScrubURL(finalURL), title) + text
	return &hwcloud.ToolResult{Content: utils.WrapUntrusted(result)}
}

// ── browser_screenshot ──

// browserScreenshotTool captures a full-page screenshot as base64 PNG.
type browserScreenshotTool struct{}

type browserScreenshotParams struct {
	URL          string `json:"url" jsonschema:"description=URL to navigate to before taking the screenshot"`
	WaitSelector string `json:"wait_selector,omitempty" jsonschema:"description=Optional CSS selector to wait for before capturing"`
	Timeout      int    `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds (default 60, max 120)"`
}

func (t *browserScreenshotTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "browser_screenshot",
		Description: "Navigate to a URL using a real headless browser and capture a full-page screenshot returned as a base64-encoded PNG. " +
			"Useful for visually inspecting page layout or verifying dynamic UI that text extraction cannot capture.",
		Parameters: hwcloud.SchemaOf[browserScreenshotParams](),
	}
}

func (t *browserScreenshotTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[browserScreenshotParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_screenshot: %w", err), false, "")
	}
	if _, err := utils.ValidateRequestURL(p.URL); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_screenshot: %w", err), false, "")
	}

	bCtx, cancel, berr := newBrowserCtx(ctx, p.Timeout)
	if berr != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_screenshot: %w", berr), false, "")
	}
	defer cancel()

	var buf []byte
	acts := []chromedp.Action{
		chromedp.Navigate(p.URL),
	}
	if ws := strings.TrimSpace(p.WaitSelector); ws != "" {
		acts = append(acts, chromedp.WaitVisible(ws, chromedp.ByQuery))
	} else {
		acts = append(acts, chromedp.WaitReady("body", chromedp.ByQuery), chromedp.Sleep(browserFetchWait))
	}
	acts = append(acts, chromedp.FullScreenshot(&buf, 90))

	if err := chromedp.Run(bCtx, acts...); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_screenshot: %w", err), false, "")
	}
	if len(buf) == 0 {
		return hwcloud.ErrorResult(fmt.Errorf("browser_screenshot: captured an empty image for %s", utils.ScrubURL(p.URL)), false, "")
	}
	encoded := base64.StdEncoding.EncodeToString(buf)
	result := fmt.Sprintf("URL: %s\nScreenshot (base64 PNG, %d bytes):\n%s", utils.ScrubURL(p.URL), len(buf), encoded)
	return &hwcloud.ToolResult{Content: result}
}

// ── browser_evaluate ──

// browserEvaluateTool evaluates a JS expression in the page context.
type browserEvaluateTool struct{}

type browserEvaluateParams struct {
	URL          string `json:"url" jsonschema:"description=URL to navigate to before evaluating the expression"`
	Expression   string `json:"expression" jsonschema:"description=JavaScript expression to evaluate in the page context. The result is converted to a string."`
	WaitSelector string `json:"wait_selector,omitempty" jsonschema:"description=Optional CSS selector to wait for before evaluating"`
	Timeout      int    `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds (default 60, max 120)"`
}

func (t *browserEvaluateTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "browser_evaluate",
		Description: "Navigate to a URL using a real headless browser, then evaluate a JavaScript expression in the page context and return the result as a string. " +
			"Useful for extracting dynamic data, reading DOM properties, or triggering page actions that text extraction cannot reach.",
		Parameters: hwcloud.SchemaOf[browserEvaluateParams](),
	}
}

func (t *browserEvaluateTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[browserEvaluateParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_evaluate: %w", err), false, "")
	}
	if _, err := utils.ValidateRequestURL(p.URL); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_evaluate: %w", err), false, "")
	}

	bCtx, cancel, berr := newBrowserCtx(ctx, p.Timeout)
	if berr != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_evaluate: %w", berr), false, "")
	}
	defer cancel()

	acts := []chromedp.Action{chromedp.Navigate(p.URL)}
	if ws := strings.TrimSpace(p.WaitSelector); ws != "" {
		acts = append(acts, chromedp.WaitVisible(ws, chromedp.ByQuery))
	} else {
		acts = append(acts, chromedp.WaitReady("body", chromedp.ByQuery), chromedp.Sleep(browserFetchWait))
	}
	var evalResult any
	acts = append(acts, chromedp.Evaluate(p.Expression, &evalResult))

	if err := chromedp.Run(bCtx, acts...); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_evaluate: %w", err), false, "")
	}
	result := fmt.Sprintf("URL: %s\nExpression: %s\nResult: %v", utils.ScrubURL(p.URL), p.Expression, evalResult)
	return &hwcloud.ToolResult{Content: result}
}

// ── browser_click ──

// browserClickTool navigates, clicks a CSS selector, and returns the result
// page text.
type browserClickTool struct{}

type browserClickParams struct {
	URL          string `json:"url" jsonschema:"description=URL to navigate to before clicking"`
	Selector     string `json:"selector" jsonschema:"description=CSS selector of the element to click (e.g. #submit-btn, .nav-link, button[type=submit])"`
	WaitSelector string `json:"wait_selector,omitempty" jsonschema:"description=Optional CSS selector to wait for after the click before extracting content"`
	Timeout      int    `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds (default 60, max 120)"`
}

func (t *browserClickTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "browser_click",
		Description: "Navigate to a URL using a real headless browser, click an element identified by a CSS selector, wait for the page to update, and return the resulting page text. " +
			"Useful for interacting with buttons, links, tabs, or other clickable UI that requires JavaScript execution.",
		Parameters: hwcloud.SchemaOf[browserClickParams](),
	}
}

func (t *browserClickTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[browserClickParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_click: %w", err), false, "")
	}
	if _, err := utils.ValidateRequestURL(p.URL); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_click: %w", err), false, "")
	}

	bCtx, cancel, berr := newBrowserCtx(ctx, p.Timeout)
	if berr != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_click: %w", berr), false, "")
	}
	defer cancel()

	acts := []chromedp.Action{
		chromedp.Navigate(p.URL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Click(p.Selector, chromedp.ByQuery),
	}
	if ws := strings.TrimSpace(p.WaitSelector); ws != "" {
		acts = append(acts, chromedp.WaitVisible(ws, chromedp.ByQuery))
	} else {
		acts = append(acts, chromedp.Sleep(browserFetchWait))
	}
	var outerHTML, title, loc string
	acts = append(acts,
		chromedp.Title(&title),
		chromedp.OuterHTML("html", &outerHTML),
		chromedp.Location(&loc),
	)

	if err := chromedp.Run(bCtx, acts...); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("browser_click: %w", err), false, "")
	}
	text, _ := extractBrowserText(outerHTML)
	finalURL := loc
	if finalURL == "" {
		finalURL = p.URL
	}
	result := fmt.Sprintf("URL: %s\nClicked: %s\nTitle: %s\n\n%s", utils.ScrubURL(finalURL), p.Selector, title, text)
	return &hwcloud.ToolResult{Content: utils.WrapUntrusted(result)}
}

// NewBrowserTools returns the four one-shot headless browser tools.
func NewBrowserTools() []hwcloud.Tool {
	return []hwcloud.Tool{
		&browserNavigateTool{},
		&browserScreenshotTool{},
		&browserEvaluateTool{},
		&browserClickTool{},
	}
}
