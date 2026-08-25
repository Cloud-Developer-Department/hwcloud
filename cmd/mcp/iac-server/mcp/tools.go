// Package mcp defines the deployment tools exposed by iac-server over MCP.
//
// Tools are split into two groups:
//   - LLM tools (propose_architecture, specify_resources, generate_terraform_plan,
//     estimate_cost, troubleshoot_deployment, query_cloud): delegate to agent.Planner
//   - Execution tools (apply, destroy, get_status, list): call iac.Client
//     directly
//
// All tools return JSON strings. Server-side execution is unconditional —
// approval is the client's concern, not the server's.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/mcp/iac-server/agent"
	"github.com/Cloud-Developer-Department/hwcloud/cmd/mcp/iac-server/provider"
	"github.com/Cloud-Developer-Department/hwcloud/iac"
)

// Config holds shared dependencies for all tools.
type Config struct {
	Planner         *agent.Planner
	Cloud           provider.CloudProvider
	DeploymentsDir  string   // root dir for deployment workspaces
	DryRun          bool     // pass to iac.Config.DryRun
	BinaryMirrors   []string // terraform binary download mirrors
	ProviderMirrors []string // provider download mirrors (URLs or local paths)
}

// NewTools builds the 12 tools exposed by iac-server.
func NewTools(cfg Config) []hwcloud.Tool {
	return []hwcloud.Tool{
		&proposeArchitectureTool{cfg: cfg},
		&specifyResourcesTool{cfg: cfg},
		&generateTerraformPlanTool{cfg: cfg},
		&estimateCostTool{cfg: cfg},
		&troubleshootDeploymentTool{cfg: cfg},
		&applyDeploymentTool{cfg: cfg},
		&destroyDeploymentTool{cfg: cfg},
		&getDeploymentStatusTool{cfg: cfg},
		&listDeploymentsTool{cfg: cfg},
		&queryCloudTool{cfg: cfg},
		&updateDeploymentTool{cfg: cfg},
		&getJobResultTool{cfg: cfg},
	}
}

// jobStarted renders the immediate response of an async tool call: the
// client polls get_job_result with the returned job_id.
func jobStarted(jobID, tool string) *hwcloud.ToolResult {
	content := fmt.Sprintf(`{"job_id": %q, "status": "started", "tool": %q, "hint": "call get_job_result with job_id to poll for completion (wait_seconds up to 120 to block)"}`, jobID, tool)
	return &hwcloud.ToolResult{Content: content}
}

// workDir returns the workspace path for a deployment ID.
func workDir(deploymentsDir, deploymentID string) string {
	return filepath.Join(deploymentsDir, deploymentID)
}

