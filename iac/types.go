package iac

import (
	"encoding/json"

	tfjson "github.com/hashicorp/terraform-json"
)

// Action represents the kind of change Terraform will make to a resource.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
	ActionNoop   Action = "noop"
)

// Summary is an aggregate count of planned changes.
type Summary struct {
	Create int `json:"create"`
	Update int `json:"update"`
	Delete int `json:"delete"`
	Noop   int `json:"noop"`
}

// ResourceChange describes a single resource diff in a plan.
type ResourceChange struct {
	Address string          `json:"address"` // e.g. "huaweicloud_compute_instance.web"
	Type    string          `json:"type"`    // e.g. "huaweicloud_compute_instance"
	Action  Action          `json:"action"`
	Before  json.RawMessage `json:"before,omitempty"` // prior state (populated on update/delete)
	After   json.RawMessage `json:"after,omitempty"`  // desired state (populated on create/update)
}

// Plan is the structured result of terraform plan.
type Plan struct {
	Summary Summary          `json:"summary"`
	Changes []ResourceChange `json:"changes"`
	Raw     *tfjson.Plan     `json:"-"` // full JSON for advanced use
}

// ValidateResult is the structured result of terraform validate.
type ValidateResult struct {
	Valid    bool     `json:"valid"`
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// ApplyResult is the structured result of terraform apply.
type ApplyResult struct {
	Outputs   map[string]Output `json:"outputs,omitempty"`
	Resources []string          `json:"resources,omitempty"` // addresses of created/modified resources
}

// Output is a single terraform output value.
type Output struct {
	Type      string          `json:"type"` // "string", "number", "list", ...
	Value     json.RawMessage `json:"value"`
	Sensitive bool            `json:"sensitive,omitempty"`
}

// planFromJSON converts a tfjson.Plan into the package's Plan type.
func planFromJSON(p *tfjson.Plan) *Plan {
	if p == nil {
		return &Plan{}
	}

	pl := &Plan{
		Raw: p,
	}

	for _, rc := range p.ResourceChanges {
		if len(rc.Change.Actions) == 0 {
			continue
		}

		// Map tfjson.Action (which can be a composite like "create-delete")
		// to our Action. We take the first action as the primary.
		primary := actionFromTFJSON(rc.Change.Actions[0])

		switch primary {
		case ActionCreate:
			pl.Summary.Create++
		case ActionUpdate:
			pl.Summary.Update++
		case ActionDelete:
			pl.Summary.Delete++
		case ActionNoop:
			pl.Summary.Noop++
		}

		// Skip no-op resources from the detailed change list — they
		// add noise without value.
		if primary == ActionNoop {
			continue
		}

		change := ResourceChange{
			Address: rc.Address,
			Type:    rc.Type,
			Action:  primary,
		}
		if rc.Change.Before != nil {
			change.Before, _ = json.Marshal(rc.Change.Before)
		}
		if rc.Change.After != nil {
			change.After, _ = json.Marshal(rc.Change.After)
		}
		pl.Changes = append(pl.Changes, change)
	}

	return pl
}

func actionFromTFJSON(a tfjson.Action) Action {
	switch a {
	case tfjson.ActionCreate:
		return ActionCreate
	case tfjson.ActionUpdate:
		return ActionUpdate
	case tfjson.ActionDelete:
		return ActionDelete
	case tfjson.ActionNoop:
		return ActionNoop
	default:
		// Composite actions (e.g. "create-delete") — represent as
		// the raw string for forward compatibility.
		return Action(a)
	}
}
