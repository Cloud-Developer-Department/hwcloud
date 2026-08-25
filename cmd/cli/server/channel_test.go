package server

import (
	"strings"
	"testing"

	"github.com/Cloud-Developer-Department/hwcloud/channel"
)

func TestToolCardCompleted(t *testing.T) {
	card := toolCallSubCard(toolCallEntry{name: "shell", args: `{"command":"echo hello"}`, status: "completed", output: "hello\n", title: "shell echo hello"})
	if card.Header.Title != "**shell** echo hello ✓" {
		t.Errorf("title = %q", card.Header.Title)
	}
	if !card.Header.TitleMarkdown {
		t.Errorf("TitleMarkdown should be true")
	}
	if !strings.Contains(card.Content, "echo hello") {
		t.Errorf("content should contain command: %s", card.Content)
	}
	if !strings.Contains(card.Content, "hello") {
		t.Errorf("content should contain output: %s", card.Content)
	}
}

func TestToolCardFailed(t *testing.T) {
	card := toolCallSubCard(toolCallEntry{name: "write", args: `{"path":"/tmp/x"}`, status: "failed", output: "error: permission denied", title: "write /tmp/x"})
	if !strings.Contains(card.Header.Title, "✗") {
		t.Errorf("failed card should have ✗ in title: %s", card.Header.Title)
	}
}

func TestToolCardInProgress(t *testing.T) {
	card := toolCallSubCard(toolCallEntry{name: "shell", args: `{"command":"sleep 10"}`, status: "in_progress", output: "running...", title: "shell sleep 10"})
	if !strings.Contains(card.Header.Title, "shell") {
		t.Errorf("in-progress title should contain name: %s", card.Header.Title)
	}
}

func TestFormatInputShell(t *testing.T) {
	result := formatInput("shell", `{"command":"ls -la"}`)
	if !strings.Contains(result, "ls -la") {
		t.Errorf("should contain command: %s", result)
	}
}

func TestFormatInputRead(t *testing.T) {
	result := formatInput("read", `{"path":"/src/main.go"}`)
	if !strings.Contains(result, "/src/main.go") {
		t.Errorf("should contain path: %s", result)
	}
}

func TestFormatInputGrep(t *testing.T) {
	result := formatInput("grep", `{"query":"TODO","path":"/src"}`)
	if !strings.Contains(result, "TODO") && !strings.Contains(result, "`") {
		t.Errorf("should contain query: %s", result)
	}
}

func TestFormatInputUnknown(t *testing.T) {
	result := formatInput("unknown_tool", `{"key":"val"}`)
	if !strings.Contains(result, "```") {
		t.Errorf("unknown tool should show raw args in code block: %s", result)
	}
}

func TestFormatInputShellWithBackticks(t *testing.T) {
	cmd := "echo ```bash```"
	result := formatInput("shell", `{"command":"`+cmd+`"}`)
	fence4 := strings.Repeat("`", 4)
	if !strings.HasPrefix(result, fence4) {
		t.Errorf("shell command with ``` should use 4-tick fence, got: %s", result)
	}
	if !strings.Contains(result, cmd) {
		t.Errorf("result should contain command: %s", result)
	}
}

func TestFormatInputUnknownWithBackticks(t *testing.T) {
	args := "{\"key\":\"```x```\"}"
	result := formatInput("unknown_tool", args)
	fence4 := strings.Repeat("`", 4)
	if !strings.HasPrefix(result, fence4) {
		t.Errorf("unknown tool args with ``` should use 4-tick fence, got: %s", result)
	}
}

func TestParsePlanCreateValid(t *testing.T) {
	args := `{"goal":"refactor auth","steps":[{"id":"1","content":"extract middleware","priority":"high"},{"id":"2","content":"add tests","priority":"low"}]}`
	goal, steps := parsePlanCreate(args)
	if goal != "refactor auth" {
		t.Errorf("goal = %q", goal)
	}
	if !strings.Contains(steps, "extract middleware") {
		t.Errorf("steps should contain step 1: %s", steps)
	}
	if !strings.Contains(steps, "add tests") {
		t.Errorf("steps should contain step 2: %s", steps)
	}
}

func TestParsePlanCreateEmpty(t *testing.T) {
	goal, steps := parsePlanCreate(`{}`)
	if goal != "" || steps != "" {
		t.Errorf("expected empty, got goal=%q steps=%q", goal, steps)
	}
}

func TestParsePlanCreateInvalidJSON(t *testing.T) {
	goal, steps := parsePlanCreate(`not json`)
	if goal != "" || steps != "" {
		t.Errorf("expected empty for invalid JSON, got goal=%q steps=%q", goal, steps)
	}
}

func TestParsePlanCreatePriorityEmoji(t *testing.T) {
	args := `{"goal":"test","steps":[{"id":"h","content":"high","priority":"high"},{"id":"m","content":"med","priority":"medium"},{"id":"l","content":"low","priority":"low"}]}`
	_, steps := parsePlanCreate(args)
	if !strings.Contains(steps, "🔴") {
		t.Errorf("high priority should have red circle emoji: %s", steps)
	}
	if !strings.Contains(steps, "🟡") {
		t.Errorf("medium priority should have yellow circle emoji: %s", steps)
	}
	if !strings.Contains(steps, "🟢") {
		t.Errorf("low priority should have green circle emoji: %s", steps)
	}
}

func TestCardTooLarge(t *testing.T) {
	card := &channel.Card{Header: channel.CardHeader{Title: "test"}, Content: "x"}

	t.Run("nil_sizer_returns_false", func(t *testing.T) {
		if cardTooLarge(nil, card) {
			t.Error("nil sizer should never report too large")
		}
	})

	t.Run("under_limit", func(t *testing.T) {
		sizer := &mockSizer{size: maxCardBytes - 1}
		if cardTooLarge(sizer, card) {
			t.Error("size below limit should report false")
		}
	})

	t.Run("at_limit", func(t *testing.T) {
		sizer := &mockSizer{size: maxCardBytes}
		if cardTooLarge(sizer, card) {
			t.Error("size at limit should report false")
		}
	})

	t.Run("over_limit", func(t *testing.T) {
		sizer := &mockSizer{size: maxCardBytes + 1}
		if !cardTooLarge(sizer, card) {
			t.Error("size over limit should report true")
		}
	})
}

func TestRunCardInterleaved(t *testing.T) {
	// Two text blocks with a tool call in between should produce 3 panels:
	// [text (FoldNone)] [tool (FoldCollapsed)] [text (FoldNone)]
	blks := []block{
		{text: "Let me check."},
		{tool: &toolCallEntry{name: "shell", args: `{"command":"ls"}`, status: "completed", title: "shell ls"}},
		{text: "Done."},
	}
	card := runCard(stageDone, "", blks, "")
	if len(card.Panels) != 3 {
		t.Fatalf("expected 3 panels, got %d", len(card.Panels))
	}
	if card.Panels[0].Fold != channel.FoldNone {
		t.Errorf("panel 0 should be FoldNone, got %v", card.Panels[0].Fold)
	}
	if card.Panels[1].Fold != channel.FoldCollapsed {
		t.Errorf("panel 1 should be FoldCollapsed, got %v", card.Panels[1].Fold)
	}
	if card.Panels[2].Fold != channel.FoldNone {
		t.Errorf("panel 2 should be FoldNone, got %v", card.Panels[2].Fold)
	}
}

type mockSizer struct{ size int }

func (m *mockSizer) CardSize(_ *channel.Card) int { return m.size }
