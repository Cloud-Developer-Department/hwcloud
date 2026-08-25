package governance

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ── Shell command decomposition ──
//
// An "allow always" decision for a shell call must cover the right unit:
// the whole command string changes with every argument, so it would ask
// again for `cat a | grep x` vs `cat a | grep y` despite both being
// already-approved operations. Instead the command is decomposed into:
//
//   - command atoms — every CallExpr (command + args, printed through the
//     syntax printer so equivalent whitespace normalizes identically)
//   - file accesses — every redirection target, classified read/write
//
// The decision is ALL-of: every atom and every file access must be
// remembered as Allow, or the call asks again. A new command in a chain
// or a new file being written therefore re-asks, while a reused one
// doesn't.

// ShellAccess is one file the command touches via redirection.
type ShellAccess struct {
	Path  string // redirection target as written (variables NOT expanded)
	Write bool   // true: > >> <> >| &> &>> ; false: <
}

// ParseShell decomposes a shell command into its command atoms and file
// accesses. A parse failure (invalid syntax) or an AST node type the
// walker does not know returns an error — callers then treat the whole
// command as a single atom (the conservative fallback: it asks every
// time). Partial extraction must NEVER happen: an unhandled node would
// silently drop the commands it contains from the approval check.
func ParseShell(command string) (cmds []string, files []ShellAccess, err error) {
	f, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, nil, err
	}
	pr := syntax.NewPrinter()
	incomplete := false

	var walk func(n syntax.Node)
	var walkWord func(w *syntax.Word)

	// walkWord extracts commands hiding inside words: $(...),
	// <(...)/>(...) process substitution, and quoted nesting. A
	// parameter expansion containing a command substitution (${x:-$(cmd)})
	// is not walked — instead the whole command falls back to the
	// single-key path (conservative: never partially extract).
	walkWord = func(w *syntax.Word) {
		if w == nil {
			return
		}
		for _, p := range w.Parts {
			switch x := p.(type) {
			case *syntax.CmdSubst:
				for _, s := range x.Stmts {
					walk(s)
				}
			case *syntax.ProcSubst:
				for _, s := range x.Stmts {
					walk(s)
				}
			case *syntax.DblQuoted:
				for _, q := range x.Parts {
					walkWord(&syntax.Word{Parts: []syntax.WordPart{q}})
				}
			case *syntax.ParamExp:
				found := false
				syntax.Walk(x, func(n syntax.Node) bool {
					if _, ok := n.(*syntax.CmdSubst); ok {
						found = true
						return false
					}
					return true
				})
				if found {
					incomplete = true
				}
			}
		}
	}

	walk = func(n syntax.Node) {
		switch x := n.(type) {
		case *syntax.File:
			for _, s := range x.Stmts {
				walk(s)
			}
		case *syntax.CallExpr:
			cmds = append(cmds, cmdAtom(pr, x))
			for _, w := range x.Args {
				walkWord(w)
			}
		case *syntax.BinaryCmd: // && || | |& — pipes are binary too in this AST
			walk(x.X)
			walk(x.Y)
		case *syntax.Stmt:
			collectRedirs(x.Redirs, &files)
			if x.Cmd != nil {
				walk(x.Cmd)
			}
		case *syntax.Block: // { ... }
			for _, s := range x.Stmts {
				walk(s)
			}
		case *syntax.Subshell: // ( ... )
			for _, s := range x.Stmts {
				walk(s)
			}
		case *syntax.IfClause:
			for _, s := range x.Cond {
				walk(s)
			}
			for _, s := range x.Then {
				walk(s)
			}
			if x.Else != nil {
				walk(x.Else)
			}
		case *syntax.WhileClause:
			for _, s := range x.Cond {
				walk(s)
			}
			for _, s := range x.Do {
				walk(s)
			}
		case *syntax.ForClause:
			for _, s := range x.Do {
				walk(s)
			}
		case *syntax.CaseClause:
			for _, it := range x.Items {
				for _, s := range it.Stmts {
					walk(s)
				}
			}
		case *syntax.FuncDecl:
			// Function bodies execute when the function is called; their
			// commands are real operations and are extracted like any
			// other command (consistent: an approved command stays
			// approved regardless of the path that executes it).
			if x.Body != nil {
				walk(x.Body)
			}
		case *syntax.TimeClause:
			if x.Stmt != nil {
				walk(x.Stmt)
			}
		case *syntax.CoprocClause:
			if x.Stmt != nil {
				walk(x.Stmt)
			}
		case *syntax.TestDecl:
			if x.Body != nil {
				walk(x.Body)
			}
		case *syntax.TestClause: // [[ ... ]] — command substitutions inside
			if x.X != nil {
				syntax.Walk(x.X, func(n syntax.Node) bool {
					if cs, ok := n.(*syntax.CmdSubst); ok {
						for _, s := range cs.Stmts {
							walk(s)
						}
						return false // handled — prune to avoid double-walk
					}
					return true
				})
			}
		case *syntax.DeclClause: // declare/local/export z=$(cmd)
			for _, a := range x.Args {
				walkWord(a.Value)
			}
		case *syntax.ArithmCmd, *syntax.LetClause:
			// Arithmetic only — no commands, no file accesses.
		default:
			// Unknown node type: NEVER partially extract. Marking the
			// parse incomplete makes the caller fall back to the
			// whole-command key, which asks every time a variant appears.
			incomplete = true
		}
	}
	walk(f)
	if incomplete {
		return nil, nil, errors.New("shell: unsupported syntax — whole-command fallback")
	}
	return cmds, files, nil
}

