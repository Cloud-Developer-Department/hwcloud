package tool

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

//go:embed pptx-worker/worker.bundle.mjs
var pptxWorkerBundleFS embed.FS

// ── pptx_write ──

// pptxWriteTool creates a .pptx file by running an LLM-generated JavaScript
// build script (PptxGenJS) in a Node.js subprocess. The worker.bundle.mjs is
// embedded in the binary and extracted to a temp file at runtime — no npm
// install needed, only a Node.js runtime.
//
// If Node.js is absent, the tool returns a clear error so the model can
// install Node via the shell tool and retry.
type pptxWriteTool struct{}

type pptxWriteParams struct {
	Path      string          `json:"path" jsonschema:"description=Output path for the .pptx file. Relative paths resolve to the Documents folder."`
	Script    string          `json:"script" jsonschema:"description=JavaScript module exporting default async function build(pptx, ctx) or a named build function. The script adds slides to the PptxGenJS instance."`
	Data      json.RawMessage `json:"data,omitempty" jsonschema:"description=Optional JSON value passed to ctx.data inside the script"`
	AssetsDir string          `json:"assets_dir,omitempty" jsonschema:"description=Optional base directory for ctx.resolveAsset() and ctx.imageData() calls"`
}

func (t *pptxWriteTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "pptx_write",
		Description: "Create a PowerPoint (.pptx) file from scratch by running a PptxGenJS build script in Node.js. " +
			"The script exports default async function build(pptx, ctx) and adds slides to the provided PptxGenJS instance. " +
			"Requires Node.js (install via shell if missing). " +
			"Use this when creating a deck with no template; when a template is available, prefer pptx_template_fill (no Node needed, preserves design).",
		Parameters: hwcloud.SchemaOf[pptxWriteParams](),
	}
}

func (t *pptxWriteTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[pptxWriteParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("pptx_write: %w", err), false, "")
	}
	if strings.TrimSpace(p.Path) == "" {
		return officeToolError("pptx_write", "missing required parameter: path")
	}
	if strings.TrimSpace(p.Script) == "" {
		return officeToolError("pptx_write", "missing required parameter: script")
	}

	// Resolve Node.js.
	nodePath, err := exec.LookPath("node")
	if err != nil {
		return officeToolError("pptx_write", "Node.js was not found; install Node.js (v18+) to enable PowerPoint generation, then retry")
	}

	// Resolve assets_dir if provided.
	assetsDir := strings.TrimSpace(p.AssetsDir)
	if assetsDir != "" {
		if !filepath.IsAbs(assetsDir) {
			assetsDir, err = filepath.Abs(assetsDir)
			if err != nil {
				return officeToolError("pptx_write", fmt.Sprintf("invalid assets_dir: %s", err.Error()))
			}
		}
		info, err := os.Stat(assetsDir)
		if err != nil {
			return officeToolError("pptx_write", fmt.Sprintf("invalid assets_dir: %s", err.Error()))
		}
		if !info.IsDir() {
			return officeToolError("pptx_write", "invalid assets_dir: must be a directory")
		}
	}

	outputPath := resolveOutputPath(strings.TrimSpace(p.Path))

	// Write the LLM-generated build script to a temp .mjs file.
	scriptFile, err := os.CreateTemp("", "hwcloud-pptx-build-*.mjs")
	if err != nil {
		return officeToolError("pptx_write", fmt.Sprintf("failed to create build script: %s", err.Error()))
	}
	scriptPath := scriptFile.Name()
	defer os.Remove(scriptPath)

	if _, err := scriptFile.WriteString(p.Script); err != nil {
		scriptFile.Close()
		return officeToolError("pptx_write", fmt.Sprintf("failed to write build script: %s", err.Error()))
	}
	if err := scriptFile.Close(); err != nil {
		return officeToolError("pptx_write", fmt.Sprintf("failed to close build script: %s", err.Error()))
	}

	// Extract the embedded worker bundle to a temp file.
	bundlePath, err := extractPptxWorkerBundle()
	if err != nil {
		return officeToolError("pptx_write", fmt.Sprintf("failed to extract worker: %s", err.Error()))
	}
	defer os.Remove(bundlePath)

	// Build the worker spec and write it to a temp JSON file (avoids
	// fragile command-line escaping of nested data).
	spec := struct {
		Path       string          `json:"path"`
		ScriptPath string          `json:"script_path"`
		AssetsDir  string          `json:"assets_dir,omitempty"`
		Data       json.RawMessage `json:"data,omitempty"`
	}{
		Path:       outputPath,
		ScriptPath: scriptPath,
		AssetsDir:  assetsDir,
		Data:       p.Data,
	}
	specFile, err := os.CreateTemp("", "hwcloud-pptx-spec-*.json")
	if err != nil {
		return officeToolError("pptx_write", fmt.Sprintf("failed to create spec file: %s", err.Error()))
	}
	defer os.Remove(specFile.Name())
	if err := json.NewEncoder(specFile).Encode(spec); err != nil {
		specFile.Close()
		return officeToolError("pptx_write", fmt.Sprintf("failed to write spec: %s", err.Error()))
	}
	if err := specFile.Close(); err != nil {
		return officeToolError("pptx_write", fmt.Sprintf("failed to close spec: %s", err.Error()))
	}

	// Run the Node worker.
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, nodePath, bundlePath, specFile.Name())
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		// Try to parse a structured error from stdout first.
		var result pptxWriteWorkerResult
		if len(bytes.TrimSpace(stdout)) > 0 && json.Unmarshal(bytes.TrimSpace(stdout), &result) == nil && result.Error != "" {
			return officeToolError("pptx_write", fmt.Sprintf("worker failed: %s", result.Error))
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return officeToolError("pptx_write", fmt.Sprintf("failed to run Node worker: %s", detail))
	}

	var result pptxWriteWorkerResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &result); err != nil {
		return officeToolError("pptx_write", fmt.Sprintf("failed to parse worker output: %s", err.Error()))
	}
	if !result.OK {
		return officeToolError("pptx_write", fmt.Sprintf("failed to write PowerPoint: %s", result.Error))
	}

	// Report file size for the model.
	var size int64
	if info, err := os.Stat(result.Path); err == nil {
		size = info.Size()
	}
	return officeToolText(fmt.Sprintf("Successfully wrote PowerPoint file: %s\n%d slide(s) written (%d bytes)",
		result.Path, result.SlideCount, size))
}

type pptxWriteWorkerResult struct {
	OK         bool   `json:"ok"`
	Path       string `json:"path"`
	SlideCount int    `json:"slideCount"`
	Mode       string `json:"mode"`
	Error      string `json:"error"`
}

// extractPptxWorkerBundle writes the embedded worker.bundle.mjs to a temp
// file and returns its path. The caller must remove it when done.
func extractPptxWorkerBundle() (string, error) {
	data, err := pptxWorkerBundleFS.ReadFile("pptx-worker/worker.bundle.mjs")
	if err != nil {
		return "", fmt.Errorf("read embedded worker: %w", err)
	}
	f, err := os.CreateTemp("", "hwcloud-pptx-worker-*.mjs")
	if err != nil {
		return "", fmt.Errorf("create temp worker file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write temp worker: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("close temp worker: %w", err)
	}
	return f.Name(), nil
}
