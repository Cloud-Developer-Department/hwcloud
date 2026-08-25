package iac

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
)

// Init runs `terraform init` to download providers and modules.
func (c *Client) Init(ctx context.Context) error {
	if c.dryRun {
		return nil
	}
	if err := c.ensureTF(); err != nil {
		return err
	}
	return c.tf.Init(ctx, tfexec.Upgrade(false))
}

// Validate runs `terraform validate` and returns structured results.
func (c *Client) Validate(ctx context.Context) (*ValidateResult, error) {
	if c.dryRun {
		return &ValidateResult{Valid: true}, nil
	}
	if err := c.ensureTF(); err != nil {
		return nil, err
	}

	result, err := c.tf.Validate(ctx)
	if err != nil {
		return nil, fmt.Errorf("terraform validate: %w", err)
	}

	vr := &ValidateResult{Valid: result.Valid}
	for _, d := range result.Diagnostics {
		switch d.Severity {
		case tfjson.DiagnosticSeverityError:
			vr.Errors = append(vr.Errors, d.Summary)
		case tfjson.DiagnosticSeverityWarning:
			vr.Warnings = append(vr.Warnings, d.Summary)
		}
	}
	return vr, nil
}

// Format runs `terraform fmt`. When write is true, files are modified
// in place. When write is false, returns the list of unformatted files.
func (c *Client) Format(ctx context.Context, write bool) ([]string, error) {
	if c.dryRun {
		return nil, nil
	}
	if err := c.ensureTF(); err != nil {
		return nil, err
	}

	if write {
		if err := c.tf.FormatWrite(ctx, tfexec.Recursive(true)); err != nil {
			return nil, fmt.Errorf("terraform fmt: %w", err)
		}
		return nil, nil
	}

	// Check mode: return list of files that need formatting.
	_, paths, err := c.tf.FormatCheck(ctx, tfexec.Recursive(true))
	if err != nil {
		return nil, fmt.Errorf("terraform fmt -check: %w", err)
	}
	return paths, nil
}

// Plan runs `terraform plan -out=tfplan`, saves the plan to a file,
// and returns the structured plan.
func (c *Client) Plan(ctx context.Context) (*Plan, error) {
	if c.dryRun {
		return c.simulatedPlan(), nil
	}
	if err := c.ensureTF(); err != nil {
		return nil, err
	}

	// Generate plan and save to tfplan file.
	_, err := c.tf.Plan(ctx, tfexec.Out("tfplan"))
	if err != nil {
		return nil, fmt.Errorf("terraform plan: %w", err)
	}

	return c.ShowPlan(ctx)
}

// ShowPlan reads the saved tfplan file and returns the structured plan.
// Use after Plan() to inspect the plan without regenerating it.
func (c *Client) ShowPlan(ctx context.Context) (*Plan, error) {
	if c.dryRun {
		return c.simulatedPlan(), nil
	}
	if err := c.ensureTF(); err != nil {
		return nil, err
	}

	plan, err := c.tf.ShowPlanFile(ctx, "tfplan")
	if err != nil {
		return nil, fmt.Errorf("terraform show: %w", err)
	}

	return planFromJSON(plan), nil
}

// PlanDestroy runs `terraform plan -destroy -out=tfdestroy`, generating a
// destroy plan and saving it to a file. Use before Destroy() to preview what
// will be removed. Call ShowDestroyPlan to inspect the result.
func (c *Client) PlanDestroy(ctx context.Context) (*Plan, error) {
	if c.dryRun {
		return c.simulatedDestroyPlan(), nil
	}
	if err := c.ensureTF(); err != nil {
		return nil, err
	}

	_, err := c.tf.Plan(ctx, tfexec.Destroy(true), tfexec.Out("tfdestroy"))
	if err != nil {
		return nil, fmt.Errorf("terraform plan -destroy: %w", err)
	}

	return c.ShowDestroyPlan(ctx)
}

// ShowDestroyPlan reads the saved tfdestroy file and returns the structured plan.
func (c *Client) ShowDestroyPlan(ctx context.Context) (*Plan, error) {
	if c.dryRun {
		return c.simulatedDestroyPlan(), nil
	}
	if err := c.ensureTF(); err != nil {
		return nil, err
	}

	plan, err := c.tf.ShowPlanFile(ctx, "tfdestroy")
	if err != nil {
		return nil, fmt.Errorf("terraform show -destroy: %w", err)
	}

	return planFromJSON(plan), nil
}

// Apply runs `terraform apply tfplan` to apply the saved plan.
// Plan() must be called first to generate the tfplan file.
func (c *Client) Apply(ctx context.Context) (*ApplyResult, error) {
	if c.dryRun {
		return c.simulatedApply(), nil
	}
	// Pre-check: ensure the plan file exists so we return a clear error
	// instead of a confusing terraform file-not-found message.
	planPath := filepath.Join(c.workDir, "tfplan")
	if _, err := os.Stat(planPath); err != nil {
		return nil, fmt.Errorf("no saved plan found — call Plan() first: %w", err)
	}
	if err := c.ensureTF(); err != nil {
		return nil, err
	}

	if err := c.tf.Apply(ctx, tfexec.DirOrPlan("tfplan")); err != nil {
		return nil, fmt.Errorf("terraform apply: %w", err)
	}

	// Extract resource addresses from the plan for the result.
	var resources []string
	if plan, err := c.tf.ShowPlanFile(ctx, "tfplan"); err == nil && plan != nil {
		for _, rc := range plan.ResourceChanges {
			if len(rc.Change.Actions) > 0 && rc.Change.Actions[0] != tfjson.ActionNoop {
				resources = append(resources, rc.Address)
			}
		}
	}

	// Fetch outputs after successful apply. If this fails, we still
	// return the resources we know were applied — outputs are best-effort.
	outputs, err := c.outputMap(ctx)
	if err != nil {
		return &ApplyResult{Resources: resources}, nil
	}

	return &ApplyResult{
		Outputs:   outputs,
		Resources: resources,
	}, nil
}

