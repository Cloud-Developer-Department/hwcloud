package iac

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeProviderMirrorConfig generates a Terraform CLI configuration file
// (.terraformrc) that configures provider installation mirrors.
//
// Each entry in mirrors is classified by prefix:
//   - "http://" or "https://" → network_mirror
//   - everything else        → filesystem_mirror
//
// Terraform tries mirrors in order and falls back to the official registry
// via a final direct { exclude = [...] } block.
//
// The file is written to <workDir>/.terraformrc and its path is returned.
// If mirrors is empty, no file is written and an empty string is returned.
func writeProviderMirrorConfig(workDir string, mirrors []string) (string, error) {
	if len(mirrors) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("provider_installation {\n")

	for _, m := range mirrors {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if isURL(m) {
			b.WriteString(fmt.Sprintf("  network_mirror {\n    url = %q\n    include = [\"registry.terraform.io/*/*\"]\n  }\n", m))
		} else {
			b.WriteString(fmt.Sprintf("  filesystem_mirror {\n    path = %q\n    include = [\"registry.terraform.io/*/*\"]\n  }\n", m))
		}
	}

	// Final fallback: official registry for anything not found in mirrors.
	// Only huaweicloud is excluded — the default mirror covers just that
	// provider, so auxiliary providers (random, tls, ...) must still reach
	// the official registry.
	b.WriteString("  direct {\n    exclude = [\"registry.terraform.io/huaweicloud/*\"]\n  }\n")
	b.WriteString("}\n")

	path := filepath.Join(workDir, ".terraformrc")
	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		return "", fmt.Errorf("write terraformrc: %w", err)
	}
	return path, nil
}

// isURL returns true if the string starts with http:// or https://.
func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
