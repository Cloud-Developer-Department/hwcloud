package governance

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func parseShell(t *testing.T, cmd string) ([]string, []ShellAccess) {
	t.Helper()
	cmds, files, err := ParseShell(cmd)
	if err != nil {
		t.Fatalf("ParseShell(%q): %v", cmd, err)
	}
	return cmds, files
}

func wantCmds(t *testing.T, cmd string, want ...string) {
	t.Helper()
	cmds, _ := parseShell(t, cmd)
	sort.Strings(cmds)
	sort.Strings(want)
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("ParseShell(%q) cmds = %v, want %v", cmd, cmds, want)
	}
}

func wantFiles(t *testing.T, cmd string, want ...ShellAccess) {
	t.Helper()
	_, files := parseShell(t, cmd)
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("ParseShell(%q) files = %v, want %v", cmd, files, want)
	}
}

func TestParseShellSimpleCommand(t *testing.T) {
	wantCmds(t, "cat a", "cat a")
}

func TestParseShellPipeline(t *testing.T) {
	wantCmds(t, "cat a | grep x", "cat a", "grep x")
	wantCmds(t, "cat a | grep x | wc -l", "cat a", "grep x", "wc -l")
}

func TestParseShellAndOrChain(t *testing.T) {
	wantCmds(t, "cat a && ls b", "cat a", "ls b")
	wantCmds(t, "cat a || echo failed", "cat a", "echo") // echo: readonly, name-level
	wantCmds(t, "cat a && ls b || echo x", "cat a", "ls b", "echo")
}

func TestParseShellSemicolon(t *testing.T) {
	wantCmds(t, "cat a; ls b", "cat a", "ls b")
}

func TestParseShellRedirections(t *testing.T) {
	wantFiles(t, "cat < /etc/passwd", ShellAccess{Path: "/etc/passwd", Write: false})
	wantFiles(t, "echo x > /tmp/out.log", ShellAccess{Path: "/tmp/out.log", Write: true})
	wantFiles(t, "echo x >> /var/log/app.log", ShellAccess{Path: "/var/log/app.log", Write: true})
	wantFiles(t, "cat a > /tmp/x < /etc/passwd",
		ShellAccess{Path: "/tmp/x", Write: true},
		ShellAccess{Path: "/etc/passwd", Write: false})
}

func TestParseShellHeredocIgnored(t *testing.T) {
	// Heredocs feed stdin; they touch no file.
	wantFiles(t, "cat > /tmp/x <<EOF\ntext\nEOF", ShellAccess{Path: "/tmp/x", Write: true})
	wantCmds(t, "cat > /tmp/x <<EOF\ntext\nEOF", "cat")
}

func TestParseShellFdDupIgnored(t *testing.T) {
	wantFiles(t, "cmd 2>&1") // fd dup — no file access
	wantFiles(t, "cmd 2>/dev/null", ShellAccess{Path: "/dev/null", Write: true})
}

func TestParseShellQuotedPipe(t *testing.T) {
	// Pipes inside quotes are data, not operators. echo is readonly —
	// the atom is the name; the quoted content carries no commands.
	wantCmds(t, `echo "a | b"`, `echo`)
}

func TestParseShellSubshell(t *testing.T) {
	// echo is readonly (name-level); the $(...) commands are extracted.
	wantCmds(t, "echo $(cat a | wc -l)", "echo", "cat a", "wc -l")
}

func TestParseShellIfClause(t *testing.T) {
	wantCmds(t, "if test -f x; then cat x; else ls; fi", "test -f x", "cat x", "ls")
}

func TestParseShellFunctionBody(t *testing.T) {
	wantCmds(t, "foo() { rm -rf /tmp/x; }; foo", "rm -rf /tmp/x", "foo")
}

func TestParseShellWhitespaceNormalized(t *testing.T) {
	// Printer normalizes whitespace: same command, different spacing.
	a, _ := parseShell(t, "cat   a")
	b, _ := parseShell(t, "cat a")
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("whitespace normalization failed: %v vs %v", a, b)
	}
}

func TestParseShellInvalidSyntax(t *testing.T) {
	if _, _, err := ParseShell("cat ("); err == nil {
		t.Fatal("invalid syntax must error (caller falls back to whole-command key)")
	}
}

// ── FileUnit ──

func TestFileUnitDirectoryLevel(t *testing.T) {
	if got := FileUnit("/tmp/build-1234.log"); got != "/tmp/" {
		t.Fatalf("FileUnit = %q, want /tmp/", got)
	}
	if got := FileUnit("out/a.txt"); got != "out/" {
		t.Fatalf("FileUnit = %q, want out/", got)
	}
	if got := FileUnit("a.txt"); got != "a.txt" {
		t.Fatalf("FileUnit = %q, want a.txt (no directory)", got)
	}
	if got := FileUnit("/x"); got != "/x" {
		t.Fatalf("FileUnit = %q, want /x (root file)", got)
	}
	if got := FileUnit("/tmp/sub/y"); got != "/tmp/sub/" {
		t.Fatalf("FileUnit = %q, want /tmp/sub/ (one level, not recursive)", got)
	}
}

