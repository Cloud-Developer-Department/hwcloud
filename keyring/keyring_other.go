//go:build !linux

package keyring

import (
	"errors"

	"github.com/Cloud-Developer-Department/hwcloud/version"
	gkr "github.com/zalando/go-keyring"
)

// openBackend selects a backend on non-Linux platforms (macOS / Windows).
// zalando/go-keyring routes to the native keychain (Keychain / WinCred)
// which does not depend on D-Bus.
func openBackend() (backend, error) {
	if _, err := gkr.Get(version.Name, "__probe__"); err != nil &&
		!errors.Is(err, gkr.ErrNotFound) {
		return nil, err
	}
	return secretServiceBackend{}, nil
}
