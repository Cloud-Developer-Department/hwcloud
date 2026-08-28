// Package fs implements the filesystem skill backend: a directory tree of
// SKILL.md files. It is consumed via provider/skill.NewFSBridge.
//
// Directory layout:
//
//	<root>/
//	  example-skill/
//	    SKILL.md
//	    scripts/...
//	  another-skill/
//	    SKILL.md
//
// Each SKILL.md begins with YAML frontmatter (--- ... ---) containing at minimum
// name and description. All frontmatter fields are preserved in Frontmatter.
// The body (after the closing ---) is loaded on demand via Load().
//
// In addition to disk roots, a Loader may carry an embedded fs.FS (set via
// WithEmbedFS) as the lowest-priority source. Embedded skills are bundled into
// the binary at build time (-tags embed) so the agent ships with built-in
// skills without requiring on-disk files. Disk roots always override embedded
// skills of the same name.
package fs

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// embedPathPrefix marks a SkillInfo.Path as pointing into the embedded fs.FS
// rather than the disk. Load checks this prefix to route the read.
const embedPathPrefix = "embed:"

// Loader discovers and loads skills from one or more directory trees and an
// optional embedded filesystem. When multiple roots are given, Discover scans
// them in order and skills in later roots override same-name skills from
// earlier roots (the earlier entry keeps its position in the result, only its
// content is replaced). This lets a project-level root (<cwd>/.agents/skills)
// take priority over a user-level root (~/.agents/skills) when the project
// root is passed last.
//
// The embedded fs.FS (if set via WithEmbedFS) is scanned last, so its skills
// are the lowest priority — disk skills of the same name always win.
type Loader struct {
	roots   []string
	embedFS fs.FS // nil = no embedded source
}

// New creates a Loader rooted at the given directories. With a single
// root it behaves as a classic single-tree loader; passing multiple
// roots enables the override semantics described on Loader.
func New(roots ...string) *Loader {
	return &Loader{roots: roots}
}

// WithEmbedFS adds an embedded filesystem as the lowest-priority skill
// source. Disk roots (user → project) override embedded skills of the same
// name. Returns the receiver for chaining.
func (l *Loader) WithEmbedFS(fsys fs.FS) *Loader {
	l.embedFS = fsys
	return l
}

// Discover scans each root for subdirectories containing SKILL.md, reads
// each file's YAML frontmatter, and returns a SkillInfo for each valid
// skill. Roots are processed in order; a skill from a later root with
// the same name as one from an earlier root replaces the earlier entry
// in place (preserving its position), while skills with new names are
// appended. Skills missing name or description are skipped. A root that
// cannot be read is treated as empty rather than failing the whole call.
//
// After all disk roots, the embedded fs.FS (if set) is scanned the same way.
// Embedded skills use a synthetic Path prefixed with "embed:" so Load can
// route the read back through the embedded filesystem.
func (l *Loader) Discover(ctx context.Context) ([]hwcloud.SkillInfo, error) {
	var skills []hwcloud.SkillInfo
	indexByName := make(map[string]int)

	// addSkill inserts or overrides a skill by name, preserving position.
	addSkill := func(info hwcloud.SkillInfo) {
		if idx, ok := indexByName[info.Name]; ok {
			skills[idx] = info // override in place, keep position
		} else {
			indexByName[info.Name] = len(skills)
			skills = append(skills, info)
		}
	}

	for _, root := range l.roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			// Missing/unreadable root contributes nothing.
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			skillDir := filepath.Join(root, entry.Name())
			mdPath := filepath.Join(skillDir, "SKILL.md")

			fm, _, err := parseFrontmatter(mdPath)
			if err != nil {
				continue
			}

			name, _ := fm["name"].(string)
			desc, _ := fm["description"].(string)
			if name == "" || desc == "" {
				continue
			}

			addSkill(hwcloud.SkillInfo{
				Name:        name,
				Description: desc,
				Frontmatter: fm,
				Path:        skillDir,
			})
		}
	}

	// Embedded source — scanned last, lowest priority. An embedded skill
	// with the same name as a disk skill is skipped (disk wins); it never
	// overrides an existing entry.
	if l.embedFS != nil {
		entries, err := fs.ReadDir(l.embedFS, ".")
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				mdPath := filepath.Join(entry.Name(), "SKILL.md")
				fm, _, err := parseFrontmatterFS(l.embedFS, mdPath)
				if err != nil {
					continue
				}
				name, _ := fm["name"].(string)
				desc, _ := fm["description"].(string)
				if name == "" || desc == "" {
					continue
				}
				// Skip if a disk skill of the same name already exists —
				// embedded skills are the lowest-priority source.
				if _, exists := indexByName[name]; exists {
					continue
				}
				// Path uses the embed: prefix + skill dir name so Load
				// can route back through embedFS. The dir name (not the
				// skill name) is the filesystem key.
				addSkill(hwcloud.SkillInfo{
					Name:        name,
					Description: desc,
					Frontmatter: fm,
					Path:        embedPathPrefix + entry.Name(),
				})
			}
		}
	}

	return skills, nil
}

