package iac

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/hc-install/src"
	oaversion "github.com/Cloud-Developer-Department/hwcloud/version"
)

// defaultInstallDir is where downloaded terraform binaries are cached.
func defaultInstallDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, oaversion.ConfigDirName(), "bin")
}

// Detect looks for an existing terraform binary in PATH.
// Returns its full path and version. Returns an error if not found.
func Detect() (string, string, error) {
	path, err := exec.LookPath("terraform")
	if err != nil {
		return "", "", err
	}
	ver, _ := detectVersion(path)
	return path, ver, nil
}

// detectVersion runs `terraform version` and extracts the version number.
func detectVersion(binaryPath string) (string, error) {
	out, err := exec.Command(binaryPath, "version").Output()
	if err != nil {
		return "", err
	}
	// Output: "Terraform v1.9.5\n..."
	s := strings.TrimSpace(string(out))
	if !strings.HasPrefix(s, "Terraform v") {
		return "", fmt.Errorf("unexpected version output: %s", s)
	}
	s = strings.TrimPrefix(s, "Terraform v")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s), nil
}

// Install downloads and installs a specific terraform version.
// It tries each mirror URL in order; if all fail it falls back to the
// official HashiCorp releases via hc-install.
// The binary is placed in destDir and its full path is returned.
//
// If ver is empty, mirrors and the version-specific cache are skipped
// (a cached "terraform" could be any stale version) and hc-install
// fetches the latest release directly.
func Install(ctx context.Context, ver string, mirrors []string, destDir string) (string, error) {
	// Absolute destDir — a relative path (e.g. when $HOME is unset and
	// UserHomeDir returns "") would install the binary relative to the
	// process cwd and fail at exec time with a confusing fork/exec error.
	abs, err := filepath.Abs(destDir)
	if err != nil {
		return "", fmt.Errorf("resolve install dir: %w", err)
	}
	destDir = abs
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("create install dir: %w", err)
	}

	// Empty version: skip cache + mirrors, go straight to hc-install latest.
	if ver == "" {
		return installViaHCInstall(ctx, ver, destDir)
	}

	// Check if already installed.
	binName := binaryName(ver)
	binPath := filepath.Join(destDir, binName)
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	// Try mirrors first.
	for _, mirror := range mirrors {
		path, err := downloadFromMirror(ctx, mirror, ver, destDir)
		if err == nil {
			return path, nil
		}
		// Continue to next mirror on failure.
	}

	// Fall back to hc-install (official HashiCorp releases).
	return installViaHCInstall(ctx, ver, destDir)
}

// downloadFromMirror downloads terraform from a mirror URL.
// Expected URL pattern:
//
//	<mirror>/terraform/<version>/terraform_<version>_<os>_<arch>.zip
func downloadFromMirror(ctx context.Context, mirror, ver, destDir string) (string, error) {
	platform := fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
	zipName := fmt.Sprintf("terraform_%s_%s.zip", ver, platform)
	zipURL := strings.TrimRight(mirror, "/") + "/terraform/" + ver + "/" + zipName

	zipPath := filepath.Join(destDir, zipName)
	if err := downloadFile(ctx, zipURL, zipPath); err != nil {
		return "", fmt.Errorf("download from mirror %s: %w", mirror, err)
	}

	binPath := filepath.Join(destDir, binaryName(ver))
	if err := unzip(zipPath, binPath); err != nil {
		return "", fmt.Errorf("unzip: %w", err)
	}

	os.Remove(zipPath) // clean up zip
	return binPath, nil
}