// validDeploymentID reports whether id is a safe deployment identifier:
// non-empty, no path separators, no parent-dir components. This prevents
// deployment_id values like "../etc" from escaping deploymentsDir.
func validDeploymentID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, `/\`) {
		return false
	}
	// Reject any segment that is ".." after cleaning.
	cleaned := filepath.Clean(id)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// iacConfig builds an iac.Config with cloud credentials and mirror settings.
func iacConfig(cloud provider.CloudProvider, dryRun bool, binaryMirrors, providerMirrors []string) iac.Config {
	return iac.Config{
		Env:             cloud.Env(),
		DryRun:          dryRun,
		BinaryMirrors:   binaryMirrors,
		ProviderMirrors: providerMirrors,
	}
}

// ── propose_architecture ──

type proposeArchitectureTool struct{ cfg Config }

func (t *proposeArchitectureTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "propose_architecture",
		Description: "Step 1 of deployment: Analyze a deployment request and recommend a cloud architecture. Returns architecture name, required services, reasoning, and a deployment_id. Does NOT write .tf files. The user should confirm the architecture before calling specify_resources." + " ASYNC: returns immediately with a job_id — call get_job_result(job_id, wait_seconds=25) to poll for the result.",
		Parameters:  hwcloud.SchemaOf[ProposeArchitectureParams](),
	}
}

func (t *proposeArchitectureTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[ProposeArchitectureParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("propose_architecture: %w", err), false, "")
	}
	jobID, err := t.cfg.Planner.SubmitJob(ctx, "", "propose_architecture", func(ctx context.Context) (string, error) {
		return t.cfg.Planner.ProposeArchitecture(ctx, params.Request)
	})
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
	return jobStarted(jobID, "propose_architecture")
}

// ── specify_resources ──

type specifyResourcesTool struct{ cfg Config }

func (t *specifyResourcesTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "specify_resources",
		Description: "Step 2 of deployment: Determine concrete resource specs (flavor, image, disk, CIDR, etc.) for each node of the DAG from propose_architecture. Queries available specs via cloud APIs. If too many choices or missing info, returns status need_input with questions — call again with answers filled in. Optional adjustments let the user modify specs. The user should confirm the resources before calling generate_terraform_plan." + " ASYNC: returns immediately with a job_id — call get_job_result(job_id, wait_seconds=25) to poll for the result.",
		Parameters:  hwcloud.SchemaOf[SpecifyResourcesParams](),
	}
}

func (t *specifyResourcesTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[SpecifyResourcesParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("specify_resources: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return hwcloud.ErrorResult(fmt.Errorf("specify_resources: invalid deployment_id %q", params.DeploymentID), false, "")
	}
	jobID, err := t.cfg.Planner.SubmitJob(ctx, params.DeploymentID, "specify_resources", func(ctx context.Context) (string, error) {
		return t.cfg.Planner.SpecifyResources(ctx, params.DeploymentID, params.Answers, params.Adjustments)
	})
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
	return jobStarted(jobID, "specify_resources")
}

// ── generate_terraform_plan ──

type generateTerraformPlanTool struct{ cfg Config }

func (t *generateTerraformPlanTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "generate_terraform_plan",
		Description: "Step 3 of deployment: Write .tf files from the deployment DAG (nodes = resources with confirmed specs, edges = dependencies), then run terraform init + plan. Returns the .tf files and a plan preview. Requires specify_resources to have completed. The user should review the plan before calling estimate_cost." + " ASYNC: returns immediately with a job_id — call get_job_result(job_id, wait_seconds=25) to poll for the result.",
		Parameters:  hwcloud.SchemaOf[GenerateTerraformPlanParams](),
	}
}

func (t *generateTerraformPlanTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[GenerateTerraformPlanParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("generate_terraform_plan: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return hwcloud.ErrorResult(fmt.Errorf("generate_terraform_plan: invalid deployment_id %q", params.DeploymentID), false, "")
	}
	jobID, err := t.cfg.Planner.SubmitJob(ctx, params.DeploymentID, "generate_terraform_plan", func(ctx context.Context) (string, error) {
		return t.cfg.Planner.GenerateTerraformPlan(ctx, params.DeploymentID)
	})
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
	return jobStarted(jobID, "generate_terraform_plan")
}

// ── update_deployment ──

type updateDeploymentTool struct{ cfg Config }

func (t *updateDeploymentTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "update_deployment",
		Description: "Modify an existing deployment. Re-runs specify_resources (with user answers/adjustments) and generate_terraform_plan. Use this when the user wants to adjust an existing deployment (e.g. \"change ECS flavor to s6.xlarge.2\"). Returns the updated plan with the same deployment_id. The previous cost estimate is invalidated — call estimate_cost again before apply_deployment." + " ASYNC: returns immediately with a job_id — call get_job_result(job_id, wait_seconds=25) to poll for the result.",
		Parameters:  hwcloud.SchemaOf[UpdateDeploymentParams](),
	}
}

func (t *updateDeploymentTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[UpdateDeploymentParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("update_deployment: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return hwcloud.ErrorResult(fmt.Errorf("update_deployment: invalid deployment_id %q", params.DeploymentID), false, "")
	}
	jobID, err := t.cfg.Planner.SubmitJob(ctx, params.DeploymentID, "update_deployment", func(ctx context.Context) (string, error) {
		return t.cfg.Planner.UpdateDeployment(ctx, params.DeploymentID, params.Answers, params.ChangeRequest)
	})
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
	return jobStarted(jobID, "update_deployment")
}

// ── estimate_cost ──

type estimateCostTool struct{ cfg Config }

func (t *estimateCostTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "estimate_cost",
		Description: "Step 4 of deployment: Estimate the cost of a PLANNED deployment (resources not yet created) from the deployment DAG. MUST be called after generate_terraform_plan (and after any update_deployment) and before apply_deployment — apply is rejected without it. pricing_mode: \"on-demand\" (按需) or \"monthly\" (包月); if the user did not state a preference, omit it. This forecasts FUTURE costs — it does NOT query past billing. For existing bills/costs, use query_cloud." + " ASYNC: returns immediately with a job_id — call get_job_result(job_id, wait_seconds=25) to poll for the result.",
		Parameters:  hwcloud.SchemaOf[EstimateCostParams](),
	}
}

func (t *estimateCostTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[EstimateCostParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("estimate_cost: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return hwcloud.ErrorResult(fmt.Errorf("estimate_cost: invalid deployment_id %q", params.DeploymentID), false, "")
	}
	jobID, err := t.cfg.Planner.SubmitJob(ctx, params.DeploymentID, "estimate_cost", func(ctx context.Context) (string, error) {
		return t.cfg.Planner.EstimateCost(ctx, params.DeploymentID, params.PricingMode)
	})
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
	return jobStarted(jobID, "estimate_cost")
}

// ── troubleshoot_deployment ──

type troubleshootDeploymentTool struct{ cfg Config }

func (t *troubleshootDeploymentTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "troubleshoot_deployment",
		Description: "Diagnose a deployment error and suggest fixes. Reads the .tf files and error message, researches solutions via examples and web search, and returns a diagnosis with recommended actions." + " ASYNC: returns immediately with a job_id — call get_job_result(job_id, wait_seconds=25) to poll for the result.",
		Parameters:  hwcloud.SchemaOf[TroubleshootParams](),
	}
}

func (t *troubleshootDeploymentTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[TroubleshootParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("troubleshoot_deployment: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return hwcloud.ErrorResult(fmt.Errorf("troubleshoot_deployment: invalid deployment_id %q", params.DeploymentID), false, "")
	}
	jobID, err := t.cfg.Planner.SubmitJob(ctx, params.DeploymentID, "troubleshoot_deployment", func(ctx context.Context) (string, error) {
		return t.cfg.Planner.Troubleshoot(ctx, params.DeploymentID, params.Error)
	})
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
	return jobStarted(jobID, "troubleshoot_deployment")
}

// ── apply_deployment ──

type applyDeploymentTool struct{ cfg Config }

func (t *applyDeploymentTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "apply_deployment",
		Description: "Step 5 of deployment: Apply a saved terraform plan. This creates/modifies real cloud resources. The deployment must have been planned (generate_terraform_plan succeeded) AND cost-estimated (estimate_cost succeeded) first — apply is rejected with an error if the deployment has no current cost estimate." + " ASYNC: returns immediately with a job_id — call get_job_result(job_id, wait_seconds=25) to poll for the result.",
		Parameters:  hwcloud.SchemaOf[ApplyDeploymentParams](),
	}
}

func (t *applyDeploymentTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[ApplyDeploymentParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("apply_deployment: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return hwcloud.ErrorResult(fmt.Errorf("apply_deployment: invalid deployment_id %q", params.DeploymentID), false, "")
	}

	jobID, err := t.cfg.Planner.SubmitJob(ctx, params.DeploymentID, "apply_deployment", func(ctx context.Context) (string, error) {
		dir := workDir(t.cfg.DeploymentsDir, params.DeploymentID)

		// State gate: apply requires a plan that was actually generated and
		// validated by terraform plan, OR a deployment that was already applied
		// (idempotent re-apply). A stale cost.json from a previous estimate
		// (before a failed re-generate) must not let apply through — the
		// DagSpecified status from a failed generate rollback blocks that.
		status := agent.DagStatusOf(dir)
		switch status {
		case agent.DagPlanned, agent.DagCostEstimated:
			// ok — has a valid plan or a cost-estimated plan
		case agent.DagApplied:
			// ok — idempotent re-apply; terraform apply is a no-op on a
			// converged state, so allowing this preserves apply idempotency.
		case agent.DagDestroyed:
			return "", fmt.Errorf("apply_deployment: deployment %s was already destroyed — call generate_terraform_plan to re-create it", params.DeploymentID)
		default:
			return "", fmt.Errorf("apply_deployment: deployment %s is not in a plannable state (dag.Status=%q) — call generate_terraform_plan first", params.DeploymentID, status)
		}

		// Cost gate: apply is only allowed after estimate_cost ran for the
		// current deployment state (any DAG/.tf mutation invalidates the marker).
		if !agent.HasCost(dir) {
			return "", fmt.Errorf("apply_deployment: deployment %s has not been cost-estimated for its current state — call estimate_cost first", params.DeploymentID)
		}

		client, err := iac.NewClient(ctx, dir, iacConfig(t.cfg.Cloud, t.cfg.DryRun, t.cfg.BinaryMirrors, t.cfg.ProviderMirrors))
		if err != nil {
			return "", fmt.Errorf("apply_deployment: %w", err)
		}

		result, err := client.Apply(ctx)
		if err != nil {
			// If the job was cancelled (superseded by a newer submission or
			// the 15-min timeout fired), terraform received SIGINT and may
			// have left partial resources / a stale state lock. Tell the
			// client explicitly so it uses troubleshoot_deployment instead
			// of blindly retrying.
			if ctx.Err() != nil {
				return "", fmt.Errorf("apply_deployment: interrupted (ctx cancelled: %v) — terraform may have left partial resources or a state lock; run troubleshoot_deployment to check the real state before retrying: %w", ctx.Err(), err)
			}
			return "", fmt.Errorf("apply_deployment: %w", err)
		}

		// Advance the DAG state machine to DagApplied so a subsequent
		// apply_deployment is treated as an idempotent re-apply (the state
		// gate accepts DagApplied) rather than an error. A save failure here
		// does not undo the apply — the resources exist on the cloud; only
		// the persisted contract is stale, and the client can troubleshoot.
		_ = agent.SetDagStatus(dir, agent.DagApplied)

		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("apply_deployment: marshal: %w", err)
		}
		return string(data), nil
	})
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
	return jobStarted(jobID, "apply_deployment")
}

// ── destroy_deployment ──

type destroyDeploymentTool struct{ cfg Config }

func (t *destroyDeploymentTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "destroy_deployment",
		Description: "Destroy all resources in a deployment. This permanently deletes cloud resources. Use with caution." + " ASYNC: returns immediately with a job_id — call get_job_result(job_id, wait_seconds=25) to poll for the result.",
		Parameters:  hwcloud.SchemaOf[DestroyDeploymentParams](),
	}
}

func (t *destroyDeploymentTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[DestroyDeploymentParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("destroy_deployment: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return hwcloud.ErrorResult(fmt.Errorf("destroy_deployment: invalid deployment_id %q", params.DeploymentID), false, "")
	}

	jobID, err := t.cfg.Planner.SubmitJob(ctx, params.DeploymentID, "destroy_deployment", func(ctx context.Context) (string, error) {
		dir := workDir(t.cfg.DeploymentsDir, params.DeploymentID)
		// Refuse to "destroy" a deployment that was never created. terraform
		// destroy on an empty/missing directory succeeds as a no-op and would
		// report {"destroyed": true} — misleading when the id is bogus. Require
		// the deployment directory to exist (it is created by propose_architecture).
		if _, err := os.Stat(dir); err != nil {
			if os.IsNotExist(err) {
				return "", fmt.Errorf("destroy_deployment: deployment %s does not exist — check list_deployments", params.DeploymentID)
			}
			return "", fmt.Errorf("destroy_deployment: %w", err)
		}
		// State gate: destroy only makes sense for a deployment that was
		// actually applied (or at least planned/cost-estimated). Destroying
		// a proposed/specified deployment (never applied) would run terraform
		// destroy on a state with no resources — a no-op reported as success,
		// which is misleading. An already-destroyed deployment is rejected
		// so the client doesn't think it destroyed something that wasn't there.
		switch status := agent.DagStatusOf(dir); status {
		case agent.DagApplied, agent.DagPlanned, agent.DagCostEstimated:
			// ok — has resources (applied) or a plan that may have partial resources
		case agent.DagDestroyed:
			return "", fmt.Errorf("destroy_deployment: deployment %s was already destroyed", params.DeploymentID)
		case "":
			// Pre-DAG deployment (no dag.json) — allow for backward compat.
		default:
			return "", fmt.Errorf("destroy_deployment: deployment %s was never applied (dag.Status=%q) — nothing to destroy", params.DeploymentID, status)
		}
		client, err := iac.NewClient(ctx, dir, iacConfig(t.cfg.Cloud, t.cfg.DryRun, t.cfg.BinaryMirrors, t.cfg.ProviderMirrors))
		if err != nil {
			return "", fmt.Errorf("destroy_deployment: %w", err)
		}

		resources, err := client.Destroy(ctx)
		if err != nil {
			// Same interruption guidance as apply — a cancelled destroy may
			// leave resources half-destroyed or a state lock behind.
			if ctx.Err() != nil {
				return "", fmt.Errorf("destroy_deployment: interrupted (ctx cancelled: %v) — terraform may have left resources partially destroyed or a state lock; run troubleshoot_deployment to check the real state before retrying: %w", ctx.Err(), err)
			}
			return "", fmt.Errorf("destroy_deployment: %w", err)
		}

		result := map[string]any{
			"destroyed": true,
			"resources": resources,
		}

		// Advance the DAG state machine to DagDestroyed.
		_ = agent.SetDagStatus(dir, agent.DagDestroyed)

		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("destroy_deployment: marshal: %w", err)
		}
		return string(data), nil
	})
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
	return jobStarted(jobID, "destroy_deployment")
}

// ── get_deployment_status ──

type getDeploymentStatusTool struct{ cfg Config }

func (t *getDeploymentStatusTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "get_deployment_status",
		Description: "Read the terraform state for a deployment and return a status summary. Does not call terraform binary — reads the state file directly.",
		Parameters:  hwcloud.SchemaOf[GetDeploymentStatusParams](),
	}
}

func (t *getDeploymentStatusTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[GetDeploymentStatusParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("get_deployment_status: %w", err), false, "")
	}
	if !validDeploymentID(params.DeploymentID) {
		return hwcloud.ErrorResult(fmt.Errorf("get_deployment_status: invalid deployment_id %q", params.DeploymentID), false, "")
	}

	dir := workDir(t.cfg.DeploymentsDir, params.DeploymentID)
	statePath := filepath.Join(dir, "terraform.tfstate")

	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return hwcloud.ErrorResult(fmt.Errorf("get_deployment_status: no state file for deployment %s — has it been planned/applied?", params.DeploymentID), false, "")
		}
		return hwcloud.ErrorResult(fmt.Errorf("get_deployment_status: %w", err), false, "")
	}

	// Parse the state file to extract a summary.
	// terraform state v4 resources have no "address" field — the address is
	// implicitly "<type>.<name>". We read type+name and synthesize address so
	// the client gets a usable identifier for each resource.
	var state struct {
		Resources []struct {
			Address string `json:"address"` // absent in state v4; kept for forward-compat
			Type    string `json:"type"`
			Name    string `json:"name"`
		} `json:"resources"`
		Outputs map[string]struct {
			Value any `json:"value"`
			Type  any `json:"type"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("get_deployment_status: parse state: %w", err), false, "")
	}

	// Build resource summaries with a non-empty address (type.name when the
	// state file omits it).
	resources := make([]map[string]string, 0, len(state.Resources))
	for _, r := range state.Resources {
		addr := r.Address
		if addr == "" && r.Type != "" && r.Name != "" {
			addr = r.Type + "." + r.Name
		}
		resources = append(resources, map[string]string{
			"address": addr,
			"type":    r.Type,
			"name":    r.Name,
		})
	}

	summary := map[string]any{
		"deployment_id":  params.DeploymentID,
		"resource_count": len(state.Resources),
		"resources":      resources,
		"outputs":        state.Outputs,
	}
	result, err := json.Marshal(summary)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("get_deployment_status: marshal: %w", err), false, "")
	}
	return &hwcloud.ToolResult{Content: string(result)}
}

