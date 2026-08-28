package skill

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cloud-Developer-Department/hwcloud/skill/fs"
)

// TestFSBridge_MatchRanksRelevantSkills verifies intent-matched skill
// selection: the skill whose name/description overlaps the intent ranks
// first, and unrelated skills are excluded.
func TestFSBridge_MatchRanksRelevantSkills(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"terraform-deploy", "kubernetes-debug", "docker-build"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			t.Fatal(err)
		}
		md := "---\nname: " + d + "\ndescription: " + d + " playbook\n---\n# " + d + "\n\nInstructions.\n"
		if err := os.WriteFile(filepath.Join(dir, d, "SKILL.md"), []byte(md), 0644); err != nil {
			t.Fatal(err)
		}
	}

	loader := fs.New(dir)
	b := NewFSBridge(loader)

	skills, err := b.Match(context.Background(), "deploy infrastructure with terraform", 5)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if len(skills) == 0 {
		t.Fatal("expected matched skills, got none")
	}
	if skills[0].Name != "terraform-deploy" {
		t.Fatalf("top match = %q, want terraform-deploy", skills[0].Name)
	}
	for _, s := range skills {
		if s.Name == "docker-build" {
			t.Fatal("docker-build should not match a terraform intent")
		}
	}
}