// installViaHCInstall uses hc-install to download from official HashiCorp releases.
// If ver is empty, it fetches the latest version; otherwise it fetches the exact version.
func installViaHCInstall(ctx context.Context, ver, destDir string) (string, error) {
	i := install.NewInstaller()

	var dir string
	var err error

	if ver == "" {
		// Latest version — use LatestVersion source.
		lv := &releases.LatestVersion{
			Product:    product.Terraform,
			InstallDir: destDir,
		}
		dir, err = i.Ensure(ctx, []src.Source{lv})
		if err != nil {
			return "", fmt.Errorf("hc-install latest: %w", err)
		}
	} else {
		// Exact version — use ExactVersion source.
		v, err := version.NewVersion(ver)
		if err != nil {
			return "", fmt.Errorf("invalid version %q: %w", ver, err)
		}
		ev := &releases.ExactVersion{
			Product:    product.Terraform,
			Version:    v,
			InstallDir: destDir,
		}
		dir, err = i.Ensure(ctx, []src.Source{ev})
		if err != nil {
			return "", fmt.Errorf("hc-install %s: %w", ver, err)
		}
	}

	// hc-install names the binary "terraform"; rename to include version
	// for our caching scheme.
	srcPath := filepath.Join(dir, "terraform")
	if runtime.GOOS == "windows" {
		srcPath = filepath.Join(dir, "terraform.exe")
	}

	if ver == "" {
		return srcPath, nil
	}

	dstPath := filepath.Join(destDir, binaryName(ver))
	if err := os.Rename(srcPath, dstPath); err != nil {
		if err := copyFile(srcPath, dstPath); err != nil {
			return "", fmt.Errorf("rename binary: %w", err)
		}
		os.Remove(srcPath)
	}
	return dstPath, nil
}

// EnsureTerraform ensures a terraform binary is available and returns its path.
// Priority:
//  1. cfg.BinaryPath — use directly if set
//  2. Detect() — use existing binary in PATH
//  3. Install() from cfg.BinaryMirrors — try each in order
//  4. Install() via hc-install — official HashiCorp releases
func EnsureTerraform(ctx context.Context, cfg Config) (string, error) {
	// 1. Explicit path.
	if cfg.BinaryPath != "" {
		if err := validateBinaryPath(cfg.BinaryPath); err != nil {
			return "", fmt.Errorf("binary at %s: %w", cfg.BinaryPath, err)
		}
		return cfg.BinaryPath, nil
	}

	// 2. Detect existing in PATH.
	var pathVersion string
	if path, ver, err := Detect(); err == nil {
		// If a specific version is required, check it matches.
		if cfg.Version == "" || ver == cfg.Version {
			return path, nil
		}
		// Version mismatch — fall through to install, but remember the
		// PATH version so we can include it in the error if install fails.
		pathVersion = ver
	}

	// 3+4. Install.
	destDir := defaultInstallDir()
	installPath, err := Install(ctx, cfg.Version, cfg.BinaryMirrors, destDir)
	if err != nil {
		if pathVersion != "" {
			return "", fmt.Errorf("terraform %s found in PATH but version %s required, and install failed: %w",
				pathVersion, cfg.Version, err)
		}
		return "", err
	}
	if err := validateBinaryPath(installPath); err != nil {
		return "", fmt.Errorf("terraform install at %s: %w", installPath, err)
	}
	return installPath, nil
}

// validateBinaryPath checks that path is a regular file and its parent is a
// directory. This catches the "file where a directory was expected" (or vice
// versa) mismatch that produces a bare OS "not a directory" error at exec
// time, translating it into a clear message before the binary is handed to
// fork/exec.
func validateBinaryPath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("expected a binary file but found a directory — remove it and reinstall")
	}
	parent := filepath.Dir(path)
	pInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("parent dir: %w", err)
	}
	if !pInfo.IsDir() {
		return fmt.Errorf("parent %s is not a directory — path layout is corrupted, remove the install dir and retry", parent)
	}
	return nil
}

// binaryName returns the cached binary filename for a version.
func binaryName(ver string) string {
	name := "terraform"
	if ver != "" {
		name = "terraform-" + ver
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// downloadFile fetches a URL and saves it to dest.
// On any failure (HTTP error, write error, empty body), the partial
// destination file is removed so retries start clean.
func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}

	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(dest) // clean up partial download
		return err
	}
	if n == 0 {
		os.Remove(dest)
		return fmt.Errorf("downloaded file is empty")
	}
	return nil
}

// copyFile copies src to dst and makes dst executable.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// unzip extracts the terraform binary from a zip archive.
// The zip is expected to contain a single "terraform" (or "terraform.exe")
// binary at the root level.
func unzip(zipPath, destPath string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		// Skip directories and non-binary files.
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(f.Name)
		if name != "terraform" && name != "terraform.exe" {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
		if err != nil {
			rc.Close()
			return err
		}

		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
		return nil
	}

	return fmt.Errorf("terraform binary not found in zip archive")
}
