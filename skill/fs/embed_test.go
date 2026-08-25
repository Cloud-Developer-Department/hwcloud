package fs

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// writeDiskSkill creates a skill directory on disk with a SKILL.md.
func writeDiskSkill(t *testing.T, root, name, desc, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeEmbedFS builds a fstest.MapFS with one skill.
func makeEmbedFS(name, desc, body string) fs.FS {
	return fstest.MapFS{
		name + "/SKILL.md": &fstest.MapFile{
			Data: []byte("---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body),
			Mode: 0o644,
		},
	}
}

// TestLoaderEmbedDiscoversEmbeddedSkills verifies that a Loader with an
// embedFS discovers skills from the embedded filesystem.
func TestLoaderEmbedDiscoversEmbeddedSkills(t *testing.T) {
	embedFS := makeEmbedFS("embedded-skill", "an embedded test skill", "# Embedded\n\nBody.")
	loader := New().WithEmbedFS(embedFS)

	skills, err := loader.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Name != "embedded-skill" {
		t.Fatalf("name = %q, want embedded-skill", skills[0].Name)
	}
	if skills[0].Description != "an embedded test skill" {
		t.Fatalf("desc = %q", skills[0].Description)
	}
	if skills[0].Path != "embed:embedded-skill" {
		t.Fatalf("Path = %q, want embed:embedded-skill", skills[0].Path)
	}
}

// TestLoaderEmbedLoad reads the body of an embedded skill via Load.
func TestLoaderEmbedLoad(t *testing.T) {
	loader := New().WithEmbedFS(makeEmbedFS("embedded-skill", "desc", "# Embedded\n\nHello body."))

	skills, err := loader.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	body, err := loader.Load(context.Background(), skills[0])
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if body != "# Embedded\n\nHello body." {
		t.Fatalf("body = %q", body)
	}
}

// TestLoaderDiskOverridesEmbed verifies that a disk skill with the same name
// as an embedded skill takes priority (disk roots scanned first).
func TestLoaderDiskOverridesEmbed(t *testing.T) {
	diskDir := t.TempDir()
	// Disk skill: "shared-skill" with disk-specific body.
	writeDiskSkill(t, diskDir, "shared-skill", "disk desc", "DISK BODY")
	// Also add a disk-only and an embed-only skill to verify both sources contribute.
	writeDiskSkill(t, diskDir, "disk-only", "disk only", "D")
	embedOnlyFS := fstest.MapFS{
		"shared-skill/SKILL.md": {
			Data: []byte("---\nname: shared-skill\ndescription: embed desc\n---\nEMBED BODY"),
			Mode: 0o644,
		},
		"embed-only/SKILL.md": {
			Data: []byte("---\nname: embed-only\ndescription: embed only\n---\nE"),
			Mode: 0o644,
		},
	}

	loader := New(diskDir).WithEmbedFS(embedOnlyFS)
	skills, err := loader.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(skills) != 3 {
		t.Fatalf("expected 3 skills (shared + disk-only + embed-only), got %d: %+v", len(skills), skillNames(skills))
	}

	byName := make(map[string]hwcloud.SkillInfo, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}

	// shared-skill should be the disk version (disk overrides embed).
	shared, ok := byName["shared-skill"]
	if !ok {
		t.Fatal("shared-skill not found")
	}
	if shared.Description != "disk desc" {
		t.Fatalf("shared-skill desc = %q, want disk desc (disk should override embed)", shared.Description)
	}
	body, err := loader.Load(context.Background(), shared)
	if err != nil {
		t.Fatalf("Load shared-skill: %v", err)
	}
	if body != "DISK BODY" {
		t.Fatalf("shared-skill body = %q, want DISK BODY", body)
	}

	// embed-only should be present from the embed source.
	if _, ok := byName["embed-only"]; !ok {
		t.Fatal("embed-only skill not found")
	}
	// disk-only should be present from the disk root.
	if _, ok := byName["disk-only"]; !ok {
		t.Fatal("disk-only skill not found")
	}
}

// TestLoaderNoEmbedFS verifies that a Loader without WithEmbedFS behaves
// exactly as before (disk-only, no embed: paths).
func TestLoaderNoEmbedFS(t *testing.T) {
	diskDir := t.TempDir()
	writeDiskSkill(t, diskDir, "disk-skill", "desc", "body")
	loader := New(diskDir)

	skills, err := loader.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "disk-skill" {
		t.Fatalf("expected 1 disk-skill, got %+v", skillNames(skills))
	}
}

func skillNames(skills []hwcloud.SkillInfo) []string {
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Name
	}
	return names
}