// ── query_cloud ──

type queryCloudTool struct{ cfg Config }

func (t *queryCloudTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "query_cloud",
		Description: "Query EXISTING cloud resources, specs, bills, costs, or quotas. Use this for any read-only query about the current cloud account state — e.g. \"list all ECS instances\", \"what specs does s6.large.2 have\", \"how much did I spend this month\", \"show my bills for 2025-07\". This queries real cloud APIs for already-existing resources and past billing data. Does NOT modify any resources. For estimating FUTURE costs of a planned deployment, use estimate_cost." + " ASYNC: returns immediately with a job_id — call get_job_result(job_id, wait_seconds=25) to poll for the result.",
		Parameters:  hwcloud.SchemaOf[QueryCloudParams](),
	}
}

func (t *queryCloudTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[QueryCloudParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("query_cloud: %w", err), false, "")
	}
	jobID, err := t.cfg.Planner.SubmitJob(ctx, "", "query_cloud", func(ctx context.Context) (string, error) {
		return t.cfg.Planner.QueryCloud(ctx, params.Query)
	})
	if err != nil {
		return hwcloud.ErrorResult(err, false, "")
	}
	return jobStarted(jobID, "query_cloud")
}

// ── get_job_result ──

type getJobResultTool struct{ cfg Config }