func sprint(pr *syntax.Printer, n syntax.Node) string {
	var buf strings.Builder
	pr.Print(&buf, n)
	return strings.TrimSpace(buf.String())
}

// cmdAtom renders a call's atom: the bare command name for readonlyCmds
// (approving echo covers every echo variant), the full command+args
// otherwise.
func cmdAtom(pr *syntax.Printer, x *syntax.CallExpr) string {
	if len(x.Args) > 0 {
		if lit, ok := x.Args[0].Parts[0].(*syntax.Lit); ok && readonlyCmds[lit.Value] {
			return lit.Value
		}
	}
	return sprint(pr, x)
}

func collectRedirs(redirs []*syntax.Redirect, out *[]ShellAccess) {
	for _, r := range redirs {
		if a, ok := redirectAccess(r); ok {
			*out = append(*out, a)
		}
	}
}

// redirectAccess maps a redirection operator to a file access. Heredocs
// (`<<`/`<<-`/`<<<`) provide input content and touch no file; `>&`/`<&`
// duplicate file descriptors; both are ignored.
func redirectAccess(r *syntax.Redirect) (ShellAccess, bool) {
	var write bool
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrInOut, syntax.RdrClob, syntax.RdrAll, syntax.AppAll:
		write = true
	case syntax.RdrIn:
		write = false
	default:
		return ShellAccess{}, false
	}
	if r.Word == nil {
		return ShellAccess{}, false
	}
	pr := syntax.NewPrinter()
	var buf strings.Builder
	pr.Print(&buf, r.Word)
	return ShellAccess{Path: buf.String(), Write: write}, true
}

// ── Approval keys ──

// readonlyCmds are side-effect-free query/output commands whose command
// atom is the NAME alone — approving `echo hello` covers every `echo`
// variant (the arguments are output content, not a different operation;
// per-argument granularity would re-ask forever on echo/which/pwd).
// Every other command keeps command+args granularity. The boundary is
// strict: no file-reading command (cat/head/tail/grep) belongs here —
// their arguments are paths and stay in the atom — and file writes via
// redirection are still gated by the file layer regardless.
var readonlyCmds = map[string]bool{
	"echo": true, "printf": true, "which": true, "pwd": true, "date": true,
	"whoami": true, "uname": true, "hostname": true, "dirname": true,
	"basename": true, "true": true, "false": true, "type": true, "sleep": true,
}

