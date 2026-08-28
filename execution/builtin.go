package execution

import (
	"context"
	"fmt"
	"strings"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
)

// ── Built-in tool definitions ──

var (
	builtinLoadSkillDef = hwcloud.FunctionDefinition{
		Name:        "load_skill",
		Description: "Load a skill's full instructions from its SKILL.md. Use when you need detailed guidance on a specific topic.",
		Parameters:  hwcloud.SchemaOf[LoadSkillParams](),
	}
	builtinReloadSkillsDef = hwcloud.FunctionDefinition{
		Name:        "reload_skills",
		Description: "Rescan the skills directory for newly installed or removed skills. Use after installing or uninstalling a skill.",
		Parameters:  hwcloud.SchemaOf[ReloadSkillsParams](),
	}
	builtinRecallDef = hwcloud.FunctionDefinition{
		Name:        "recall",
		Description: "Search the long-term memory store (knowledge extracted from past sessions) for durable facts about the user and the project — preferences, decisions, technical details. It does NOT search this session's raw message history: details omitted by the conversation summary are not retrievable via recall. Returns ranked results with relevance scores.",
		Parameters:  hwcloud.SchemaOf[RecallParams](),
	}
)

// LoadSkillParams are the arguments to load_skill.
type LoadSkillParams struct {
	Name string `json:"name" jsonschema:"description=Name of the skill to load"`
}

// ReloadSkillsParams is empty (no arguments).
type ReloadSkillsParams struct{}

// RecallParams are the arguments to recall.
type RecallParams struct {
	Query string `json:"query" jsonschema:"description=Specific keywords to find (e.g. 'kubectl rollout restart', 'benchmark_2024.csv', 'port 5432')"`
}

// builtinDef returns the definition for a built-in tool name.
func (e *ExecutionRuntime) builtinDef(name string) hwcloud.FunctionDefinition {
	switch name {
	case "load_skill":
		return builtinLoadSkillDef
	case "reload_skills":
		return builtinReloadSkillsDef
	case "recall":
		return builtinRecallDef
	}
	return hwcloud.FunctionDefinition{}
}

// builtinHandler returns the execution handler for a built-in tool.
func (e *ExecutionRuntime) builtinHandler(name string) BuiltinHandler {
	switch name {
	case "load_skill":
		return e.executeLoadSkill
	case "reload_skills":
		return e.executeReloadSkills
	case "recall":
		return e.executeRecall
	}
	return nil
}

func builtinSkillToolDefs() []hwcloud.FunctionDefinition {
	return []hwcloud.FunctionDefinition{builtinLoadSkillDef, builtinReloadSkillsDef}
}

// ── load_skill ──

func (e *ExecutionRuntime) executeLoadSkill(ctx context.Context, session hwcloud.Session, call hwcloud.ToolCall, ch chan<- hwcloud.StreamEvent) hwcloud.Message {
	args, err := hwcloud.ParseArgs[LoadSkillParams]([]byte(call.Function.Arguments))
	if err != nil {
		return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("load_skill: %v", err)}
	}

	// Idempotent: return cached.
	e.loadedSkillsMu.RLock()
	body, ok := e.loadedSkills[args.Name]
	e.loadedSkillsMu.RUnlock()
	if ok {
		return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: body}
	}

	// Find skill in catalog.
	var info hwcloud.SkillInfo
	found := false
	if e.cfg.SkillProvider != nil {
		skills, err := e.cfg.SkillProvider.Discover(ctx)
		if err != nil {
			return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("load_skill: discover failed: %v", err)}
		}
		for _, s := range skills {
			if s.Name == args.Name {
				info = s
				found = true
				break
			}
		}
	}
	if !found {
		return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("load_skill: skill %q not found", args.Name)}
	}

	loaded, err := e.cfg.SkillProvider.Load(ctx, info)
	if err != nil {
		return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("load_skill: %v", err)}
	}
	body = fmt.Sprintf("**Directory:** %s\n\n%s", info.Path, loaded)
	e.loadedSkillsMu.Lock()
	e.loadedSkills[args.Name] = body
	e.loadedSkillsMu.Unlock()
	return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: body}
}

// ── reload_skills ──

func (e *ExecutionRuntime) executeReloadSkills(ctx context.Context, session hwcloud.Session, call hwcloud.ToolCall, ch chan<- hwcloud.StreamEvent) hwcloud.Message {
	if e.cfg.SkillProvider == nil {
		return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: "reload_skills: no skill provider configured"}
	}
	skills, err := e.cfg.SkillProvider.Discover(ctx)
	if err != nil {
		return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("reload_skills: %v", err)}
	}
	// Prune loaded skills that no longer exist.
	seen := make(map[string]bool, len(skills))
	for _, s := range skills {
		seen[s.Name] = true
	}
	e.loadedSkillsMu.Lock()
	for name := range e.loadedSkills {
		if !seen[name] {
			delete(e.loadedSkills, name)
		}
	}
	e.loadedSkillsMu.Unlock()
	return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("reloaded %d skills", len(skills))}
}

// ── recall ──

func (e *ExecutionRuntime) executeRecall(ctx context.Context, session hwcloud.Session, call hwcloud.ToolCall, ch chan<- hwcloud.StreamEvent) hwcloud.Message {
	if e.cfg.MemoryProvider == nil {
		return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: "recall: no memory provider configured"}
	}
	args, err := hwcloud.ParseArgs[RecallParams]([]byte(call.Function.Arguments))
	if err != nil {
		return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("recall: %v", err)}
	}
	// User-level scope: knowledge is stored by user (extractor), not by
	// session — session-scoped recall would find nothing across sessions.
	results, err := e.cfg.MemoryProvider.Recall(ctx, ctxpkg.ContextScope{UserID: session.UserID}, args.Query, 5)
	if err != nil {
		return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("recall: %v", err)}
	}
	if len(results) == 0 {
		return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: "No results found in memory."}
	}

	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "--- [%.2f] %s ---\n%s\n\n", r.Score, r.Kind, r.Content)
	}
	return hwcloud.Message{Role: hwcloud.RoleTool, ToolCallID: call.ID, Content: strings.TrimSpace(b.String())}
}

// BuiltinSkillToolDefs returns load_skill + reload_skills definitions.
func BuiltinSkillToolDefs() []hwcloud.FunctionDefinition { return builtinSkillToolDefs() }

// BuiltinRecallDef returns the recall tool definition.
func BuiltinRecallDef() hwcloud.FunctionDefinition { return builtinRecallDef }