func (t *getJobResultTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name: "get_job_result",
		Description: "Poll the result of an async job started by propose_architecture, specify_resources, generate_terraform_plan, " +
			"update_deployment, estimate_cost, troubleshoot_deployment, or query_cloud. " +
			"Those tools return immediately with a job_id; call this with that job_id to retrieve the outcome. " +
			"Returns {status: \"running\"|\"done\"|\"failed\", progress_msg, progress_cur, progress_tot, outputs, result, error}. " +
			"wait_seconds (0-120) blocks up to that long and returns as soon as the job finishes — " +
			"pass a value comfortably below your client's tool-call timeout to poll efficiently. " +
			"IMPORTANT: when status is \"running\" and progress_msg is non-empty, you MUST tell the user the current progress " +
			"(progress_msg, progress_cur/progress_tot) before calling get_job_result again. Do not poll silently — the user " +
			"should see what the server is doing while they wait.",
		Parameters: hwcloud.SchemaOf[GetJobResultParams](),
	}
}

func (t *getJobResultTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[GetJobResultParams](args)
	if err != nil {
		// Some client LLMs send wait_seconds as a string ("60") instead of an
		// int (60). json.Unmarshal rejects that, so retry with a manual
		// coercion before giving up — the field is otherwise valid.
		params, err = parseJobResultParamsLenient(args)
		if err != nil {
			return hwcloud.ErrorResult(fmt.Errorf("get_job_result: %w", err), false, "")
		}
	}
	wait := time.Duration(params.WaitSeconds) * time.Second
	if wait < 0 || wait > 120*time.Second {
		return hwcloud.ErrorResult(fmt.Errorf("get_job_result: wait_seconds must be 0-120"), false, "")
	}
	job, err := t.cfg.Planner.GetJob(ctx, params.JobID, wait)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("get_job_result: %w", err), false, "")
	}
	if job == nil {
		return hwcloud.ErrorResult(fmt.Errorf("get_job_result: unknown job %q", params.JobID), false, "")
	}
	data, err := json.Marshal(job)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("get_job_result: marshal: %w", err), false, "")
	}
	return &hwcloud.ToolResult{Content: string(data), JSON: data}
}

