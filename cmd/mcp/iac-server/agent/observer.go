package agent

import (
	"context"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// jobObserver streams the server LLM's per-turn text output into job logs
// and reports fine-grained progress via the runtime's stage observer.
//
// It is a pure observer (read-only, per the RunObserver contract) — unlike
// hooks which may mutate results. The same instance is shared by all agents;
// the job output sink and progress callback are read from each run's context,
// so concurrent jobs are isolated and non-job runs are a no-op.
type jobObserver struct{}

// ObserveStage forwards model output to the job log and reports progress from
// the LLM's per-turn text. The progress callback (from ctx) writes to the job
// file; the MCP Get loop polls the file and forwards new messages to the
// client. The Get loop dedups by message text, so frequent updates from fast
// turns are naturally coalesced — only the latest distinct message reaches the
// client per poll interval.
func (o *jobObserver) ObserveStage(ctx context.Context, ev hwcloud.StageEvent) {
	if ev.Name != hwcloud.StageModelCall || ev.Phase != "leave" {
		return
	}
	content, ok := ev.Detail["content"].(string)
	if !ok || content == "" {
		return
	}

	// Forward full model output to the job output log (existing behavior).
	if sink := JobOutputsFromContext(ctx); sink != nil {
		sink(content)
	}

	// Report fine-grained progress from the model's own text. The LLM
	// narrates its plan each turn, which is more intuitive to the user
	// than raw tool names. We forward the first line of the content as
	// the progress message. cur/tot are 0 (unknown) — progress is
	// non-monotonic across turns and the dedup in Get handles coalescing.
	if progress := hwcloud.ProgressFromContext(ctx); progress != nil {
		if msg := firstLine(content); msg != "" {
			progress(msg, 0, 0)
		}
	}
}

// firstLine returns the first non-empty line of s, trimmed. The model's
// per-turn content often starts with a one-line summary of its intent;
// everything after is reasoning detail the user doesn't need in a progress
// bar. If the content is a single line with no newline, it's returned whole.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

var _ hwcloud.RunObserver = (*jobObserver)(nil)
