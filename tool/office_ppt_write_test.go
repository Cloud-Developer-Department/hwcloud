package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

func TestPptxWriteSmoke(t *testing.T) {
	// Skip if Node.js is not available — this test needs the runtime.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not found, skipping pptx_write smoke test")
	}

	script := `
export default async function build(pptx, ctx) {
  const slide = pptx.addSlide();
  slide.addText("Hello from Hwcloud PPT!", { x: 1, y: 1, w: 8, h: 1, fontSize: 36, bold: true });
}
`
	outPath := t.TempDir() + "/test.pptx"
	args, _ := json.Marshal(map[string]any{
		"path":   outPath,
		"script": script,
	})

	tools := NewOfficeTools(t.TempDir())
	// Find pptx_write by name (index is fragile as tools are added).
	var writeTool hwcloud.Tool
	for _, t2 := range tools {
		if t2.Definition().Name == "pptx_write" {
			writeTool = t2
			break
		}
	}
	if writeTool == nil {
		t.Fatal("pptx_write tool not found in NewOfficeTools")
	}
	result := writeTool.Execute(context.Background(), args)
	if result.Error != nil {
		t.Fatalf("pptx_write failed: %s", result.Error.Message)
	}
	t.Log("result:", result.Content)

	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	if info.Size() < 1000 {
		t.Fatalf("output file too small (%d bytes), likely invalid", info.Size())
	}
	t.Logf("created %s (%d bytes)", outPath, info.Size())
}

func TestPptxWriteMissingNode(t *testing.T) {
	// Temporarily hide node by overriding PATH. t.Setenv saves/restores
	// automatically and marks the test as ineligible for parallel run,
	// so this cannot race with TestPptxWriteSmoke.
	t.Setenv("PATH", "/nonexistent")

	args, _ := json.Marshal(map[string]any{
		"path":   t.TempDir() + "/out.pptx",
		"script": "export default async function build(pptx, ctx) {}",
	})
	tools := NewOfficeTools(t.TempDir())
	var writeTool hwcloud.Tool
	for _, t2 := range tools {
		if t2.Definition().Name == "pptx_write" {
			writeTool = t2
			break
		}
	}
	if writeTool == nil {
		t.Fatal("pptx_write tool not found")
	}
	result := writeTool.Execute(context.Background(), args)
	if result.Error == nil {
		t.Fatal("expected error when node is missing")
	}
	if result.Error.Code != "" {
		t.Logf("error code: %s", result.Error.Code)
	}
	// The error message should mention Node.js so the model knows what to install.
	msg := result.Error.Message
	if !strings.Contains(msg, "Node.js") {
		t.Fatalf("error should mention Node.js, got: %s", msg)
	}
	t.Logf("got expected error: %s", msg)
}
