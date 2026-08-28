package tool

import (
	"fmt"
	"os"
	"path/filepath"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// resolveOutputPath returns path unchanged if it is absolute. For relative
// paths it resolves them against the user's Documents folder so files created
// by the AI land in a predictable, user-visible location rather than the
// server's working directory.
//
// Resolution order:
//  1. XDG_DOCUMENTS_DIR environment variable (Linux / freedesktop standard)
//  2. ~/Documents as a cross-platform fallback (Windows, macOS, Linux)
//
// Mirrors the reference implementation's ResolveOutputPath.
func resolveOutputPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if xdgDocs := os.Getenv("XDG_DOCUMENTS_DIR"); xdgDocs != "" {
		return filepath.Join(xdgDocs, path)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "Documents", path)
	}
	return path
}

// officeToolText wraps a success string into a ToolResult.
func officeToolText(text string) *hwcloud.ToolResult {
	return &hwcloud.ToolResult{Content: text}
}

// officeToolError wraps an error message into a failed ToolResult.
func officeToolError(toolName, text string) *hwcloud.ToolResult {
	return hwcloud.ErrorResult(fmt.Errorf("%s: %s", toolName, text), false, "")
}

// NewOfficeTools returns the office tools (Word + Excel + PPT). workDir is
// the workspace root used by read tools for relative-path resolution.
func NewOfficeTools(workDir string) []hwcloud.Tool {
	return []hwcloud.Tool{
		&wordReadTool{workDir: workDir},
		&wordWriteTool{},
		&excelReadTool{workDir: workDir},
		&excelWriteTool{},
		&pptxReadTool{workDir: workDir},
		&pptxWriteTool{},
		&pptxTemplateAnalyzeTool{},
		&pptxTemplateFillTool{},
	}
}
