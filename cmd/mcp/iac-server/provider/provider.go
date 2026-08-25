// Package provider defines the CloudProvider interface for iac-server.
//
// A CloudProvider provides cloud identity, credentials, agent prompts,
// and an embedded skills directory (terraform deployment guide +
// examples, pricing guide, troubleshoot guide). The skills are compiled
// into the provider implementation via go:embed — iac-server extracts
// them to disk at startup and uses the standard skill/fs loader.
//
// Adding a cloud = implement this interface + embed a skills/ directory +
// provide agent prompts. Nothing in server core or iac/ changes.
package provider

import "io/fs"

// PromptRole identifies a server-side LLM agent role. Each role gets its
// cloud-specific system prompt via Agents(). The roles are type-safe so a
// cloud implementation cannot silently miss one (validated at startup).
type PromptRole string

const (
	RoleArchitect     PromptRole = "architect"     // propose_architecture
	RoleSpecifier     PromptRole = "specifier"     // specify_resources
	RolePlanner       PromptRole = "planner"       // generate_terraform_plan
	RolePricer        PromptRole = "pricer"        // estimate_cost
	RoleTroubleshooter PromptRole = "troubleshooter" // troubleshoot_deployment
	RoleQueryer       PromptRole = "queryer"       // query_cloud
)

// AllRoles lists every role the server requires a cloud to provide.
var AllRoles = []PromptRole{
	RoleArchitect, RoleSpecifier, RolePlanner,
	RolePricer, RoleTroubleshooter, RoleQueryer,
}

// AgentConfig is the cloud-specific configuration for one agent role.
// Prompt is the expert guidance injected into the agent's system prompt
// (identity, cloud-specific operations, examples — NOT the JSON output
// contract, which lives in the server core next to its parser). SkillName
// is the embedded skill statically loaded for this role (e.g.
// "huaweicloud-deploy"); empty means no static skill (the role loads
// skills dynamically via load_skill).
type AgentConfig struct {
	Prompt    string
	SkillName string
}

// CloudProvider abstracts a cloud provider for iac-server.
//
// Implementations embed their skills directory at compile time. Each
// subdirectory of Skills() is a skill (with SKILL.md + reference files).
// iac-server extracts Skills() to disk at startup for the skill loader
// and standard read/grep/ls tools.
//
// The JSON output contract for each role is owned by the server core
// (agent/planner.go) — it is parsed programmatically there. Agents()
// must provide the cloud-specific expertise only.
//
// Adding a cloud = implement this interface + embed skills + provide
// prompts. Nothing in server core or iac/ changes.
type CloudProvider interface {
	// Name returns the cloud identifier, e.g. "huaweicloud".
	Name() string

	// Env returns cloud credential environment variables for the
	// terraform subprocess. Reads from the process environment at
	// call time so secrets never persist in the struct.
	Env() map[string]string

	// Skills returns the embedded skills directory as an fs.FS.
	// Each subdirectory is a skill containing SKILL.md and optional
	// reference files (examples, guides, etc.).
	// nil means no skills are available for this cloud.
	Skills() fs.FS

	// Agents returns the cloud-specific prompt + static skill name for
	// each agent role. A role may be missing from the map; the server
	// rejects such clouds at startup.
	Agents() map[PromptRole]AgentConfig

	// ProviderSource is the terraform provider source for this cloud
	// (e.g. "huaweicloud/huaweicloud"), used to prewarm the provider
	// plugin cache at startup.
	ProviderSource() string
}
