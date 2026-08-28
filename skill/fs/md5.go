package fs

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FolderMD5 computes an aggregate MD5 of a directory, matching the algorithm
// used by the Python generation script:
//
//	entries = [dirname]
//	for each level (depth-first, dirs sorted, files sorted):
//	    for each file in sorted(files):
//	        entries = append(entries, "relpath:md5(filebytes)")
//	    recurse into each dir in sorted(dirs)
//	result = md5("\n".join(entries))
//
// Files are visited before directories at each level (matching Python's
// os.walk + dirs.sort() + sorted(files)). The directory name participates
// (renaming the dir changes the MD5) but the parent path does not.
func FolderMD5(dirpath, dirname string) (string, error) {
	entries := []string{dirname}
	if err := walkSorted(dirpath, dirpath, &entries); err != nil {
		return "", err
	}
	combined := strings.Join(entries, "\n")
	sum := md5.Sum([]byte(combined))
	return hex.EncodeToString(sum[:]), nil
}

// walkSorted recursively walks dir, appending "relpath:md5" entries.
// At each level, files are processed first (sorted), then directories
// (sorted) — matching Python's os.walk + dirs.sort() + sorted(files).
//
// Non-regular entries are handled defensively:
//   - Symlinks: skipped (neither read nor recursed). Python's os.walk
//     with followlinks=False lists a symlink-to-dir in `dirs` but does
//     not recurse; Go's os.ReadDir reports it as a non-dir (Lstat).
//     Reading it with os.ReadFile would error ("is a directory") or
//     follow a file-symlink; either way the Python parity breaks.
//   - Named pipes, devices, sockets: skipped to avoid blocking
//     os.ReadFile forever (a FIFO with no writer blocks indefinitely).
func walkSorted(root, dir string, entries *[]string) error {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	sort.Slice(dirEntries, func(i, j int) bool {
		return dirEntries[i].Name() < dirEntries[j].Name()
	})

	var subdirs []os.DirEntry
	for _, e := range dirEntries {
		// Skip symlinks and special files (pipes, devices, sockets).
		// ModeSymlink | ModeNamedPipe | ModeDevice | ModeSocket.
		if e.Type()&(os.ModeSymlink|os.ModeNamedPipe|os.ModeDevice|os.ModeSocket) != 0 {
			continue
		}
		if e.IsDir() {
			subdirs = append(subdirs, e)
			continue
		}
		full := filepath.Join(dir, e.Name())
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		sum := md5.Sum(data)
		*entries = append(*entries, rel+":"+hex.EncodeToString(sum[:]))
	}

	for _, d := range subdirs {
		if err := walkSorted(root, filepath.Join(dir, d.Name()), entries); err != nil {
			return err
		}
	}
	return nil
}