// Load reads the SKILL.md for the given skill and returns the body
// (content after the closing YAML frontmatter). For embedded skills
// (Path prefixed with "embed:"), the read goes through the embedded
// fs.FS; for disk skills, through os.ReadFile.
func (l *Loader) Load(ctx context.Context, skill hwcloud.SkillInfo) (string, error) {
	if strings.HasPrefix(skill.Path, embedPathPrefix) {
		if l.embedFS == nil {
			return "", fmt.Errorf("embedded skill %q but no embedFS configured", skill.Name)
		}
		skillDir := strings.TrimPrefix(skill.Path, embedPathPrefix)
		mdPath := filepath.Join(skillDir, "SKILL.md")
		_, body, err := parseFrontmatterFS(l.embedFS, mdPath)
		return body, err
	}
	mdPath := filepath.Join(skill.Path, "SKILL.md")
	_, body, err := parseFrontmatter(mdPath)
	return body, err
}

// parseFrontmatter splits a disk file into YAML frontmatter (map) and body.
func parseFrontmatter(path string) (map[string]any, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	return splitFrontmatter(data)
}

// parseFrontmatterFS splits an embedded file into YAML frontmatter and body.
func parseFrontmatterFS(fsys fs.FS, path string) (map[string]any, string, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, "", err
	}
	return splitFrontmatter(data)
}

// splitFrontmatter parses raw SKILL.md bytes into YAML frontmatter (map) and
// the markdown body. Shared by the disk and embedded read paths so the
// parsing logic (CRLF normalization, closing-separator detection, YAML
// unmarshal) stays in one place.
func splitFrontmatter(data []byte) (map[string]any, string, error) {
	// Normalize CRLF → LF so that SKILL.md files with Windows line
	// endings are handled identically to Unix ones. This is an in-memory
	// operation; the file on disk is not modified. LF-only files are
	// unaffected (no \r\n sequences to replace).
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, "", fmt.Errorf("no frontmatter")
	}

	// Find closing --- on its own line
	idx := strings.Index(text[4:], "\n---\n")
	closeLen := 5 // "\n---\n"
	if idx == -1 {
		// Closing separator at EOF without a trailing newline ("...\n---").
		// The body after it is empty — slice to len(text), not beyond it.
		if strings.HasSuffix(text[4:], "\n---") {
			idx = len(text[4:]) - 4
			closeLen = 4 // "\n---"
		} else {
			return nil, "", fmt.Errorf("unclosed frontmatter")
		}
	}

	yamlBlock := text[4 : 4+idx]
	body := text[4+idx+closeLen:] // skip the closing separator

	var fm map[string]any
	if err := yaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return nil, "", fmt.Errorf("invalid YAML frontmatter: %w", err)
	}
	if fm == nil {
		fm = make(map[string]any)
	}

	return fm, body, nil
}