// ── list_deployments ──

type listDeploymentsTool struct{ cfg Config }

func (t *listDeploymentsTool) Definition() hwcloud.FunctionDefinition {
	return hwcloud.FunctionDefinition{
		Name:        "list_deployments",
		Description: "List all deployments by scanning the deployments directory. Returns deployment IDs and whether each has a state file.",
		Parameters:  hwcloud.SchemaOf[struct{}](),
	}
}

func (t *listDeploymentsTool) Execute(ctx context.Context, _ json.RawMessage) *hwcloud.ToolResult {
	entries, err := os.ReadDir(t.cfg.DeploymentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &hwcloud.ToolResult{Content: "[]"} // no deployments yet
		}
		return hwcloud.ErrorResult(fmt.Errorf("list_deployments: %w", err), false, "")
	}

	type deployment struct {
		ID       string `json:"id"`
		HasState bool   `json:"has_state"`
		HasPlan  bool   `json:"has_plan"`
	}

	// Start as a non-nil slice so an empty deployments dir marshals to []
	// (not null) — clients that unmarshal into []T choke on null.
	deployments := make([]deployment, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(t.cfg.DeploymentsDir, entry.Name())
		_, stateErr := os.Stat(filepath.Join(dir, "terraform.tfstate"))
		_, planErr := os.Stat(filepath.Join(dir, "tfplan"))
		deployments = append(deployments, deployment{
			ID:       entry.Name(),
			HasState: stateErr == nil,
			HasPlan:  planErr == nil,
		})
	}

	// Sort by ID for stable output.
	sort.Slice(deployments, func(i, j int) bool {
		return deployments[i].ID < deployments[j].ID
	})

	data, err := json.Marshal(deployments)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("list_deployments: marshal: %w", err), false, "")
	}
	return &hwcloud.ToolResult{Content: string(data), JSON: data}
}

