package wasmhost

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"

	skillfs "github.com/Cloud-Developer-Department/hwcloud/skill/fs"
)

// osFS implements FS over the host filesystem with NO sandbox boundary.
// The trust model is explicit: whoever can place a .wasm in the plugin
// directory already has the process's capabilities (keyring, exec,
// env) — fs_* adds file access to the same trust domain. Deployments
// that need a boundary substitute their own FS implementation (chroot,
// container, allowlist) via HostAPI.WithFS.
type osFS struct{}

// NewOSFS returns an unrestricted filesystem-backed FS.
func NewOSFS() FS { return osFS{} }

func (osFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (osFS) WriteFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func (osFS) ReadDir(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

func (osFS) FileMD5(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:]), nil
}

func (osFS) DirectoryMD5(path string) (string, error) {
	return skillfs.FolderMD5(path, filepath.Base(path))
}
