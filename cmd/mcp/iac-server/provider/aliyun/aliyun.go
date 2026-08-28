// Package aliyun implements provider.CloudProvider for Alibaba Cloud.
//
// This is a stub. It exists to remind us that iac-server is not coupled
// to HuaweiCloud — adding a cloud means implementing this interface +
// embedding a skills directory, nothing in server core or iac/ changes.
package aliyun

import (
	"io/fs"
	"os"

	"github.com/Cloud-Developer-Department/hwcloud/cmd/mcp/iac-server/provider"
)

// Aliyun implements provider.CloudProvider.
type Aliyun struct {
	region string
}

// New creates an Aliyun provider for the given region.
// Credentials are read from the environment on demand via Env().
func New(region string) *Aliyun {
	return &Aliyun{region: region}
}

// Compile-time interface check.
var _ provider.CloudProvider = (*Aliyun)(nil)

// Name returns the cloud identifier.
func (a *Aliyun) Name() string { return "aliyun" }

// Env returns Aliyun credential environment variables.
// Reads from the process environment at call time so secrets never
// persist in the struct.
func (a *Aliyun) Env() map[string]string {
	return map[string]string{
		"ALICLOUD_ACCESS_KEY": os.Getenv("ALICLOUD_ACCESS_KEY"),
		"ALICLOUD_SECRET_KEY": os.Getenv("ALICLOUD_SECRET_KEY"),
		"ALICLOUD_REGION":     a.region,
	}
}

// Skills returns the embedded skills directory.
// nil until skills are embedded for this cloud.
func (a *Aliyun) Skills() fs.FS { return nil }

// ProviderSource returns the terraform provider source for Alibaba Cloud.
func (a *Aliyun) ProviderSource() string { return "aliyun/alicloud" }

// Agents returns placeholder prompts for each agent role. Full prompts
// (API names, skill references) land with the skills directory.
func (a *Aliyun) Agents() map[provider.PromptRole]provider.AgentConfig {
	generic := map[provider.PromptRole]provider.AgentConfig{
		provider.RoleArchitect: {
			Prompt: "You are an Alibaba Cloud architecture expert. Recommend an architecture as a DAG (one node per resource, depends_on lists dependencies). Resource types are alicloud terraform resource types.",
		},
		provider.RoleSpecifier: {
			Prompt: "You are an Alibaba Cloud resource specification expert. Determine concrete specs for each DAG node. Use load_skill on the matching service skill to find API endpoints, then http_request (read-only) to query available specs. Your queries serve ONE purpose: picking specs for the DAG — you are NOT answering account questions. If choices are too many or info is missing, ask the user.",
		},
		provider.RolePlanner: {
			Prompt: "You are an Alibaba Cloud terraform configuration expert. Generate .tf files from the deployment DAG: one resource block per node (address = type.name), dependencies per depends_on. Follow the deployment skill guide's credential rules and naming conventions.",
		},
		provider.RolePricer: {
			Prompt: "You are an Alibaba Cloud pricing expert. Browse the pricing skill references/ for API definitions, use http_request (read-only) for price queries, WebSearch/WebFetch as fallback. The billing mode is given in your user message. Mark undeterminable prices as null — do NOT fabricate.",
		},
		provider.RoleTroubleshooter: {
			Prompt: "You are an Alibaba Cloud infrastructure troubleshooting expert. Compare the failed .tf files against correct patterns in the deployment skill references/, and search the web for solutions.",
		},
		provider.RoleQueryer: {
			Prompt: "You are an Alibaba Cloud cloud query expert. Use load_skill on the matching service skill, then http_request to query. CRITICAL: only read-only APIs (List/Show/Get) — never Create/Update/Delete.",
		},
	}
	return generic
}
