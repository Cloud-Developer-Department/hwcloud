package tool

import (
	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// ArtifactRoot returns the platform-appropriate artifact directory.
// Linux/macOS: /tmp/<version.Name> (default /tmp/hwcloud)
// Windows:     %TEMP%\<version.Name>
//
// Delegates to hwcloud.ArtifactRoot so there is a single source of truth
// for the tmp root (the hwcloud package owns the version.Name sanitization).
// Tool results exceeding a size threshold can be saved here by hooks and
// referenced in the tool result summary. The system tmp cleaner reclaims
// the space eventually, so artifacts are best-effort persistent.
func ArtifactRoot() string {
	return hwcloud.ArtifactRoot()
}