func TestFileUnitSensitiveDirsSingleFile(t *testing.T) {
	for _, p := range []string{"/etc/passwd", "/etc/shadow", "/root/.ssh/id_rsa", "/home/user/secret", "/var/log/syslog", "/usr/bin/x"} {
		if got := FileUnit(p); got != p {
			t.Fatalf("FileUnit(%q) = %q, want single-file (sensitive dir)", p, got)
		}
	}
}

// ── Keys ──

func TestFileKeyReadWriteSeparate(t *testing.T) {
	if FileKey(true, "/tmp/x") == FileKey(false, "/tmp/x") {
		t.Fatal("read and write keys must differ")
	}
	if FileKey(true, "/tmp/build-1.log") != FileKey(true, "/tmp/build-2.log") {
		t.Fatal("same-directory writes must share a key (directory-level grant)")
	}
	if FileKey(false, "/etc/passwd") == FileKey(false, "/etc/shadow") {
		t.Fatal("sensitive-dir reads must differ per file")
	}
}

func TestMemoryKeysShell(t *testing.T) {
	keys := MemoryKeys("shell", json.RawMessage(`{"command":"cat a | grep x > /tmp/o"}`))
	want := []string{ShellCmdKey("cat a"), ShellCmdKey("grep x"), FileKey(true, "/tmp/o")}
	for _, w := range want {
		found := false
		for _, k := range keys {
			if k == w {
				found = true
			}
		}
		if !found {
			t.Fatalf("MemoryKeys missing %q (got %v)", w, keys)
		}
	}
}

func TestMemoryKeysShellParseFailureFallsBack(t *testing.T) {
	if keys := MemoryKeys("shell", json.RawMessage(`{"command":"cat ("}`)); keys != nil {
		t.Fatalf("parse failure must return nil (single-key fallback), got %v", keys)
	}
}

func TestMemoryKeysWriteDirectoryLevel(t *testing.T) {
	keys := MemoryKeys("write", json.RawMessage(`{"path":"out/result.txt","content":"x"}`))
	if len(keys) != 1 || keys[0] != FileKey(true, "out/result.txt") {
		t.Fatalf("write keys = %v, want [%v]", keys, FileKey(true, "out/result.txt"))
	}
}

func TestMemoryKeysOtherToolsNil(t *testing.T) {
	if keys := MemoryKeys("read", json.RawMessage(`{"path":"a.txt"}`)); keys != nil {
		t.Fatalf("read must fall back to single key, got %v", keys)
	}
	if keys := MemoryKeys("webfetch", json.RawMessage(`{}`)); keys != nil {
		t.Fatalf("webfetch must fall back, got %v", keys)
	}
}

// ── P1: sensitive-dir bypass via path tricks ──

// Dot-dot variants must normalize BEFORE the sensitive check, so
// /tmp/../etc/shadow stays single-file instead of granting /etc/.
func TestFileUnitDotDotCannotBypassSensitive(t *testing.T) {
	if got := FileUnit("/tmp/../etc/shadow"); got != "/etc/shadow" {
		t.Fatalf("FileUnit(/tmp/../etc/shadow) = %q, want /etc/shadow", got)
	}
	if got := FileUnit("/tmp/../etc/passwd"); got != "/etc/passwd" {
		t.Fatalf("FileUnit(/tmp/../etc/passwd) = %q, want /etc/passwd", got)
	}
	if FileKey(true, "/tmp/../etc/shadow") == FileKey(true, "/tmp/../etc/passwd") {
		t.Fatal("dot-dot variants of different /etc files must not share a grant")
	}
	if FileKey(true, "/tmp/../etc/shadow") != FileKey(true, "/etc/shadow") {
		t.Fatal("dot-dot variant must normalize to the same single-file key")
	}
}

// Credential dirs behind variables ($HOME/.ssh/...) keep single-file
// granularity via segment matching — the literal prefix list can't see
// them.
func TestFileUnitSensitiveSegments(t *testing.T) {
	for _, p := range []string{
		"$HOME/.ssh/authorized_keys", "$HOME/.ssh/id_rsa",
		"/home/user/.gnupg/secring.gpg", "/x/.aws/credentials",
	} {
		if got := FileUnit(p); got != p {
			t.Fatalf("FileUnit(%q) = %q, want single-file (sensitive segment)", p, got)
		}
	}
}

// ── P2: unsupported nodes must never partially extract ──

func TestParseShellTimeClause(t *testing.T) {
	wantCmds(t, "time rm -rf /tmp/x", "rm -rf /tmp/x")
	wantCmds(t, "time cat a && cat b", "cat a", "cat b")
}

