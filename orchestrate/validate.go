package orchestrate

import (
	"fmt"
	"strings"
)

// ValidateError reports one or more validation problems.
type ValidateError struct {
	Errors []string
}

func (e *ValidateError) Error() string {
	return fmt.Sprintf("plan validation failed:\n  - %s", strings.Join(e.Errors, "\n  - "))
}

// Validate checks a PlanDef for structural correctness.
// agentNames is the set of valid agent names (must be non-empty).
//
// Checks performed:
//   - At least one step
//   - No duplicate step IDs
//   - All agent references exist
//   - All DependsOn references exist
//   - No self-dependency
//   - No duplicate dependencies
//   - No cycles (Kahn's algorithm)
//   - No orphan steps (every step must be reachable from an in-degree-0 node)
//   - At least one step is reachable
func Validate(def *PlanDef, agentNames map[string]bool) error {
	var errs []string

	if len(def.Steps) == 0 {
		errs = append(errs, "plan must have at least one step")
		return &ValidateError{Errors: errs}
	}

	stepIDs := make(map[string]bool)
	stepByID := make(map[string]StepDef)

	for _, s := range def.Steps {
		if s.ID == "" {
			errs = append(errs, "step has empty id")
			continue
		}
		if stepIDs[s.ID] {
			errs = append(errs, fmt.Sprintf("duplicate step id %q", s.ID))
		}
		stepIDs[s.ID] = true
		stepByID[s.ID] = s
	}

	// Check agent references and dependencies.
	for _, s := range def.Steps {
		if s.ID == "" {
			continue
		}
		if s.Agent == "" {
			errs = append(errs, fmt.Sprintf("step %q: agent is required", s.ID))
		} else if len(agentNames) > 0 && !agentNames[s.Agent] {
			errs = append(errs, fmt.Sprintf("step %q: agent %q not found in available agents", s.ID, s.Agent))
		}

		seenDeps := make(map[string]bool)
		for _, dep := range s.DependsOn {
			if dep == s.ID {
				errs = append(errs, fmt.Sprintf("step %q: self-dependency is not allowed", s.ID))
			}
			if dep == "" {
				errs = append(errs, fmt.Sprintf("step %q: dependency id is empty", s.ID))
				continue
			}
			if !stepIDs[dep] {
				errs = append(errs, fmt.Sprintf("step %q: depends on unknown step %q", s.ID, dep))
			}
			if seenDeps[dep] {
				errs = append(errs, fmt.Sprintf("step %q: duplicate dependency %q", s.ID, dep))
			}
			seenDeps[dep] = true
		}
	}

	if len(errs) > 0 {
		return &ValidateError{Errors: errs}
	}

	// ── Cycle detection (Kahn's algorithm) ──

	inDegree := make(map[string]int)
	adj := make(map[string][]string)
	for _, s := range def.Steps {
		inDegree[s.ID] = len(s.DependsOn)
		for _, dep := range s.DependsOn {
			adj[dep] = append(adj[dep], s.ID)
		}
	}

	queue := make([]string, 0)
	for _, s := range def.Steps {
		if inDegree[s.ID] == 0 {
			queue = append(queue, s.ID)
		}
	}

	sorted := make([]string, 0, len(def.Steps))
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		sorted = append(sorted, node)
		for _, next := range adj[node] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if len(sorted) != len(def.Steps) {
		// Find nodes still in the cycle for a useful error message.
		cycleNodes := make([]string, 0)
		for _, s := range def.Steps {
			if inDegree[s.ID] > 0 {
				cycleNodes = append(cycleNodes, s.ID)
			}
		}
		errs = append(errs, fmt.Sprintf("circular dependency detected involving: %s", strings.Join(cycleNodes, ", ")))
	}

	// ── Reachability check ──
	// Every step must be reachable from at least one in-degree-0 node.
	// sorted already contains all reachable nodes (from Kahn).
	reachable := make(map[string]bool)
	for _, id := range sorted {
		reachable[id] = true
	}
	for _, s := range def.Steps {
		if !reachable[s.ID] {
			errs = append(errs, fmt.Sprintf("step %q is not reachable from any entry point (this is a bug — Kahn should have caught it)", s.ID))
		}
	}

	if len(errs) > 0 {
		return &ValidateError{Errors: errs}
	}
	return nil
}
