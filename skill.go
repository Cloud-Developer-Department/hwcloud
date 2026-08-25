package hwcloud

// SkillInfo is the lightweight summary of a skill, produced by Discover.
// Name and Description are extracted from YAML frontmatter; Frontmatter
// retains ALL fields (known and unknown). Path is the absolute path to
// the skill directory. The full SKILL.md body is loaded on demand via
// provider/skill.Provider.Load.
//
// SkillInfo lives in the root package (core layer) as a shared data model:
// it is consumed across provider/skill, context, kernel, execution, and
// apps — like Message/ToolResult. The skill capability (Provider + FS
// backend) lives in provider/skill; data models shared by many consumers
// stay at the DAG leaf (single-consumer models follow their consumer,
// e.g. MemoryItem in context).
type SkillInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Frontmatter map[string]any `json:"frontmatter"`
	Path        string         `json:"path"` // absolute path to skill directory
}
