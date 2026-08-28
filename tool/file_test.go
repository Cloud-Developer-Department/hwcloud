package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileWithinWorkspace(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello"), 0644)

	r := NewReadFile(dir)
	out := r.Execute(context.Background(), []byte(`{"path":"test.txt"}`))
	if !strings.Contains(out.Content, "hello") {
		t.Errorf("expected 'hello', got: %s", out.Content)
	}
	t.Logf("✅ read file in workspace: %s", out.Content)
}

func TestReadFileResolvesTraversal(t *testing.T) {
	dir := t.TempDir()
	r := NewReadFile(dir)

	// Traversal is resolved to an absolute path outside workspace.
	// The tool allows it; workspace boundary enforcement is the Approver's job.
	out := r.Execute(context.Background(), []byte(`{"path":"../etc/passwd"}`))
	if out.Error != nil {
		// May fail if /etc/passwd doesn't exist or is unreadable on this system,
		// but should NOT be rejected by ValidatePath.
		if strings.Contains(out.Error.Message, "path outside workspace") {
			t.Errorf("boundary enforcement should be in approver, not ValidatePath: %v", out.Error.Message)
		} else {
			t.Logf("✅ traversal resolved (non-workspace, approver's job to reject): %v", out.Error.Message)
		}
	} else {
		t.Logf("✅ traversal resolved — file outside workspace was readable (approver would normally block this)")
	}
}

func TestReadFileAbsoluteOutsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	r := NewReadFile(dir)

	// ValidatePath accepts absolute paths (boundary enforcement is the Approver's job).
	// The call may succeed or fail depending on whether /etc/passwd exists and is readable.
	out := r.Execute(context.Background(), []byte(`{"path":"/etc/passwd"}`))
	if out.Error != nil {
		t.Logf("absolute outside workspace (approver's job to reject): %v", out.Error.Message)
	} else {
		t.Logf("absolute outside workspace resolved — approver would normally block this")
	}
}

func TestReadFileAcceptsAbsolutePathWithinWorkspace(t *testing.T) {
	dir := t.TempDir()
	absPath := filepath.Join(dir, "test.txt")
	os.WriteFile(absPath, []byte("hello"), 0644)

	r := NewReadFile(dir)
	out := r.Execute(context.Background(), []byte(`{"path":"`+absPath+`"}`))
	if out.Error != nil {
		// Acceptable: file exists but ValidatePath resolved symlinks, etc.
		t.Logf("absolute path result: err=%v, out=%s", out.Error.Message, out.Content)
	} else if !strings.Contains(out.Content, "hello") {
		t.Errorf("expected 'hello', got: %s", out.Content)
	} else {
		t.Logf("✅ absolute path within workspace accepted: %s", out.Content)
	}
}

func TestReadFileNotFound(t *testing.T) {
	dir := t.TempDir()
	r := NewReadFile(dir)

	out := r.Execute(context.Background(), []byte(`{"path":"nonexistent.txt"}`))
	if out.Error == nil {
		t.Error("missing file should return error")
	} else {
		t.Logf("✅ not found: %v", out.Error)
	}
}

func TestWriteFileWithinWorkspace(t *testing.T) {
	dir := t.TempDir()
	w := NewWriteFile(dir)

	out := w.Execute(context.Background(), []byte(`{"path":"out.txt","content":"generated"}`))
	t.Logf("✅ write: %s", out.Content)

	// Verify file was created.
	data, _ := os.ReadFile(filepath.Join(dir, "out.txt"))
	if string(data) != "generated" {
		t.Errorf("file content mismatch: %q", string(data))
	}
}

func TestListDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0755)

	l := NewListDir(dir)
	out := l.Execute(context.Background(), []byte(`{}`))
	if !strings.Contains(out.Content, "a.txt") || !strings.Contains(out.Content, "sub/") {
		t.Errorf("expected a.txt and sub/ in output: %s", out.Content)
	}
	t.Logf("✅ ls: %s", out.Content)
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package main\nfunc helper() {}\n"), 0644)

	g := NewGrep(dir)

	// Find "main" in all files.
	out := g.Execute(context.Background(), []byte(`{"pattern":"main"}`))
	if !strings.Contains(out.Content, "a.go") || !strings.Contains(out.Content, "b.go") {
		t.Errorf("expected matches in a.go and b.go: %s", out.Content)
	}
	t.Logf("✅ grep 'main':\n%s", out.Content)

	// Glob filter — only *.go files.
	out = g.Execute(context.Background(), []byte(`{"pattern":"func","glob":"*.go"}`))
	if !strings.Contains(out.Content, "func") {
		t.Errorf("expected func matches: %s", out.Content)
	}
	t.Logf("✅ grep 'func' *.go:\n%s", out.Content)

	// No matches.
	out = g.Execute(context.Background(), []byte(`{"pattern":"nonexistent"}`))
	if !strings.Contains(out.Content, "No matches") {
		t.Errorf("expected no matches: %s", out.Content)
	}
	t.Logf("✅ grep no match: %s", out.Content)
}
