package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// ValidatePath resolves p against workDir into a safe absolute path.
// Accepts both relative paths (joined with workDir) and absolute paths.
// Resolves symlinks but does NOT enforce workspace boundaries —
// that policy belongs to the sandbox and the governance policy chain.
func ValidatePath(workDir, p string) (string, error) {
	var abs string
	var err error
	if filepath.IsAbs(p) {
		abs = p
	} else {
		abs = filepath.Join(workDir, p)
	}
	abs, err = filepath.Abs(abs)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	// Resolve symlinks to prevent /workspace/link → /etc escapes.
	real, err := filepath.EvalSymlinks(abs)
	if err == nil {
		abs = real
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	return abs, nil
}

// ── ReadFile ──

// ReadFile reads a file from the sandbox workspace.
type ReadFile struct {
	workDir string
}

func NewReadFile(workDir string) *ReadFile {
	abs, _ := filepath.Abs(workDir)
	return &ReadFile{workDir: abs}
}

func (t *ReadFile) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "read",
		Description: "Read a file from the given path. Use line+limit to read a specific line range — combine with grep to locate a line number first, then read the surrounding context.",
		Parameters:  hwcloud.SchemaOf[ReadFileParams](),
	}
}

func (t *ReadFile) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[ReadFileParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("read: %w", err), false, "")
	}
	if params.Line < 0 {
		params.Line = 0
	}
	if params.Line == 0 {
		params.Line = 1
	}

	abs, err := ValidatePath(t.workDir, params.Path)
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return hwcloud.ErrorResult(fmt.Errorf("read: file not found: %s", params.Path), false, "")
		}
		return hwcloud.ErrorResult(fmt.Errorf("read: %w", err), false, "")
	}

	file, err := os.Open(abs)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("read: %w", err), false, "")
	}
	defer file.Close()

	// Binary detection: peek first 512 bytes for null bytes.
	peek := make([]byte, 512)
	n, _ := file.Read(peek)
	if n > 0 && isBinary(peek[:n]) {
		return &hwcloud.ToolResult{Content: fmt.Sprintf("[binary file: %s, %d bytes, type: %s]",
			params.Path, info.Size(), detectType(peek[:n]))}
	}
	// Rewind to beginning.
	file.Seek(0, 0)

	var (
		out       strings.Builder
		lineNum   int
		lineCount int
		hitOffset bool
	)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1 MB max line

	for scanner.Scan() {
		lineNum++
		if lineNum < params.Line {
			continue
		}
		hitOffset = true
		out.WriteString(scanner.Text())
		out.WriteByte('\n')
		lineCount++
		if params.Limit > 0 && lineCount >= params.Limit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("read: %w", err), false, "")
	}

	if !hitOffset {
		return &hwcloud.ToolResult{Content: fmt.Sprintf("[line %d is beyond end of file (%d lines)]", params.Line, lineNum)}
	}

	result := out.String()
	if params.Line > 1 || params.Limit > 0 {
		prefix := fmt.Sprintf("[lines %d-%d, %d total, %d bytes]:\n",
			params.Line, params.Line+lineCount-1, lineNum, info.Size())
		result = prefix + result
	}
	return &hwcloud.ToolResult{Content: result}
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 512 {
		n = 512
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

func detectType(data []byte) string {
	n := len(data)
	if n > 64 {
		n = 64
	}
	for _, b := range data[:n] {
		if b < 9 || (b > 13 && b < 32) && b != 27 {
			return "binary data"
		}
	}
	if n >= 4 && data[0] == 0x7f && data[1] == 'E' && data[2] == 'L' && data[3] == 'F' {
		return "ELF executable"
	}
	return "unknown binary"
}

// ── WriteFile ──

// WriteFile writes content to a file in the sandbox workspace.
type WriteFile struct {
	workDir string
}

func NewWriteFile(workDir string) *WriteFile {
	abs, _ := filepath.Abs(workDir)
	return &WriteFile{workDir: abs}
}

func (t *WriteFile) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "write",
		Description: "Write content to a file. Creates parent directories as needed.",
		Parameters:  hwcloud.SchemaOf[WriteFileParams](),
	}
}

func (t *WriteFile) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[WriteFileParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("write: %w", err), false, "")
	}

	const maxSize = 10 * 1024 * 1024 // 10MB
	if len(params.Content) > maxSize {
		return hwcloud.ErrorResult(fmt.Errorf("write: content too large (%d bytes, max %d)", len(params.Content), maxSize), false, "")
	}

	abs, err := ValidatePath(t.workDir, params.Path)
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("write: %w", err), false, "")
	}
	if err := writeFilePreservingMode(abs, []byte(params.Content)); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("write: %w", err), false, "")
	}

	info, _ := os.Stat(abs)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}
	return &hwcloud.ToolResult{Content: fmt.Sprintf("Wrote %s (%d bytes)", params.Path, size)}
}

// ── ListDir ──

// ListDir lists directory contents in the sandbox workspace.
type ListDir struct {
	workDir string
}

