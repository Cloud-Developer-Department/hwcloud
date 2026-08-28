// Package huaweicloud implements provider.CloudProvider for HuaweiCloud.
//
// Skills (terraform deployment guide + examples, pricing guide, troubleshoot
// guide) are embedded at compile time via go:embed (see embed.go). iac-server
// extracts them to disk at startup for the standard skill loader and
// read/grep/ls tools.
//
// Catalog is queried via terraform data sources at plan time.
// Pricing is queried by the server LLM via http_request (BSS API, auto-signed)
// with WebSearch/WebFetch as a fallback.
package huaweicloud

import (
	"io/fs"
	"os"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/mcp/iac-server/provider"
)

// DefaultProviderMirror is the HuaweiCloud community provider mirror.
// Used as the default TF_PROVIDER_MIRRORS entry so terraform init works
// on networks where registry.terraform.io is slow or unreachable; user
// configured mirrors are appended after it.
const DefaultProviderMirror = "https://mirrors.huaweicloud.com/terraform/"

// HuaweiCloud implements provider.CloudProvider.
type HuaweiCloud struct {
	region string
}

// New creates a HuaweiCloud provider for the given region.
// Credentials are read from the environment on demand via Env().
func New(region string) *HuaweiCloud {
	return &HuaweiCloud{region: region}
}

// Compile-time interface check.
var _ provider.CloudProvider = (*HuaweiCloud)(nil)

// Name returns the cloud identifier.
func (h *HuaweiCloud) Name() string { return "huaweicloud" }

// Env returns HuaweiCloud credential environment variables.
// Reads from the process environment at call time so secrets never
// persist in the struct.
func (h *HuaweiCloud) Env() map[string]string {
	env := map[string]string{
		"HW_ACCESS_KEY": os.Getenv("HW_ACCESS_KEY"),
		"HW_SECRET_KEY": os.Getenv("HW_SECRET_KEY"),
		"HW_REGION":     h.region,
	}
	if t := os.Getenv("HW_SECURITY_TOKEN"); t != "" {
		env["HW_SECURITY_TOKEN"] = t
	}
	return env
}

// Skills returns the embedded skills directory.
func (h *HuaweiCloud) Skills() fs.FS { return Skills() }

// Agents returns the cloud-specific agent prompts and static skill names.
func (h *HuaweiCloud) Agents() map[provider.PromptRole]provider.AgentConfig {
	return h.agents()
}

// ProviderSource returns the terraform provider source for HuaweiCloud.
func (h *HuaweiCloud) ProviderSource() string { return "huaweicloud/huaweicloud" }

// HTTPRequest returns an http_request tool configured with HuaweiCloud
// credentials from the environment. The tool handles SDK-HMAC-SHA256
// signing automatically — the LLM never sees AK/SK.
func (h *HuaweiCloud) HTTPRequest() hwcloud.Tool {
	return NewHTTPRequest(
		os.Getenv("HW_ACCESS_KEY"),
		os.Getenv("HW_SECRET_KEY"),
		os.Getenv("HW_SECURITY_TOKEN"), // temporary credentials; empty for permanent AK/SK
	)
}
