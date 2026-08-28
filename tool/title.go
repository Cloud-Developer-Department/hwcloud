package tool

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// ToolTitle builds a human-readable title for a tool call.
// Extracts the most informative field from the tool arguments JSON.
func ToolTitle(name string, args string) string {
	var params struct {
		Path        string `json:"path"`
		Line        int    `json:"line"`
		Limit       int    `json:"limit"`
		Command     string `json:"command"`
		Description string `json:"description"`
		Pattern     string `json:"pattern"`
		Glob        string `json:"glob"`
		Query       string `json:"query"`
		Goal        string `json:"goal"`
		URL         string `json:"url"`
		Name        string `json:"name"`
		Template    string `json:"template"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return name
	}
	switch name {
	case "read":
		if params.Path != "" {
			base := filepath.Base(params.Path)
			if params.Limit > 0 {
				start := params.Line
				if start == 0 {
					start = 1
				}
				return fmt.Sprintf("%s %s (lines %d-%d)", name, base, start, start+params.Limit-1)
			}
			return name + " " + base
		}
	case "edit", "write", "ls":
		if params.Path != "" {
			return name + " " + params.Path
		}
	case "feishu_sendfile":
		if params.Path != "" {
			return name + " " + filepath.Base(params.Path)
		}
	case "wecom_sendfile":
		if params.Path != "" {
			return name + " " + filepath.Base(params.Path)
		}
	case "shell":
		if params.Description != "" {
			return name + " " + TruncateToolArg(params.Description, 60)
		}
		if params.Command != "" {
			return name + " " + TruncateToolArg(params.Command, 60)
		}
	case "grep":
		if params.Pattern != "" {
			title := fmt.Sprintf("%s '%s'", name, params.Pattern)
			if params.Path != "" {
				title += " " + filepath.Base(params.Path)
			}
			if params.Glob != "" {
				title += fmt.Sprintf(" '%s'", params.Glob)
			}
			return title
		}
	case "recall":
		if params.Query != "" {
			return name + " " + params.Query
		}
	case "plan_create":
		if params.Goal != "" {
			return name + " " + TruncateToolArg(params.Goal, 60)
		}
	case "websearch":
		if params.Query != "" {
			return name + " " + TruncateToolArg(params.Query, 60)
		}
	case "webfetch":
		if params.URL != "" {
			return name + " " + StripURLQuery(params.URL)
		}
	case "load_skill":
		if params.Name != "" {
			return name + " " + params.Name
		}
	case "browser_navigate", "browser_screenshot", "browser_evaluate", "browser_click":
		if params.URL != "" {
			return name + " " + StripURLQuery(params.URL)
		}
	case "browser_use_open":
		if params.URL != "" {
			return name + " " + StripURLQuery(params.URL)
		}
	case "pptx_read", "word_read", "excel_read":
		if params.Path != "" {
			return name + " " + filepath.Base(params.Path)
		}
	case "pptx_write", "word_write", "excel_write":
		if params.Path != "" {
			return name + " " + params.Path
		}
	case "pptx_template_analyze":
		if params.Template != "" {
			return name + " " + filepath.Base(params.Template)
		}
	case "pptx_template_fill":
		if params.Path != "" {
			return name + " " + params.Path
		}
	}
	return name
}

// StripURLQuery removes the query string (and fragment) from rawURL so the
// title shows only scheme://host/path. Falls back to the raw value on parse
// error.
func StripURLQuery(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// TruncateToolArg truncates s to n characters, adding "..." at the end.
func TruncateToolArg(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
