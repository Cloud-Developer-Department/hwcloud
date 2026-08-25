package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ppt "github.com/casibase/goppt"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// ── pptx_read ──

// pptxReadTool reads text content from a PowerPoint (.pptx) file, slide by
// slide, using the pure-Go goppt library.
type pptxReadTool struct {
	workDir string
}

type pptxReadParams struct {
	Path string `json:"path" jsonschema:"description=Path to the .pptx file to read"`
}

func (t *pptxReadTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "pptx_read",
		Description: "Read text content from a PowerPoint (.pptx) file and return it slide by slide. " +
			"Each slide section lists the slide name and all readable text extracted from shapes (rich text, auto shapes, groups, tables) and speaker notes.",
		Parameters: hwcloud.SchemaOf[pptxReadParams](),
	}
}

func (t *pptxReadTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[pptxReadParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("pptx_read: %w", err), false, "")
	}
	abs, err := ValidatePath(t.workDir, p.Path)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("pptx_read: %w", err), false, "")
	}

	result, err := readPptxFile(abs)
	if err != nil {
		return officeToolError("pptx_read", fmt.Sprintf("failed to read PowerPoint file: %s", err.Error()))
	}
	return officeToolText(result)
}

// readPptxFile opens a .pptx file and returns its text content formatted as
// one section per slide.
func readPptxFile(path string) (string, error) {
	reader := &ppt.PPTXReader{}
	pres, err := reader.Read(path)
	if err != nil {
		return "", err
	}

	slideCount := pres.GetSlideCount()
	if slideCount == 0 {
		return "", fmt.Errorf("the PowerPoint file contains no slides")
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Total slides: %d\n", slideCount)

	for i := 0; i < slideCount; i++ {
		slide, err := pres.GetSlide(i)
		if err != nil {
			continue
		}

		name := slide.GetName()
		if name == "" {
			name = fmt.Sprintf("Slide %d", i+1)
		}
		fmt.Fprintf(&sb, "\n=== %s ===\n", name)

		for _, shape := range slide.GetShapes() {
			text := extractShapeText(shape)
			text = strings.TrimSpace(text)
			if text != "" {
				sb.WriteString(text)
				sb.WriteByte('\n')
			}
		}

		if notes := strings.TrimSpace(slide.GetNotes()); notes != "" {
			fmt.Fprintf(&sb, "[Notes] %s\n", notes)
		}
	}
	return sb.String(), nil
}

// extractShapeText recursively extracts all text from a Shape.
func extractShapeText(shape ppt.Shape) string {
	switch shape.GetType() {
	case ppt.ShapeTypeRichText:
		return extractRichTextShapeText(shape.(*ppt.RichTextShape))
	case ppt.ShapeTypeAutoShape:
		return shape.(*ppt.AutoShape).GetText()
	case ppt.ShapeTypeGroup:
		return extractGroupShapeText(shape.(*ppt.GroupShape))
	case ppt.ShapeTypeTable:
		return extractTableShapeText(shape.(*ppt.TableShape))
	default:
		return ""
	}
}

// extractRichTextShapeText collects text from all paragraphs and runs.
func extractRichTextShapeText(rt *ppt.RichTextShape) string {
	var lines []string
	for _, para := range rt.GetParagraphs() {
		var line strings.Builder
		for _, elem := range para.GetElements() {
			switch e := elem.(type) {
			case *ppt.TextRun:
				line.WriteString(e.GetText())
			case *ppt.BreakElement:
				lines = append(lines, line.String())
				line.Reset()
			}
		}
		lines = append(lines, line.String())
	}
	return strings.Join(lines, "\n")
}

// extractGroupShapeText recurses into a group's child shapes.
func extractGroupShapeText(g *ppt.GroupShape) string {
	var parts []string
	for _, child := range g.GetShapes() {
		if t := strings.TrimSpace(extractShapeText(child)); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// extractTableShapeText collects text from all table cells, row by row.
func extractTableShapeText(t *ppt.TableShape) string {
	var lines []string
	for _, row := range t.GetRows() {
		var cells []string
		for _, cell := range row {
			if cell == nil {
				cells = append(cells, "")
				continue
			}
			var cellText strings.Builder
			for _, para := range cell.GetParagraphs() {
				for _, elem := range para.GetElements() {
					if tr, ok := elem.(*ppt.TextRun); ok {
						cellText.WriteString(tr.GetText())
					}
				}
			}
			cells = append(cells, cellText.String())
		}
		lines = append(lines, strings.Join(cells, "\t"))
	}
	return strings.Join(lines, "\n")
}
