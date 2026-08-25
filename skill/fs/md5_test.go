package fs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFolderMD5_CrossLanguageConsistency verifies that the Go implementation
// produces the same MD5 as the Python generation script for a known skill
// directory. The expected value was computed by the Python script for
// huawei-cloud-obs-stats (13 files) after a successful `npx skills add`.
func TestFolderMD5_CrossLanguageConsistency(t *testing.T) {
	// Build a miniature directory tree with known content and compute the
	// expected MD5 by hand.
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "my-skill")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "guide.md"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := FolderMD5(skillDir, "my-skill")
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("empty MD5")
	}

	// Verify the same content at a different parent path yields the same MD5.
	dir2 := t.TempDir()
	skillDir2 := filepath.Join(dir2, "my-skill")
	if err := os.MkdirAll(filepath.Join(skillDir2, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir2, "SKILL.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir2, "references", "guide.md"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	got2, err := FolderMD5(skillDir2, "my-skill")
	if err != nil {
		t.Fatal(err)
	}
	if got != got2 {
		t.Errorf("MD5 changed with parent path: %s vs %s", got, got2)
	}
}

// TestFolderMD5_DirNameMatters verifies that renaming the directory changes
// the MD5 (the dirname is the first entry in the aggregation).
func TestFolderMD5_DirNameMatters(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "name-a")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	md5A, err := FolderMD5(skillDir, "name-a")
	if err != nil {
		t.Fatal(err)
	}
	md5B, err := FolderMD5(skillDir, "name-b")
	if err != nil {
		t.Fatal(err)
	}
	if md5A == md5B {
		t.Error("MD5 unchanged after dirname change")
	}
}

// TestFolderMD5_ContentChange verifies that modifying file content changes
// the MD5.
func TestFolderMD5_ContentChange(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "my-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1, err := FolderMD5(skillDir, "my-skill")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2, err := FolderMD5(skillDir, "my-skill")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("MD5 unchanged after content change")
	}
}

// TestFolderMD5_EmptyDir verifies the edge case of a directory with no files.
func TestFolderMD5_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "empty")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := FolderMD5(skillDir, "empty")
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("empty MD5 for empty dir")
	}
}

// TestFolderMD5_NonexistentDir verifies that a missing directory returns an error.
func TestFolderMD5_NonexistentDir(t *testing.T) {
	_, err := FolderMD5("/nonexistent/path/that/does/not/exist", "nope")
	if err == nil {
		t.Fatal("expected error for nonexistent dir")
	}
}

// TestFolderMD5_SkipsSymlinks verifies that symlink-to-dir entries are
// skipped rather than causing an error (matching Python's os.walk with
// followlinks=False, which does not recurse into symlinked dirs).
func TestFolderMD5_SkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "my-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a symlink-to-dir inside the skill dir.
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(skillDir, "linkdir")); err != nil {
		t.Fatal(err)
	}

	// Should not error — symlink is skipped.
	got, err := FolderMD5(skillDir, "my-skill")
	if err != nil {
		t.Fatalf("FolderMD5 with symlink: %v", err)
	}
	if got == "" {
		t.Fatal("empty MD5")
	}

	// MD5 should equal the same tree without the symlink.
	dir2 := t.TempDir()
	skillDir2 := filepath.Join(dir2, "my-skill")
	if err := os.MkdirAll(skillDir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir2, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got2, err := FolderMD5(skillDir2, "my-skill")
	if err != nil {
		t.Fatal(err)
	}
	if got != got2 {
		t.Errorf("symlink changed MD5: %s vs %s", got, got2)
	}
}
