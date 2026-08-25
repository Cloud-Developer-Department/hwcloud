package tool

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/chromedp/chromedp"
	"golang.org/x/net/html"
)

// extractBrowserText parses raw HTML and returns its visible text + title,
// reusing the same htmlNodeToText / extractHTMLTitle helpers WebFetch uses so
// browser and HTTP-fetched pages share one extraction path (no drift in what
// "visible text" means across tools). Output is capped to browserMaxContentLen.
func extractBrowserText(rawHTML string) (text, title string) {
	doc, err := html.Parse(bytes.NewReader([]byte(rawHTML)))
	if err != nil {
		// html.Parse rarely fails on truncated input (chromedp may cut at
		// a timeout). Fall back to the raw string rather than dropping all
		// content the model could use.
		return truncateBrowserText(rawHTML), ""
	}
	title = extractHTMLTitle(doc)
	text = htmlNodeToText(doc)
	return truncateBrowserText(text), title
}

// truncateBrowserText caps text to browserMaxContentLen runes with a truncation
// marker, mirroring WebFetch's rune-based truncation to avoid splitting a
// multi-byte UTF-8 sequence.
func truncateBrowserText(text string) string {
	if len([]rune(text)) <= browserMaxContentLen {
		return text
	}
	return string([]rune(text)[:browserMaxContentLen]) +
		fmt.Sprintf("\n\n[Content truncated at %d characters]", browserMaxContentLen)
}

// fetchBrowserPageContent navigates a one-shot headless Chrome to rawURL,
// waits for the body to render, and returns (visibleText, title, finalURL,
// error). finalURL is the URL after redirects so the caller can report where
// the content actually came from.
//
// This is the shared fetch path for the one-shot browser tools (navigate,
// click). browser_use_* tools use the persistent session manager instead.
func fetchBrowserPageContent(ctx context.Context, rawURL string, timeoutSecs int, actions ...chromedp.Action) (text, title, finalURL string, err error) {
	bCtx, cancel, err := newBrowserCtx(ctx, timeoutSecs)
	if err != nil {
		return "", "", "", err
	}
	defer cancel()

	var outerHTML, t, loc string
	acts := []chromedp.Action{
		chromedp.Navigate(rawURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
	}
	acts = append(acts, actions...)
	acts = append(acts,
		chromedp.Title(&t),
		chromedp.OuterHTML("html", &outerHTML),
		// Location captures the post-redirect URL into loc.
		chromedp.Location(&loc),
	)

	if err = chromedp.Run(bCtx, acts...); err != nil {
		return "", "", "", err
	}

	text, title = extractBrowserText(outerHTML)
	finalURL = loc
	if finalURL == "" {
		finalURL = rawURL
	}
	return text, title, finalURL, nil
}

// formatBrowserHeader builds the URL/Title header prepended to page text,
// mirroring WebFetch's source header so the model can identify/cite the source
// without re-deriving it from the body.
func formatBrowserHeader(url, title string) string {
	var b strings.Builder
	b.WriteString("URL: ")
	b.WriteString(url)
	b.WriteByte('\n')
	if title != "" {
		b.WriteString("Title: ")
		b.WriteString(title)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}