func NewListDir(workDir string) *ListDir {
	abs, _ := filepath.Abs(workDir)
	return &ListDir{workDir: abs}
}

func (t *ListDir) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "ls",
		Description: "List files and directories at the given path.",
		Parameters:  hwcloud.SchemaOf[ListDirParams](),
	}
}

func (t *ListDir) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[ListDirParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("ls: %w", err), false, "")
	}

	dir, err := ValidatePath(t.workDir, params.Path)
	if err != nil {
		// Empty path defaults to workspace root.
		if params.Path == "" {
			dir = t.workDir
		} else {
			return hwcloud.ErrorResult(err, false, "")
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("ls: %w", err), false, "")
	}

	type fileEntry struct {
		Name  string
		Size  int64
		IsDir bool
	}

	var files []fileEntry
	for _, e := range entries {
		info, err := e.Info()
		size := int64(0)
		if err == nil {
			size = info.Size()
		}
		files = append(files, fileEntry{e.Name(), size, e.IsDir()})
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})

	var b strings.Builder
	if params.Path != "" {
		b.WriteString(params.Path + ":\n")
	}
	for _, f := range files {
		if f.IsDir {
			b.WriteString(fmt.Sprintf("  %s/\n", f.Name))
		} else {
			b.WriteString(fmt.Sprintf("  %s  (%d)\n", f.Name, f.Size))
		}
	}
	if len(files) == 0 {
		b.WriteString("  (empty)\n")
	}
	return &hwcloud.ToolResult{Content: b.String()}
}

// ── EditFile ──

// EditFile performs exact string replacement in a file.
type EditFile struct {
	workDir string
}

func NewEditFile(workDir string) *EditFile {
	abs, _ := filepath.Abs(workDir)
	return &EditFile{workDir: abs}
}

func (t *EditFile) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "edit",
		Description: "Replace a string in a file. Finds old_text and replaces it with new_text. When replace_all is false (default), only the first match is replaced. Returns an error when old_text is not unique — use replace_all or make old_text more specific.",
		Parameters:  hwcloud.SchemaOf[EditFileParams](),
	}
}

func (t *EditFile) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[EditFileParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("edit: %w", err), false, "")
	}

	abs, err := ValidatePath(t.workDir, params.Path)
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return hwcloud.ErrorResult(fmt.Errorf("edit: file not found: %s", params.Path), false, "")
		}
		return hwcloud.ErrorResult(fmt.Errorf("edit: %w", err), false, "")
	}

	content := string(data)
	count := strings.Count(content, params.OldText)
	if count == 0 {
		return hwcloud.ErrorResult(fmt.Errorf("edit: old_text not found in %s", params.Path), false, "")
	}
	if !params.ReplaceAll && count > 1 {
		return hwcloud.ErrorResult(fmt.Errorf("edit: old_text found %d times in %s — set replace_all to true or make old_text more specific", count, params.Path), false, "")
	}

	n := 1
	if params.ReplaceAll {
		n = count
	}
	newContent := strings.Replace(content, params.OldText, params.NewText, n)
	if err := writeFilePreservingMode(abs, []byte(newContent)); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("edit: %w", err), false, "")
	}

	if params.ReplaceAll {
		return &hwcloud.ToolResult{Content: fmt.Sprintf("Replaced %d occurrences in %s", count, params.Path)}
	}
	return &hwcloud.ToolResult{Content: fmt.Sprintf("Replaced in %s", params.Path)}
}

// writeFilePreservingMode writes content, keeping the target's existing
// mode (Claude Code/Codex convention) — a fixed 0644 would strip
// executable bits and 0600 permissions on edited files. New files use
// 0644.
func writeFilePreservingMode(path string, content []byte) error {
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode()
	}
	return os.WriteFile(path, content, mode)
}

// ReadFileParams are the arguments to read.
type ReadFileParams struct {
	Path  string `json:"path" jsonschema:"description=File path"`
	Line  int    `json:"line,omitempty" jsonschema:"description=Start line (1-based, default: 1). Use with limit to read a specific range."`
	Limit int    `json:"limit,omitempty" jsonschema:"description=Max lines to read (default: all remaining). Use with line to read a window around a grep hit."`
}

// WriteFileParams are the arguments to write.
type WriteFileParams struct {
	Path    string `json:"path" jsonschema:"description=File path"`
	Content string `json:"content" jsonschema:"description=Content to write to the file"`
}

// ListDirParams are the arguments to ls. Path is optional — empty lists
// the workspace root.
type ListDirParams struct {
	Path string `json:"path,omitempty" jsonschema:"description=Directory path (default: workspace root)"`
}

// EditFileParams are the arguments to edit.
type EditFileParams struct {
	Path       string `json:"path" jsonschema:"description=File path"`
	OldText    string `json:"old_text" jsonschema:"description=Text to find and replace"`
	NewText    string `json:"new_text" jsonschema:"description=Replacement text"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"description=Replace all occurrences (default: false)"`
}
