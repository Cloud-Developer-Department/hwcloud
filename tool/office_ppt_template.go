package tool

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	office "github.com/the-open-agent/office-tool-use"
	officemodel "github.com/the-open-agent/office-tool-use/model"
	"github.com/the-open-agent/office-tool-use/ooxml"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/utils"
)

// ── pptx_template_analyze ──

// pptxTemplateAnalyzeTool analyzes a .pptx template and returns its fillable
// structure (slide types, text slot IDs, image/table/chart/SmartArt IDs) as
// JSON. The model uses the returned IDs to build a fill plan.
type pptxTemplateAnalyzeTool struct{}

type pptxTemplateAnalyzeParams struct {
	Template string `json:"template" jsonschema:"description=Local .pptx path or HTTP(S) URL to the template file"`
}

func (t *pptxTemplateAnalyzeTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "pptx_template_analyze",
		Description: "Analyze a PowerPoint template (.pptx) before filling it. Returns JSON with slide types, text slot IDs, image IDs, table IDs, chart IDs, and SmartArt IDs. " +
			"Use the returned IDs to build a fill plan, then call pptx_template_fill. Accepts a local path or an HTTP(S) URL.",
		Parameters: hwcloud.SchemaOf[pptxTemplateAnalyzeParams](),
	}
}

func (t *pptxTemplateAnalyzeTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[pptxTemplateAnalyzeParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("pptx_template_analyze: %w", err), false, "")
	}
	template := strings.TrimSpace(p.Template)
	if template == "" {
		return officeToolError("pptx_template_analyze", "missing required parameter: template")
	}

	templatePath, cleanup, err := resolvePptxTemplate(ctx, template)
	if err != nil {
		return officeToolError("pptx_template_analyze", fmt.Sprintf("failed to open template: %s", err.Error()))
	}
	defer cleanup()

	library, err := office.AnalyzeFile(templatePath, ooxml.DefaultLimits())
	if err != nil {
		return officeToolError("pptx_template_analyze", fmt.Sprintf("failed to analyze template: %s", err.Error()))
	}

	data, err := json.MarshalIndent(library, "", "  ")
	if err != nil {
		return officeToolError("pptx_template_analyze", fmt.Sprintf("failed to encode analysis: %s", err.Error()))
	}
	return officeToolText(string(data))
}

// ── pptx_template_fill ──

// pptxTemplateFillTool creates a new .pptx by deterministically filling and
// reusing slides from an existing template, per a fill plan built from
// pptx_template_analyze output.
type pptxTemplateFillTool struct{}

type pptxTemplateFillParams struct {
	Template           string          `json:"template" jsonschema:"description=Local .pptx path or HTTP(S) URL to the template"`
	Path               string          `json:"path" jsonschema:"description=Output .pptx path; relative paths resolve to the Documents folder"`
	Plan               json.RawMessage `json:"plan" jsonschema:"description=A template_fill_pptx_plan.v1 plan built from pptx_template_analyze output"`
	Transition         string          `json:"transition,omitempty" jsonschema:"description=Default slide transition; use keep to preserve the source"`
	TransitionDuration float64         `json:"transition_duration,omitempty" jsonschema:"description=Default transition duration in seconds when setting a transition"`
}

func (t *pptxTemplateFillTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "pptx_template_fill",
		Description: "Create a new PowerPoint file by filling and reusing slides from an existing template. " +
			"Call pptx_template_analyze first and use its exact slide, slot, table, chart, image, and SmartArt IDs in the plan. " +
			"Supports text replacements, table/chart/image/SmartArt edits, slide reordering/repetition, and notes. " +
			"The output contains exactly the slides in plan order. " +
			"Prefer this over pptx_write when a template is available — it needs no Node.js and preserves the template's design/layout.",
		Parameters: hwcloud.SchemaOf[pptxTemplateFillParams](),
	}
}

