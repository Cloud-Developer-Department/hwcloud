package tool

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuri/excelize/v2"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// Excel (.xlsx) read/write tools via the pure-Go excelize library.
//
// excel_read returns a sheet's content as CSV text (with a metadata header
// showing sheet name, row count, and other sheets). excel_write creates a new
// .xlsx from CSV-formatted text. Both are pure Go — no external runtime.

// ── excel_read ──

type excelReadTool struct {
	workDir string
}

type excelReadParams struct {
	Path  string `json:"path" jsonschema:"description=Path to the .xlsx file to read"`
	Sheet string `json:"sheet,omitempty" jsonschema:"description=Sheet name to read (default: first sheet)"`
}

func (t *excelReadTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "excel_read",
		Description: "Read data from an Excel (.xlsx) file and return it as CSV-formatted text. " +
			"The response header reports the sheet name, row count, and other available sheet names.",
		Parameters: hwcloud.SchemaOf[excelReadParams](),
	}
}

func (t *excelReadTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[excelReadParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("excel_read: %w", err), false, "")
	}
	abs, err := ValidatePath(t.workDir, p.Path)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("excel_read: %w", err), false, "")
	}

	result, err := readExcelFile(abs, strings.TrimSpace(p.Sheet))
	if err != nil {
		return officeToolError("excel_read", fmt.Sprintf("failed to read Excel file: %s", err.Error()))
	}
	return officeToolText(result)
}

// ── excel_write ──

type excelWriteTool struct{}

type excelWriteParams struct {
	Path string `json:"path" jsonschema:"description=Output path for the .xlsx file. Relative paths resolve to the Documents folder."`
	Data string `json:"data" jsonschema:"description=CSV-formatted text: rows separated by newlines, cells by commas. Quoted fields with embedded commas or newlines are supported."`
	Sheet string `json:"sheet,omitempty" jsonschema:"description=Sheet name (default: Sheet1)"`
}

func (t *excelWriteTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "excel_write",
		Description: "Write data to an Excel (.xlsx) file. Creates the file if it does not exist; overwrites otherwise. " +
			"Input is CSV-formatted text (each line is a row, cells are comma-separated). " +
			"Use this when the user wants a .xlsx spreadsheet; use the `write` tool for plain text/code files.",
		Parameters: hwcloud.SchemaOf[excelWriteParams](),
	}
}

func (t *excelWriteTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	p, err := hwcloud.ParseArgs[excelWriteParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("excel_write: %w", err), false, "")
	}
	if strings.TrimSpace(p.Path) == "" {
		return officeToolError("excel_write", "missing required parameter: path")
	}
	sheet := strings.TrimSpace(p.Sheet)
	if sheet == "" {
		sheet = "Sheet1"
	}

	outputPath := resolveOutputPath(strings.TrimSpace(p.Path))
	rowCount, colCount, err := writeExcelFile(outputPath, sheet, p.Data)
	if err != nil {
		return officeToolError("excel_write", fmt.Sprintf("failed to write Excel file: %s", err.Error()))
	}

	var size int64
	if info, err := os.Stat(outputPath); err == nil {
		size = info.Size()
	}
	return officeToolText(fmt.Sprintf("Successfully wrote Excel file: %s\nSheet: %s, %d rows × %d columns (%d bytes)",
		outputPath, sheet, rowCount, colCount, size))
}

// ── excelize helpers ──

// readExcelFile opens an .xlsx and returns the target sheet's content as CSV,
// prefixed with a metadata header (sheet name, row count, other sheets).
func readExcelFile(path, sheetName string) (string, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return "", fmt.Errorf("the Excel file contains no sheets")
	}

	target := sheetName
	if target == "" {
		target = sheets[0]
	} else {
		found := false
		for _, s := range sheets {
			if s == target {
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("sheet %q not found; available sheets: %s",
				target, strings.Join(sheets, ", "))
		}
	}

	rows, err := f.GetRows(target)
	if err != nil {
		return "", fmt.Errorf("failed to read sheet %q: %w", target, err)
	}

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			return "", fmt.Errorf("failed to encode row as CSV: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Sheet: %s (%d rows)\n", target, len(rows))
	if len(sheets) > 1 {
		others := make([]string, 0, len(sheets)-1)
		for _, s := range sheets {
			if s != target {
				others = append(others, s)
			}
		}
		fmt.Fprintf(&sb, "Other sheets: %s\n", strings.Join(others, ", "))
	}
	sb.WriteString(buf.String())
	return sb.String(), nil
}

// writeExcelFile creates (or overwrites) an .xlsx from CSV-formatted text.
// Returns the row count and max column count.
func writeExcelFile(path, sheetName, csvData string) (rowCount, colCount int, err error) {
	dir := filepath.Dir(path)
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return 0, 0, fmt.Errorf("failed to create directory %q: %w", dir, mkErr)
	}

	r := csv.NewReader(strings.NewReader(csvData))
	r.FieldsPerRecord = -1 // allow ragged rows
	records, err := r.ReadAll()
	if err != nil {
		return 0, 0, fmt.Errorf("invalid CSV data: %w", err)
	}

	f := excelize.NewFile()
	defer f.Close()

	defaultSheet := f.GetSheetName(f.GetActiveSheetIndex())
	if defaultSheet != sheetName {
		if renameErr := f.SetSheetName(defaultSheet, sheetName); renameErr != nil {
			return 0, 0, fmt.Errorf("failed to set sheet name: %w", renameErr)
		}
	}

	maxCols := 0
	for rowIdx, record := range records {
		if len(record) > maxCols {
			maxCols = len(record)
		}
		for colIdx, cellValue := range record {
			cellAddr, addrErr := excelize.CoordinatesToCellName(colIdx+1, rowIdx+1)
			if addrErr != nil {
				return 0, 0, fmt.Errorf("invalid cell coordinates (%d,%d): %w", colIdx+1, rowIdx+1, addrErr)
			}
			if setErr := f.SetCellValue(sheetName, cellAddr, cellValue); setErr != nil {
				return 0, 0, fmt.Errorf("failed to write cell %s: %w", cellAddr, setErr)
			}
		}
	}

	if saveErr := f.SaveAs(path); saveErr != nil {
		return 0, 0, fmt.Errorf("failed to save file: %w", saveErr)
	}
	return len(records), maxCols, nil
}
