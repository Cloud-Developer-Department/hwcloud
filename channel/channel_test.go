package channel

import (
	"strings"
	"testing"
)

func TestCodeBlockSimple(t *testing.T) {
	got := CodeBlock("ls -la")
	want := "```\nls -la\n```"
	if got != want {
		t.Errorf("CodeBlock(simple) = %q, want %q", got, want)
	}
}

func TestCodeBlockWithBackticks(t *testing.T) {
	content := "```bash\necho hi\n```"
	got := CodeBlock(content)
	fence := strings.Repeat("`", 4)
	want := fence + "\n" + content + "\n" + fence
	if got != want {
		t.Errorf("CodeBlock(with ```) = %q, want %q", got, want)
	}
}

func TestCodeBlockWithLongerRun(t *testing.T) {
	content := "`````\nweird\n`````"
	got := CodeBlock(content)
	fence := strings.Repeat("`", 6)
	want := fence + "\n" + content + "\n" + fence
	if got != want {
		t.Errorf("CodeBlock(5-tick run) = %q, want %q", got, want)
	}
}

func TestCodeBlockEmpty(t *testing.T) {
	got := CodeBlock("")
	want := "```\n\n```"
	if got != want {
		t.Errorf("CodeBlock(empty) = %q, want %q", got, want)
	}
}

func TestCodeBlockInlineBacktick(t *testing.T) {
	content := "use `git` now"
	got := CodeBlock(content)
	want := "```\n" + content + "\n```"
	if got != want {
		t.Errorf("CodeBlock(inline tick) = %q, want %q", got, want)
	}
}