// Destroy runs `terraform destroy` to remove all resources.
// Returns the addresses of resources that were destroyed.
// In dry-run mode, returns the resource addresses from .tf files
// without calling the binary.
func (c *Client) Destroy(ctx context.Context) ([]string, error) {
	if c.dryRun {
		return c.simulatedResourceAddresses(), nil
	}
	if err := c.ensureTF(); err != nil {
		return nil, err
	}

	// Capture resource addresses from state before destroying.
	var resources []string
	if state, err := c.tf.ShowStateFile(ctx, "terraform.tfstate"); err == nil && state != nil &&
		state.Values != nil && state.Values.RootModule != nil {
		for _, r := range state.Values.RootModule.Resources {
			resources = append(resources, r.Address)
		}
	}

	if err := c.tf.Destroy(ctx); err != nil {
		return nil, fmt.Errorf("terraform destroy: %w", err)
	}

	return resources, nil
}

// Output runs `terraform output` and returns all output values.
func (c *Client) Output(ctx context.Context) (map[string]Output, error) {
	if c.dryRun {
		return c.simulatedOutput(), nil
	}
	return c.outputMap(ctx)
}

// outputMap calls tf.Output and converts to our Output type.
func (c *Client) outputMap(ctx context.Context) (map[string]Output, error) {
	if err := c.ensureTF(); err != nil {
		return nil, err
	}

	outputs, err := c.tf.Output(ctx)
	if err != nil {
		return nil, fmt.Errorf("terraform output: %w", err)
	}

	result := make(map[string]Output, len(outputs))
	for k, v := range outputs {
		result[k] = Output{
			Type:      string(v.Type),
			Value:     v.Value,
			Sensitive: v.Sensitive,
		}
	}
	return result, nil
}

// ── Dry-run simulations ──

// isProviderFile reports whether a .tf file contains only provider/terraform
// configuration blocks (no real resources). Used by dry-run simulations to
// avoid counting provider setup as a resource. Detects by content, not
// filename, so it works regardless of how the server layer names the file.
func isProviderFile(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	content := string(b)
	// A real resource block starts with "resource " — if any exists,
	// this is not a pure provider file.
	if strings.Contains(content, "\nresource ") || strings.HasPrefix(content, "resource ") {
		return false
	}
	// Has terraform/provider blocks but no resource blocks → provider file.
	return strings.Contains(content, "terraform {") || strings.Contains(content, "provider ")
}

// simulatedPlan scans workDir for .tf files and returns a mock plan.
func (c *Client) simulatedPlan() *Plan {
	files, _ := filepath.Glob(filepath.Join(c.workDir, "*.tf"))

	var changes []ResourceChange
	for _, f := range files {
		if isProviderFile(f) {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(f), ".tf")
		changes = append(changes, ResourceChange{
			Address: name,
			Type:    name,
			Action:  ActionCreate,
		})
	}

	return &Plan{
		Summary: Summary{
			Create: len(changes),
		},
		Changes: changes,
	}
}

// simulatedApply returns a mock apply result.
func (c *Client) simulatedApply() *ApplyResult {
	return &ApplyResult{
		Outputs:   c.simulatedOutput(),
		Resources: c.simulatedResourceAddresses(),
	}
}

// simulatedResourceAddresses scans workDir for .tf files and returns
// their names as resource addresses. Used by dry-run simulations.
func (c *Client) simulatedResourceAddresses() []string {
	files, _ := filepath.Glob(filepath.Join(c.workDir, "*.tf"))

	var resources []string
	for _, f := range files {
		if isProviderFile(f) {
			continue
		}
		resources = append(resources, strings.TrimSuffix(filepath.Base(f), ".tf"))
	}
	return resources
}

// simulatedDestroyPlan scans workDir for .tf files and returns a mock
// destroy plan where every resource is marked for deletion.
func (c *Client) simulatedDestroyPlan() *Plan {
	files, _ := filepath.Glob(filepath.Join(c.workDir, "*.tf"))

	var changes []ResourceChange
	for _, f := range files {
		if isProviderFile(f) {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(f), ".tf")
		changes = append(changes, ResourceChange{
			Address: name,
			Type:    name,
			Action:  ActionDelete,
		})
	}

	return &Plan{
		Summary: Summary{
			Delete: len(changes),
		},
		Changes: changes,
	}
}

// simulatedOutput returns mock output values.
// In dry-run mode we don't know the real outputs — return an empty map
// so callers can detect the absence rather than getting fake data.
func (c *Client) simulatedOutput() map[string]Output {
	return map[string]Output{}
}
