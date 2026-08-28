package tool

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/Cloud-Developer-Department/hwcloud/version"
)

// Chrome-for-Testing (CfT) auto-download.
//
// On first use the browser tools need a Chrome binary. If no system Chrome is
// found at standard install paths, this module downloads a pinned
// Chrome-for-Testing build into the user cache dir and reuses it thereafter.
// This avoids requiring the operator to install Chrome manually on a
// headless server (the primary deployment target).

const (
	// cftVersionURL is the JSON endpoint listing the last-known-good
	// Chrome-for-Testing versions per platform.
	cftVersionURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions.json"
	// cftDownloadRetries bounds download attempts; a flaky mirror should
	// not kill the whole tool.
	cftDownloadRetries = 3
)

// cftCacheDirName is the subdirectory under the OS cache dir, named after
// the binary identity (version.Name) so different builds isolate their
// Chrome caches.
var cftCacheDirName = version.Name + string(filepath.Separator) + "chrome-for-testing"

// cftVersionResponse models the relevant slice of the last-known-good JSON.
type cftVersionResponse struct {
	Channels struct {
		Stable struct {
			Version string `json:"version"`
			Downloads struct {
				Chrome []struct {
					Platform string `json:"platform"`
					URL      string `json:"url"`
				} `json:"chrome"`
			} `json:"downloads"`
		} `json:"stable"`
	} `json:"channels"`
}

// cftPlatform maps GOOS/GOARCH to the Chrome-for-Testing platform string.
// Returns "" for unsupported platforms.
func cftPlatform() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "linux64"
	case "darwin/arm64":
		return "mac-arm64"
	case "darwin/amd64":
		return "mac-x64"
	case "windows/amd64":
		return "win64"
	case "windows/386":
		return "win32"
	}
	return ""
}

// cftExecutableRelPath is the path inside the extracted CfT archive to the
// Chrome binary, relative to the archive root. The internal directory name
// varies by platform+arch (chrome-linux64, chrome-mac-arm64, chrome-mac-x64,
// chrome-win64).
func cftExecutableRelPath() string {
	switch runtime.GOOS {
	case "darwin":
		dir := "chrome-mac-arm64"
		if runtime.GOARCH == "amd64" {
			dir = "chrome-mac-x64"
		}
		return filepath.Join(dir, "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing")
	case "linux":
		return filepath.Join("chrome-linux64", "chrome")
	case "windows":
		return filepath.Join("chrome-win64", "chrome.exe")
	}
	return "chrome"
}