func (t *pptxTemplateFillTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[pptxTemplateFillParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("pptx_template_fill: %w", err), false, "")
	}
	template := strings.TrimSpace(p.Template)
	outputPath := strings.TrimSpace(p.Path)
	if template == "" {
		return officeToolError("pptx_template_fill", "missing required parameter: template")
	}
	if outputPath == "" {
		return officeToolError("pptx_template_fill", "missing required parameter: path")
	}
	if strings.ToLower(filepath.Ext(outputPath)) != ".pptx" {
		return officeToolError("pptx_template_fill", "output must use the .pptx extension")
	}
	if len(p.Plan) == 0 || string(p.Plan) == "null" {
		return officeToolError("pptx_template_fill", "missing required parameter: plan")
	}

	var plan officemodel.Plan
	if err := json.Unmarshal(p.Plan, &plan); err != nil {
		return officeToolError("pptx_template_fill", fmt.Sprintf("invalid plan: %s", err.Error()))
	}
	if plan.Schema != officemodel.PlanSchema {
		return officeToolError("pptx_template_fill", "plan schema must be template_fill_pptx_plan.v1")
	}

	templatePath, cleanup, err := resolvePptxTemplate(ctx, template)
	if err != nil {
		return officeToolError("pptx_template_fill", fmt.Sprintf("failed to open template: %s", err.Error()))
	}
	defer cleanup()

	transition := strings.TrimSpace(p.Transition)
	if transition == "" {
		transition = "keep"
	}
	duration := p.TransitionDuration
	if duration == 0 {
		duration = 0.5
	}

	// Resolve any HTTP(S) image paths in the plan to local temp files.
	cleanupImages, err := resolvePptxPlanImages(ctx, &plan, ooxml.DefaultLimits().MaxPartSize)
	if err != nil {
		return officeToolError("pptx_template_fill", fmt.Sprintf("failed to resolve plan images: %s", err.Error()))
	}
	defer cleanupImages()

	outputPath = resolveOutputPath(outputPath)
	library, err := office.AnalyzeFile(templatePath, ooxml.DefaultLimits())
	if err != nil {
		return officeToolError("pptx_template_fill", fmt.Sprintf("failed to analyze template: %s", err.Error()))
	}

	report, err := office.FillFile(templatePath, outputPath, &plan, officemodel.ApplyOptions{
		Transition:         transition,
		TransitionDuration: duration,
		Library:            library,
	}, ooxml.DefaultLimits())
	if err != nil {
		if report != nil {
			if reportData, mErr := json.MarshalIndent(compactPptxCheckReport(report), "", "  "); mErr == nil {
				return officeToolError("pptx_template_fill", fmt.Sprintf("failed to fill template: %s\nValidation report:\n%s", err.Error(), reportData))
			}
		}
		return officeToolError("pptx_template_fill", fmt.Sprintf("failed to fill template: %s", err.Error()))
	}

	reportText := "none"
	if reportData, mErr := json.MarshalIndent(compactPptxCheckReport(report), "", "  "); mErr == nil {
		reportText = string(reportData)
	}
	return officeToolText(fmt.Sprintf("Successfully filled PowerPoint template: %s\n%d slide(s) written\nValidation report:\n%s",
		outputPath, len(plan.Slides), reportText))
}

// ── template resolution (local path or HTTP download) ──

const pptxTemplateDownloadLimit = 100 << 20 // 100 MB

// resolvePptxTemplate resolves a template reference (local path or HTTP URL)
// to a local file path. When downloading, returns a cleanup func to remove
// the temp file.
func resolvePptxTemplate(ctx context.Context, value string) (string, func(), error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", func() {}, fmt.Errorf("invalid template location: %w", err)
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		return downloadPptxTemplate(ctx, parsed)
	}
	if parsed.Scheme != "" && strings.Contains(value, "://") {
		return "", func() {}, fmt.Errorf("unsupported URL scheme %q; only HTTP(S) is allowed", parsed.Scheme)
	}

	path, err := filepath.Abs(value)
	if err != nil {
		return "", func() {}, err
	}
	if err := validatePptxPackage(path); err != nil {
		return "", func() {}, err
	}
	return path, func() {}, nil
}

// downloadPptxTemplate downloads a .pptx from an HTTP(S) URL to a temp file.
// Uses utils.SharedClient for SSRF safety + redirect limits.
func downloadPptxTemplate(ctx context.Context, location *url.URL) (string, func(), error) {
	release, err := utils.AcquireWebSlot(ctx)
	if err != nil {
		return "", func() {}, err
	}
	defer release()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		return "", func() {}, err
	}
	client := utils.SharedClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", func() {}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", func() {}, fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	f, err := os.CreateTemp("", "hwcloud-pptx-template-*.pptx")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }

	limited := io.LimitReader(resp.Body, pptxTemplateDownloadLimit+1)
	written, copyErr := io.Copy(f, limited)
	closeErr := f.Close()
	if copyErr != nil {
		cleanup()
		return "", func() {}, copyErr
	}
	if closeErr != nil {
		cleanup()
		return "", func() {}, closeErr
	}
	if written > pptxTemplateDownloadLimit {
		cleanup()
		return "", func() {}, fmt.Errorf("template exceeds the 100 MB download limit")
	}
	if err := validatePptxPackage(path); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

// resolvePptxPlanImages downloads any HTTP(S) image_path in the plan's
// image_edits to temp files, replacing the URL with the local path.
func resolvePptxPlanImages(ctx context.Context, plan *officemodel.Plan, limit int64) (func(), error) {
	var tempFiles []string
	cleanup := func() {
		for _, path := range tempFiles {
			_ = os.Remove(path)
		}
	}

	for slideIdx := range plan.Slides {
		slide := &plan.Slides[slideIdx]
		for editIdx := range slide.ImageEdits {
			edit := &slide.ImageEdits[editIdx]
			edit.ImagePath = strings.TrimSpace(edit.ImagePath)
			if edit.ImagePath == "" {
				cleanup()
				return nil, fmt.Errorf("image edit %s: empty image_path", edit.ImageID)
			}

			parsed, err := url.Parse(edit.ImagePath)
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("image edit %s: invalid image_path: %w", edit.ImageID, err)
			}
			if parsed.Scheme == "http" || parsed.Scheme == "https" {
				path, dlErr := downloadPptxImage(ctx, parsed, limit)
				if dlErr != nil {
					cleanup()
					return nil, fmt.Errorf("image edit %s: %s", edit.ImageID, dlErr.Error())
				}
				tempFiles = append(tempFiles, path)
				edit.ImagePath = path
				continue
			}
			if parsed.Scheme != "" && strings.Contains(edit.ImagePath, "://") {
				cleanup()
				return nil, fmt.Errorf("image edit %s: unsupported URL scheme; only HTTP(S) allowed", edit.ImageID)
			}
			absPath, err := filepath.Abs(edit.ImagePath)
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("image edit %s: cannot resolve path: %w", edit.ImageID, err)
			}
			edit.ImagePath = absPath
		}
	}
	return cleanup, nil
}