// ── Tool parameter schemas ──
//
// Definitions and parsing share these types (SchemaOf generates the
// schema, ParseArgs parses the arguments), so the schema the model sees
// always matches what Execute accepts.

// ProposeArchitectureParams are the arguments to propose_architecture.
type ProposeArchitectureParams struct {
	Request string `json:"request" jsonschema:"description=Free-text deployment request, e.g. deploy a WordPress site to cn-east-3, single instance, budget 100/month"`
}

// SpecifyResourcesParams are the arguments to specify_resources.
type SpecifyResourcesParams struct {
	DeploymentID string   `json:"deployment_id" jsonschema:"description=Deployment ID from propose_architecture"`
	Answers      []string `json:"answers,omitempty" jsonschema:"description=Answers to the questions returned by a previous specify_resources call, one per question in order"`
	Adjustments  string   `json:"adjustments,omitempty" jsonschema:"description=Optional free-text adjustments, e.g. use s6.xlarge.2 instead or add a 100GB data disk"`
}

// GenerateTerraformPlanParams are the arguments to generate_terraform_plan.
type GenerateTerraformPlanParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID from propose_architecture"`
}

// UpdateDeploymentParams are the arguments to update_deployment.
type UpdateDeploymentParams struct {
	DeploymentID  string   `json:"deployment_id" jsonschema:"description=Deployment ID to update"`
	Answers       []string `json:"answers,omitempty" jsonschema:"description=Answers to the questions returned by a previous specify_resources call, one per question in order"`
	ChangeRequest string   `json:"change_request" jsonschema:"description=Free-text change request, e.g. change ECS flavor to s6.xlarge.2 or rename vpc.test to vpc.main"`
}

