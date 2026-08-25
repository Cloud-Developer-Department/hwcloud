package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
)

// AsTool wraps an agent config + deps as a Tool so a coordinator can
// delegate sub-tasks in parallel. Unlike handoff (transfer_to_*), the
// sub-agent runs with isolated context and returns its output.
//
// The wrapped sub-agent runs with MaxTurns=3 and never spawns further
// sub-agents. Deps are used as given — pass MemoryProvider to share
// project knowledge (v2.0 §22), or leave empty for a fully isolated
// delegation. Unlike config-driven sub-agents (Agent.SubAgents), the
// default policy does NOT auto-allow delegation calls for AsTool tools:
// apps that gate them must supply rules via Deps.Policy. For
// config-driven sub-agents, the runtime builds the tool via
// newSubAgentTool with inherited deps.
func AsTool(cfg *agent.Agent, deps Deps) hwcloud.Tool {
	subCfg := cfg.Clone()
	subCfg.MaxTurns = 3
	subCfg.SubAgents = nil
	subCfg.Prompt = nil
	return &subAgentTool{cfg: subCfg, deps: deps}
}

// subAgentTool implements Tool by running a nested Runtime. The model
// supplies only the task; the agent identity (name, system prompt, tools,
// model) is fixed by config.
type subAgentTool struct {
	cfg  *agent.Agent
	deps Deps
	// toolFilter resolves the child's tool set at call time from the
	// parent's current tool snapshot (nil = use deps.Tools as-is).
	toolFilter func() []hwcloud.Tool
}

// newSubAgentTool builds the delegation tool for a configured sub-agent.
// The child inherits the parent's runtime deps — knowledge, policy,
// approver, hooks, observer — but isolates its conversation (fresh
// session, no SessionStore/Compressor). Its tool set is resolved from
// the parent snapshot at call time, minus all sub-agent tools, narrowed
// to sa.Tools when an allowlist is configured.
func (rt *Runtime) newSubAgentTool(sa agent.SubAgent) *subAgentTool {
	subCfg := rt.cfg.Clone()
	subCfg.Name = sa.Name
	subCfg.Description = sa.Description
	subCfg.SubAgents = nil // no nested delegation
	subCfg.MaxTurns = sa.MaxTurns
	if subCfg.MaxTurns == 0 {
		subCfg.MaxTurns = 3
	}
	if sa.Model != nil {
		subCfg.Model = sa.Model
	}
	subCfg.SystemPrompts = []string{sa.SystemPrompt}
	if strings.TrimSpace(sa.SystemPrompt) == "" {
		subCfg.SystemPrompts = []string{fmt.Sprintf(
			"You are the %s sub-agent. Complete the delegated task in an isolated context, using the available tools. Ask for clarification only if the task is ambiguous.", sa.Name)}
	}

	deps := rt.deps
	deps.Tools = nil                      // resolved at call time
	deps.SessionStore = nil               // isolated conversation
	deps.Compressor = nil                 // isolated: no shared summary
	deps.HumanApprover = rt.humanApprover // runtime-resolved (acp may swap mid-run)
	deps.ApprovalMemory = rt.approvalMemory

	names := rt.subAgentNames()
	allow := sa.Tools
	exclude := nameSet(rt.deps.SubAgentExcludeTools)
	return &subAgentTool{
		cfg:  subCfg,
		deps: deps,
		toolFilter: func() []hwcloud.Tool {
			return filterChildTools(rt.SnapshotTools(), names, exclude, allow)
		},
	}
}

// subAgentNames returns the names of all configured sub-agents (children
// never see other sub-agent tools — no recursion).
func (rt *Runtime) subAgentNames() map[string]bool {
	names := make(map[string]bool, len(rt.cfg.SubAgents))
	for _, sa := range rt.cfg.SubAgents {
		names[sa.Name] = true
	}
	return names
}

func nameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// filterChildTools builds a sub-agent's tool set: all parent tools except
// other sub-agents and the caller-supplied exclusions (Deps.
// SubAgentExcludeTools — e.g. session-mode tools whose callbacks are
// session-bound and would be nil in the child), narrowed to the allowlist
// when one is configured.
func filterChildTools(snapshot []hwcloud.Tool, subAgentNames, exclude map[string]bool, allowlist []string) []hwcloud.Tool {
	var allow map[string]bool
	if allowlist != nil {
		allow = make(map[string]bool, len(allowlist))
		for _, n := range allowlist {
			allow[n] = true
		}
	}
	out := make([]hwcloud.Tool, 0, len(snapshot))
	for _, t := range snapshot {
		n := t.Definition().Name
		if subAgentNames[n] || exclude[n] {
			continue
		}
		if allow != nil && !allow[n] {
			continue
		}
		out = append(out, t)
	}
	return out
}

func (t *subAgentTool) Definition() hwcloud.FunctionDefinition {
	name := t.cfg.Name
	desc := t.cfg.Description
	if desc == "" {
		desc = "Handle a task delegated by the coordinator."
	}
	return hwcloud.FunctionDefinition{
		Name: name,
		Description: fmt.Sprintf("Delegate to the %s sub-agent. %s "+
			"It runs in an isolated context with no access to the current conversation history — pass all needed context in the task.", name, desc),
		Parameters: hwcloud.SchemaOf[DelegateParams](),
	}
}

// resolveDeps materializes the child's tool set at call time (the parent
// snapshot may have grown since registration — plan tools, etc.).
func (t *subAgentTool) resolveDeps() Deps {
	deps := t.deps
	if t.toolFilter != nil {
		deps.Tools = t.toolFilter()
	}
	return deps
}

// session returns the current run's session (injected into the tool
// context by the execution runtime), or an empty one outside a run.
func (t *subAgentTool) session(ctx context.Context) hwcloud.Session {
	s, _ := hwcloud.SessionFromContext(ctx)
	return s
}

// Execute runs the sub-agent synchronously and returns its final output.
func (t *subAgentTool) Execute(ctx context.Context, args json.RawMessage) *hwcloud.ToolResult {
	params, err := hwcloud.ParseArgs[DelegateParams](args)
	if err != nil {
		return hwcloud.ErrorResult(fmt.Errorf("agent tool %q: %w", t.cfg.Name, err), false, "")
	}
	output, err := runChild(ctx, t.cfg, t.resolveDeps(), t.session(ctx), params.Task, nil)
	if err != nil {
		return &hwcloud.ToolResult{
			Error: &hwcloud.ToolError{Message: fmt.Sprintf("agent tool %q: %v", t.cfg.Name, err)},
		}
	}
	return &hwcloud.ToolResult{Content: output}
}

// ExecuteStream runs the sub-agent with streaming. Text deltas and tool
// results are emitted as ToolStreamChunk events so the coordinator can
// show real-time progress; the stream ends with the final output.
func (t *subAgentTool) ExecuteStream(ctx context.Context, args json.RawMessage) <-chan hwcloud.ToolStreamChunk {
	params, err := hwcloud.ParseArgs[DelegateParams](args)
	if err != nil {
		ch := make(chan hwcloud.ToolStreamChunk, 1)
		ch <- hwcloud.ToolStreamChunk{Error: fmt.Errorf("agent tool %q: %w", t.cfg.Name, err)}
		close(ch)
		return ch
	}

	ch := make(chan hwcloud.ToolStreamChunk, 16)
	go func() {
		defer close(ch)
		output, err := runChild(ctx, t.cfg, t.resolveDeps(), t.session(ctx), params.Task, func(ev hwcloud.StreamEvent) {
			text := ev.Text
			if ev.Type == hwcloud.StreamToolResult {
				text = ev.Message.Content
			}
			if text != "" {
				ch <- hwcloud.ToolStreamChunk{Content: text}
			}
		})
		if err != nil {
			ch <- hwcloud.ToolStreamChunk{Error: err}
			return
		}
		ch <- hwcloud.ToolStreamChunk{Content: output}
	}()
	return ch
}

// DelegateParams are the arguments to a delegation tool (kernel.AsTool or
// a configured sub-agent): only the task, the agent identity comes from
// config.
type DelegateParams struct {
	Task string `json:"task" jsonschema:"description=The task to complete"`
}