// sensitiveDirs keep single-file granularity: directory-level grants
// inside them would approve every file (e.g. /etc/passwd → /etc/ →
// /etc/shadow). Everything else is granted at parent-directory level so
// `> /tmp/build-1234.log` covers `> /tmp/build-5678.log` (dynamic
// filenames would otherwise re-ask forever, defeating allow always).
var sensitiveDirs = []string{
	"/etc", "/root", "/home", "/var", "/usr", "/boot", "/dev", "/proc", "/sys",
}

// sensitiveSegments are directory names that stay single-file wherever
// they appear — including behind variables ($HOME/.ssh/...) which the
// literal-prefix list above cannot see.
var sensitiveSegments = []string{".ssh", ".gnupg", ".aws"}

// FileUnit computes the grant unit for a path: the parent directory for
// ordinary files (directory-level), the path itself inside sensitive
// directories and for root/current-directory files. Relative paths are
// compared as text, not absolutized (the shell's cwd is fixed per
// session, so text comparison is stable).
// The path is cleaned BEFORE the sensitive check: `/tmp/../etc/shadow`
// must normalize to `/etc/shadow` and stay single-file — checking the
// literal string would grant the whole of /etc via the dot-dot variant.
//
// Paths containing variables are NOT normalized at all: filepath.Clean
// would treat `$HOME/..` as a removable literal segment pair and turn
// `$HOME/../etc/shadow` into the relative `etc/shadow`, whose sensitive
// check fails — granting the whole etc/ family via the variable form.
// A variable path stays a single-file text unit instead (no dot-dot
// elimination, no directory grant): `$HOME/../etc/passwd` is a
// different unit than `$HOME/../etc/shadow` and re-asks.
func FileUnit(path string) string {
	if strings.Contains(path, "$") {
		return path
	}
	clean := filepath.Clean(path)
	// Literal sensitive prefixes (absolute system dirs).
	for _, d := range sensitiveDirs {
		if clean == d || strings.HasPrefix(clean, d+"/") {
			return clean
		}
	}
	// Sensitive directory segments anywhere in the path (dotfiles like
	// .ssh behind $HOME/... which the literal list cannot see).
	for _, seg := range sensitiveSegments {
		if strings.HasPrefix(clean, seg+"/") || strings.Contains(clean, "/"+seg+"/") {
			return clean
		}
	}
	dir := filepath.Dir(clean)
	if dir == "." || dir == "/" {
		return clean
	}
	return dir + "/"
}

// FileKey is the memory key for one file access. Read and write are
// separate grants: approving a read never approves a write.
func FileKey(write bool, path string) string {
	mode := "r"
	if write {
		mode = "w"
	}
	return "file:" + mode + ":" + hex.EncodeToString(sha256Sum([]byte(FileUnit(path))))
}

// ShellCmdKey is the memory key for one command atom.
func ShellCmdKey(cmd string) string {
	return "shell:cmd:" + hex.EncodeToString(sha256Sum([]byte(cmd)))
}

// MemoryKeys returns the keys a call must ALL be remembered as Allow for
// to skip approval:
//
//   - shell: command atoms + redirection file accesses
//   - write: the target file's grant unit
//   - anything else: nil — the caller falls back to the single
//     ApprovalKey (tool + full args hash)
//
// Parse failures return nil (single-key fallback), which re-asks — the
// conservative direction.
func MemoryKeys(name string, args json.RawMessage) []string {
	switch name {
	case "shell":
		var params struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &params); err != nil || params.Command == "" {
			return nil
		}
		cmds, files, err := ParseShell(params.Command)
		if err != nil {
			return nil
		}
		keys := make([]string, 0, len(cmds)+len(files))
		for _, c := range cmds {
			keys = append(keys, ShellCmdKey(c))
		}
		for _, f := range files {
			keys = append(keys, FileKey(f.Write, f.Path))
		}
		return keys
	case "write":
		var params struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &params); err != nil || params.Path == "" {
			return nil
		}
		return []string{FileKey(true, params.Path)}
	}
	return nil
}
