#!/usr/bin/env bash
# build.sh — convenience build for hwcloud's two binaries.
#
# Produces <name> (Go CLI/server) and <name>-tui (Rust TUI client). The Go
# binary is built with -tags embed (built-in skills bundled) and -s -w
# (stripped, smaller binary). The Go binary's identity is set via ldflags
# (version.Name); the Rust TUI bakes <name> in as its *default* spawn target
# via HWCLOUD_BINARY_NAME, then launches `<name> serve --acp` unless
# --backend overrides it.
#
# The two binaries do NOT have to be built together: the TUI accepts
# `--backend "<command>"` at runtime, so a default `hwcloud-tui` can
# target any ACP backend without a rebuild. This script just produces a
# matched pair with the right defaults baked in, which is convenient for
# releases and branded installs.
#
# Usage:
#   ./build.sh                         # name=hwcloud (default)
#   HWCLOUD_BINARY_NAME=myagent ./build.sh
#
# Requires: Go (any recent) + Rust ≥ 1.88 (rustup) for the TUI.
set -euo pipefail

NAME="${HWCLOUD_BINARY_NAME:-hwcloud}"
VERSION="${HWCLOUD_VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$REPO_ROOT"

echo "==> Building Go binary: $NAME (version $VERSION, embedded skills)"
# -tags embed: bundle built-in skills (skills/builtin/*) into the binary.
# -s -w: strip symbol table and DWARF debug info for a smaller binary.
go build -tags embed \
         -ldflags "-s -w \
                   -X github.com/Cloud-Developer-Department/hwcloud/version.Name=$NAME \
                   -X github.com/Cloud-Developer-Department/hwcloud/version.Version=$VERSION" \
         -o "$NAME" ./cmd/cli/

echo "==> Building Rust TUI binary: $NAME-tui"
HWCLOUD_BINARY_NAME="$NAME" cargo build --release --manifest-path tui/Cargo.toml

# cargo produces a binary named after the crate (hwcloud-tui); rename to
# match the build identity so the pair is <name> + <name>-tui.
cp -f "tui/target/release/hwcloud-tui" "$NAME-tui"

echo ""
echo "Built:"
echo "  $NAME        (Go: serve/run/keyring)"
echo "  $NAME-tui    (Rust: spawns '$NAME serve --acp' by default; --backend overrides)"
echo ""
echo "Put both on PATH, then run: $NAME-tui"
