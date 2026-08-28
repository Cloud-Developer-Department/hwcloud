// Package version holds the build-time identity of the binary: the agent
// implementation name and version string. Both are injected via ldflags at
// compile time and fall back to dev defaults in the var initializers so a
// bare `go build` still reports a usable identity.
//
// Typical ldflags injection:
//
//	-X github.com/Cloud-Developer-Department/hwcloud/version.Name=foo \
//	-X github.com/Cloud-Developer-Department/hwcloud/version.Version=v1.2.3
//
// When ldflags are absent (development build), Name defaults to "hwcloud"
// and Version to "0.0.1-beta.<build-timestamp>".
package version

import (
	"strings"
)

// Name is the agent implementation name reported to peers (e.g. in ACP
// initialize agentInfo.name and MCP client identity). Inject via
// -X ...version.Name=<name>; defaults to "hwcloud".
var Name = "hwcloud"

// Version is the build version reported to peers and via `--version`.
// Inject via -X ...version.Version=<ver>.
var Version = "0.0.1-beta.0"

// SafeName returns Name with path separators and NUL replaced by '_', so it
// is safe to use as a single filesystem path segment (e.g. under os.TempDir()).
// Use this anywhere version.Name becomes a path component; the raw Name is
// still used for identity (MCP client name, ACP agentInfo, CLI help, log
// filename) and is NOT sanitized there.
func SafeName() string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == 0 {
			return '_'
		}
		return r
	}, Name)
}

// ConfigDirName returns the per-build config directory name (a dotted,
// path-safe form of Name) used as the leaf under the user home directory —
// e.g. ".hwcloud" by default, ".myagent" when Name is injected as
// "myagent". This is a build-time constant (Name is set via ldflags or its
// var initializer), so each build has a stable, isolated config tree;
// different builds of the same project do not share or clobber each other's
// ~/.<name> state.
func ConfigDirName() string {
	return "." + SafeName()
}
