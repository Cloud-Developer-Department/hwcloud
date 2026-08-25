// Package skill defines the SkillProvider — intent-matched, on-demand
// skill loading. A skill is a capability (terraform deployment, kubernetes
// debug, docker build) whose full instructions load only when relevant,
// instead of dumping every skill catalog into every prompt.
//
// Provider is the single skill entry point: the Context Runtime asks
// Match(intent) which skills are relevant to the current goal and injects
// only those; the load_skill/reload_skills tools use Discover/Load.
// The legacy root-package SkillLoader was removed — Provider replaced it.
package skill

import (
	"context"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// Provider matches, discovers, and loads skills. A nil provider means no
// skills are available.
type Provider interface {
	// Match returns up to limit skills relevant to intent, best-effort
	// ranked (most relevant first).
	Match(ctx context.Context, intent string, limit int) ([]hwcloud.SkillInfo, error)

	// Discover returns the full skill catalog (frontmatter only).
	Discover(ctx context.Context) ([]hwcloud.SkillInfo, error)

	// Load returns the full skill instructions (SKILL.md body).
	Load(ctx context.Context, skill hwcloud.SkillInfo) (string, error)
}