func TestParseShellTestClauseCmdSubst(t *testing.T) {
	wantCmds(t, `[[ $(id -u) = 0 ]] && cat y`, "id -u", "cat y")
	wantCmds(t, `[[ -f /etc/passwd ]] && cat /etc/passwd`, "cat /etc/passwd")
}

func TestParseShellProcSubst(t *testing.T) {
	wantCmds(t, `diff <(cat a) <(cat b)`, "cat a", "cat b", `diff <(cat a) <(cat b)`)
}

func TestParseShellDeclClause(t *testing.T) {
	wantCmds(t, `declare z=$(rm -rf /tmp/a)`, "rm -rf /tmp/a")
}

// A parameter expansion embedding a command substitution (${x:-$(cmd)})
// is not walked — the whole command must fall back (error), never
// partially extract.
func TestParseShellParamExpCmdSubstFallsBack(t *testing.T) {
	if _, _, err := ParseShell(`echo ${x:-$(cat a)}`); err == nil {
		t.Fatal("param-exp embedded command must fall back, not partially extract")
	}
}

// Arithmetic-only constructs extract nothing and parse cleanly (they
// fall back to the single whole-command key via the empty result).
func TestParseShellArithmeticClean(t *testing.T) {
	cmds, files, err := ParseShell(`(( x++ )) && cat a`)
	if err != nil {
		t.Fatalf("arithmetic must not mark incomplete: %v", err)
	}
	if len(files) != 0 || len(cmds) != 1 || cmds[0] != "cat a" {
		t.Fatalf("cmds=%v files=%v, want [cat a] []", cmds, files)
	}
}

// ── P1b: variable-backed dot-dot must not alias into a directory grant ──

// filepath.Clean would treat `$HOME/..` as a removable literal pair and
// turn $HOME/../etc/shadow into relative "etc/shadow" — whose sensitive
// check fails, granting the whole etc/ family. Variable paths must stay
// single-file text units: no normalization, no directory grant.
func TestFileUnitVariableDotDotStaysSingleFile(t *testing.T) {
	got := FileUnit("$HOME/../etc/shadow")
	if got != "$HOME/../etc/shadow" {
		t.Fatalf("FileUnit($HOME/../etc/shadow) = %q, want the literal path", got)
	}
	if FileKey(true, "$HOME/../etc/shadow") == FileKey(true, "$HOME/../etc/passwd") {
		t.Fatal("variable dot-dot variants of different files must not share a grant")
	}
	// The variable form must not collide with the normalized real path
	// either — approving one form never covers the other.
	if FileKey(true, "$HOME/../etc/shadow") == FileKey(true, "/etc/shadow") {
		t.Fatal("variable form must not collide with the cleaned real path")
	}
}

// Variable paths never get directory grants: `> $dir/x` covers only the
// exact text `$dir/x`, not `$dir/y` ($dir's value is unknowable here).
func TestFileUnitVariableNeverDirectoryGrant(t *testing.T) {
	if got := FileUnit("$dir/x"); got != "$dir/x" {
		t.Fatalf("FileUnit($dir/x) = %q, want literal single-file", got)
	}
	if FileKey(true, "$dir/x") == FileKey(true, "$dir/y") {
		t.Fatal("variable paths must not share a directory grant")
	}
	// Non-variable dynamic filenames keep their directory grant.
	if FileKey(true, "/tmp/build-1.log") != FileKey(true, "/tmp/build-2.log") {
		t.Fatal("non-variable same-directory writes must still share a grant")
	}
}

// ── Readonly commands: name-level atoms ──

// echo/which/pwd are side-effect-free: approving one variant covers the
// whole command (arguments are output content, not a different
// operation).
func TestParseShellReadonlyCmdNameAtom(t *testing.T) {
	a, _ := parseShell(t, "echo hello")
	b, _ := parseShell(t, "echo world")
	if len(a) != 1 || a[0] != "echo" || b[0] != "echo" {
		t.Fatalf("readonly atoms = %v / %v, want [echo] / [echo]", a, b)
	}
	if ShellCmdKey(a[0]) != ShellCmdKey(b[0]) {
		t.Fatal("echo variants must share one command-level grant")
	}
	wantCmds(t, "which curl", "which")
	wantCmds(t, "pwd && date", "pwd", "date")
	// Non-readonly commands keep command+args granularity.
	wantCmds(t, "cat a", "cat a")
	wantCmds(t, "sudo echo x", "sudo echo x") // sudo is the command — not readonly
}

// Readonly name-level atom still composes with the file layer: the echo
// is covered, the write to a sensitive file is not.
func TestParseShellReadonlyAtomWithRedirection(t *testing.T) {
	wantFiles(t, "echo x > /etc/passwd", ShellAccess{Path: "/etc/passwd", Write: true})
}