// resolveChromePath returns the Chrome binary to use for chromedp.
//
// Resolution order:
//  1. HWCLOUD_CHROME_PATH env var (operator override).
//  2. System Chrome at standard install paths (no download needed).
//  3. Chrome-for-Testing downloaded into the cache dir.
//
// The CfT download runs at most once per process (guarded by
// cftResolveOnce); subsequent calls reuse the resolved path.
func resolveChromePath(ctx context.Context) (string, error) {
	// 1. Operator override.
	if p := strings.TrimSpace(os.Getenv("HWCLOUD_CHROME_PATH")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	// 2. System Chrome.
	if p := findSystemChrome(); p != "" {
		return p, nil
	}

	// 3. CfT download (once per process).
	cftResolveOnce.Do(func() {
		cftResolvedPath, cftResolveErr = downloadChromeForTesting(ctx)
	})
	if cftResolveErr != nil {
		return "", fmt.Errorf("chrome not found and CfT download failed: %w; set HWCLOUD_CHROME_PATH or install Chrome", cftResolveErr)
	}
	return cftResolvedPath, nil
}

var (
	cftResolveOnce  sync.Once
	cftResolvedPath string
	cftResolveErr   error
)

// findSystemChrome searches standard install paths for an existing Chrome or
// Chromium binary. Returns "" if none found.
func findSystemChrome() string {
	var candidates []string
	switch runtime.GOOS {
	case "linux":
		candidates = []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
		}
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		candidates = []string{
			filepath.Join(os.Getenv("ProgramFiles"), "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(os.Getenv("ProgramFiles(x86)"), "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(os.Getenv("LocalAppData"), "Google", "Chrome", "Application", "chrome.exe"),
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// downloadChromeForTesting fetches the last-known-good version JSON, downloads
// the platform zip, and extracts the Chrome binary into the cache dir. Returns
// the absolute path to the extracted binary.
func downloadChromeForTesting(ctx context.Context) (string, error) {
	platform := cftPlatform()
	if platform == "" {
		return "", fmt.Errorf("unsupported platform %s/%s for CfT download", runtime.GOOS, runtime.GOARCH)
	}

	// Fetch the version manifest.
	manifest, err := fetchCFTManifest(ctx)
	if err != nil {
		return "", err
	}

	// Find the download URL for this platform.
	var downloadURL, version string
	version = manifest.Channels.Stable.Version
	for _, d := range manifest.Channels.Stable.Downloads.Chrome {
		if d.Platform == platform {
			downloadURL = d.URL
			break
		}
	}
	if downloadURL == "" {
		return "", fmt.Errorf("no CfT download for platform %q (version %s)", platform, version)
	}

	// Cache dir: <os cache>/hwcloud/chrome-for-testing/<version>
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve cache dir: %w", err)
	}
	versionDir := filepath.Join(cacheDir, cftCacheDirName, version)
	execPath := filepath.Join(versionDir, cftExecutableRelPath())

	// Already downloaded? Reuse.
	if info, err := os.Stat(execPath); err == nil && !info.IsDir() {
		return execPath, nil
	}

	// Download + extract the zip.
	zipPath := filepath.Join(versionDir, "chrome.zip")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	if err := downloadWithRetries(ctx, downloadURL, zipPath); err != nil {
		return "", fmt.Errorf("download CfT zip: %w", err)
	}
	if err := extractZipSafe(zipPath, versionDir); err != nil {
		return "", fmt.Errorf("extract CfT zip: %w", err)
	}
	// Clean up the zip; the extracted tree is the cache.
	_ = os.Remove(zipPath)

	// macOS / Linux: ensure the binary is executable.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(execPath, 0o755); err != nil {
			return "", fmt.Errorf("chmod chrome binary: %w", err)
		}
	}

	if _, err := os.Stat(execPath); err != nil {
		return "", fmt.Errorf("chrome binary not found after extraction at %s: %w", execPath, err)
	}
	return execPath, nil
}

// fetchCFTManifest GETs the last-known-good-versions JSON.
func fetchCFTManifest(ctx context.Context) (*cftVersionResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cftVersionURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch CfT manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CfT manifest HTTP %d", resp.StatusCode)
	}
	var m cftVersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode CfT manifest: %w", err)
	}
	if m.Channels.Stable.Version == "" {
		return nil, fmt.Errorf("CfT manifest has no stable version")
	}
	return &m, nil
}

// downloadWithRetries downloads url to destPath with up to cftDownloadRetries
// attempts. Each attempt streams to a temp file then renames, so a partial
// download never leaves a corrupt destPath.
func downloadWithRetries(ctx context.Context, url, destPath string) error {
	var lastErr error
	for attempt := 0; attempt < cftDownloadRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := downloadFile(ctx, url, destPath); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

// downloadFile streams url to destPath via a temp file + atomic rename.
func downloadFile(ctx context.Context, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, destPath)
}

// extractZipSafe extracts a zip into dest, rejecting entries whose path
// escapes dest (zip-slip protection). Each entry's parent dir is created as
// needed; file permissions are restored from the zip header.
func extractZipSafe(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}

	for _, f := range r.File {
		// zip-slip: the joined path must stay under destAbs.
		target := filepath.Join(destAbs, f.Name)
		if !strings.HasPrefix(target+string(filepath.Separator), destAbs+string(filepath.Separator)) && target != destAbs {
			return fmt.Errorf("zip-slip: entry %q escapes destination", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return err
		}
		rc.Close()
		out.Close()
	}
	return nil
}

// chromeAllocatorOptions builds the chromedp exec-allocator options for the
// resolved Chrome binary, tuned for a headless server. Shared by the one-shot
// browser tools and the persistent browser_use session manager.
//
// headless="new" uses Chrome's new headless mode (full rendering, not the
// legacy --headless=old shell). no-sandbox is required when running as root
// (typical in containers). disable-dev-shm-usage prevents /dev/shm exhaustion
// in containers with a small tmpfs. enable-automation=false +
// disable-blink-features=AutomationControlled reduce bot-detection signals
// (the primary reason WebFetch is blocked by Cloudflare).
func chromeAllocatorOptions(execPath string) []chromedp.ExecAllocatorOption {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(execPath),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("lang", "zh-CN"),
		chromedp.UserAgent(webUserAgent),
		chromedp.WindowSize(1280, 900),
	)
	return opts
}