func downloadPptxImage(ctx context.Context, location *url.URL, limit int64) (string, error) {
	release, err := utils.AcquireWebSlot(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		return "", err
	}
	client := utils.SharedClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	f, err := os.CreateTemp("", "hwcloud-pptx-image-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	limited := io.LimitReader(resp.Body, limit+1)
	written, copyErr := io.Copy(f, limited)
	if copyErr != nil {
		f.Close()
		_ = os.Remove(path)
		return "", copyErr
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if written > limit {
		_ = os.Remove(path)
		return "", fmt.Errorf("image exceeds the %d MB size limit", limit>>20)
	}
	if written == 0 {
		_ = os.Remove(path)
		return "", fmt.Errorf("downloaded image is empty")
	}
	return path, nil
}

// validatePptxPackage checks that a file is a valid .pptx (ZIP with the
// required OOXML entries).
func validatePptxPackage(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("template path is a directory")
	}
	if strings.ToLower(filepath.Ext(path)) != ".pptx" {
		return fmt.Errorf("template must use the .pptx extension")
	}
	reader, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("not a valid ZIP/PPTX package: %w", err)
	}
	defer reader.Close()
	required := map[string]bool{
		"[Content_Types].xml":  false,
		"ppt/presentation.xml": false,
	}
	for _, entry := range reader.File {
		if _, ok := required[entry.Name]; ok {
			required[entry.Name] = true
		}
	}
	for name, found := range required {
		if !found {
			return fmt.Errorf("invalid PPTX package: missing %s", name)
		}
	}
	return nil
}

// ── validation report compaction ──

// compactPptxCheckReport filters the check report to only ERROR and severe
// WARN results, keeping the output concise for the model.
func compactPptxCheckReport(report *officemodel.CheckReport) *officemodel.CheckReport {
	if report == nil {
		return nil
	}
	compact := &officemodel.CheckReport{
		Schema:  report.Schema,
		Summary: report.Summary,
		Results: []officemodel.CheckResult{},
	}
	for _, item := range report.Results {
		status, _ := item["status"].(string)
		scale, hasScale := checkResultNumber(item["estimated_font_scale_percent"])
		chartFit, hasChartFit := item["category_labels_fit"].(bool)
		if status != "ERROR" && !(status == "WARN" && ((hasScale && scale < 60) || (hasChartFit && !chartFit))) {
			continue
		}
		result := officemodel.CheckResult{}
		for _, key := range []string{
			"status", "plan_slide", "source_slide", "slot_id", "table_id", "chart_id",
			"image_id", "smartart_id", "node_id", "selector", "new_text", "message",
			"estimated_font_scale_percent", "capacity_visual_width", "collisions",
			"category_axis_font_size_pt", "category_label_area_percent",
			"category_labels_fit", "longest_category", "longest_category_visual_width",
			"suggested_max_visual_width",
		} {
			if value, ok := item[key]; ok {
				result[key] = value
			}
		}
		result["suggestion"] = pptxCheckSuggestion(item)
		compact.Results = append(compact.Results, result)
	}
	return compact
}

func pptxCheckSuggestion(item officemodel.CheckResult) string {
	status, _ := item["status"].(string)
	message, _ := item["message"].(string)
	switch {
	case strings.Contains(message, "overlap"):
		return "Shorten this replacement or choose a source slide with more space."
	case strings.Contains(message, "outside slide bounds"):
		return "This edit would push an object off the slide; adjust or choose a different object."
	case strings.Contains(message, "target not found"):
		return "Re-run pptx_template_analyze to get valid IDs for this edit."
	case strings.Contains(message, "font"):
		return "Shorten this replacement to fit the slot without shrinking below the minimum font size."
	case strings.Contains(message, "height"):
		return "Shorten this replacement; the measured text height exceeds the slide boundaries."
	case strings.Contains(message, "line break"):
		return "Keep the title on a single line, or set preserve_line_breaks if the newline is intended."
	case strings.Contains(message, "corrupted"):
		return "The image file is unsupported or truncated; supply a valid PNG/JPEG."
	case status == "warning":
		return "Consider shortening this replacement to stay within the slide's capacity."
	case status == "error":
		return "Resolve this validation error before writing the final file."
	default:
		return "Resolve this validation issue and retry."
	}
}

func checkResultNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