// EstimateCostParams are the arguments to estimate_cost.
type EstimateCostParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID from generate_terraform_plan"`
	PricingMode  string `json:"pricing_mode,omitempty" jsonschema:"description=Billing mode the user asked for: \"on-demand\" (按需/pay-as-you-go) or \"monthly\" (包月). Omit to default to on-demand. Pass the user's stated preference verbatim."`
}

// TroubleshootParams are the arguments to troubleshoot_deployment.
type TroubleshootParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID to troubleshoot"`
	Error        string `json:"error" jsonschema:"description=The error message from the failed operation"`
}

// ApplyDeploymentParams are the arguments to apply_deployment.
type ApplyDeploymentParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID to apply"`
}

// DestroyDeploymentParams are the arguments to destroy_deployment.
type DestroyDeploymentParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID to destroy"`
}

// GetDeploymentStatusParams are the arguments to get_deployment_status.
type GetDeploymentStatusParams struct {
	DeploymentID string `json:"deployment_id" jsonschema:"description=Deployment ID to check"`
}

// GetJobResultParams are the arguments to get_job_result.
type GetJobResultParams struct {
	JobID       string `json:"job_id" jsonschema:"description=Job ID returned by an async tool call"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"description=Block up to this many seconds (0-120) and return as soon as the job finishes; 0 returns immediately"`
}

// parseJobResultParamsLenient parses get_job_result args, tolerating a string
// wait_seconds (some client LLMs emit "60" instead of 60). It unmarshals into
// a raw map, coerces wait_seconds if needed, then re-marshals and parses
// normally.
func parseJobResultParamsLenient(args json.RawMessage) (GetJobResultParams, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(args, &raw); err != nil {
		return GetJobResultParams{}, err
	}
	if ws, ok := raw["wait_seconds"]; ok {
		var s string
		if err := json.Unmarshal(ws, &s); err == nil {
			n, err := strconv.Atoi(s)
			if err != nil {
				return GetJobResultParams{}, fmt.Errorf("get_job_result: wait_seconds string %q is not a valid int", s)
			}
			raw["wait_seconds"], _ = json.Marshal(n)
		}
	}
	fixed, err := json.Marshal(raw)
	if err != nil {
		return GetJobResultParams{}, err
	}
	var p GetJobResultParams
	if err := json.Unmarshal(fixed, &p); err != nil {
		return GetJobResultParams{}, err
	}
	return p, nil
}

// QueryCloudParams are the arguments to query_cloud.
type QueryCloudParams struct {
	Query string `json:"query" jsonschema:"description=Natural language query, e.g. list all ECS instances in cn-east-3 or how much did I spend this month"`
}
