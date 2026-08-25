package acp

import (
	"fmt"
	"strings"

	"github.com/Cloud-Developer-Department/hwcloud/slash"
)

// buildCommandRegistry creates the slash command registry with all built-in
// commands. The server provides the callbacks by closing over AgentServer.
func (s *AgentServer) buildCommandRegistry() *slash.Registry {
	r := slash.NewRegistry()

	r.Register("help", "Show available commands and capabilities", nil,
		func(ctx slash.Context, args string) (string, error) {
			var out string
			out += "Built-in commands:\n\n"
			for _, c := range r.Available() {
				line := "  /" + c.Name + " — " + c.Description
				if c.Input != nil {
					line += " (e.g. /" + c.Name + " " + c.Input.Hint + ")"
				}
				out += line + "\n"
			}
			return out, nil
		})

	r.Register("model", "List or switch models", &slash.InputHint{Hint: "model-id"},
		func(ctx slash.Context, args string) (string, error) {
			models := ctx.ListModels()
			if args == "" {
				if len(models) == 0 {
					return "No models configured.\n", nil
				}
				var out string
				out += "Available models:\n\n"
				for _, id := range models {
					out += "  " + id + "\n"
				}
				out += "\nUse /model <id> to switch.\n"
				return out, nil
			}
			if err := ctx.SetModel(args); err != nil {
				return "", err
			}
			return "Switched to model: " + args + "\n", nil
		})

	r.Register("mode", "Switch session mode", &slash.InputHint{Hint: "auto|manual|plan"},
		func(ctx slash.Context, args string) (string, error) {
			switch args {
			case "auto", "manual", "plan":
				if err := ctx.SetMode(args); err != nil {
					return "", err
				}
				return "Switched to " + args + " mode.\n", nil
			default:
				return "Usage: /mode auto|manual|plan (current: " + ctx.Mode + ")\n", nil
			}
		})

	r.Register("compact", "Compress session history into a summary", nil,
		func(ctx slash.Context, _ string) (string, error) {
			if ctx.Compact == nil {
				return "Compression unavailable (no summarizer configured).\n", nil
			}
			st, err := ctx.Compact()
			if err != nil {
				return "", err
			}
			if st.Compressed == 0 {
				return "Nothing to compact — session is empty or no summarizer configured.\n", nil
			}
			return fmt.Sprintf("Compacted %d messages → summary (freed ~%d tokens).\n",
				st.Compressed, st.FreedTokens), nil
		})

	r.Register("context", "Show context window usage", nil,
		func(ctx slash.Context, _ string) (string, error) {
			if ctx.ContextStats == nil {
				return "Context window: " + fmt.Sprintf("%d", ctx.TotalTokens) + " total tokens used.\n", nil
			}
			st, err := ctx.ContextStats()
			if err != nil {
				return "", err
			}
			var b strings.Builder
			b.WriteString(fmt.Sprintf("Summary: %d tokens\n", st.SummaryTokens))
			b.WriteString(fmt.Sprintf("Working: %d tokens\n", st.WorkingTokens))
			b.WriteString(fmt.Sprintf("Used:    %d tokens\n", st.SummaryTokens+st.WorkingTokens))
			b.WriteString(fmt.Sprintf("Window:  %d tokens\n", st.Window))
			return b.String(), nil
		})

	r.Register("cwd", "Show current working directory", nil,
		func(ctx slash.Context, _ string) (string, error) {
			return "Working directory: " + ctx.Cwd + "\n", nil
		})

	r.Register("clear", "Clear all session messages", nil,
		func(ctx slash.Context, _ string) (string, error) {
			if err := ctx.Clear(); err != nil {
				return "", err
			}
			return "Session cleared. All messages deleted.\n", nil
		})

	r.Register("rename", "Rename the current session", &slash.InputHint{Hint: "new title"},
		func(ctx slash.Context, args string) (string, error) {
			if args == "" {
				return "Usage: /rename <new title>\n", nil
			}
			if err := ctx.Rename(args); err != nil {
				return "", err
			}
			return "Session renamed to: " + args + "\n", nil
		})

	r.Register("sessions", "List all sessions", nil,
		func(ctx slash.Context, _ string) (string, error) {
			sessions, err := ctx.ListSess()
			if err != nil {
				return "", err
			}
			if len(sessions) == 0 {
				return "No sessions found.\n", nil
			}
			var out string
			current := ctx.SessionID
			for _, si := range sessions {
				marker := " "
				if si.ID == current {
					marker = "*"
				}
				title := si.Title
				if title == "" {
					title = "(untitled)"
				}
				out += marker + " " + si.ID + "  " + si.Cwd + "  " +
					title + "\n"
			}
			return out, nil
		})

	return r
}
