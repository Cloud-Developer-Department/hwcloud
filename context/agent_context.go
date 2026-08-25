package context

import (
	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/provider/resource"
)

// MemoryKind distinguishes the sources of contextual memories injected
// into the prompt. Each kind renders as its own prompt section so the
// model can attribute provenance.
type MemoryKind string

const (
	// MemorySummary is the rolling conversation summary (compression layer).
	MemorySummary MemoryKind = "summary"
	// MemoryKnowledge is durable recalled knowledge (MemoryProvider).
	MemoryKnowledge MemoryKind = "knowledge"
)

// MemoryEntry is one piece of remembered context injected into the prompt.
type MemoryEntry struct {
	Kind    MemoryKind  `json:"kind"`
	Content string      `json:"content"`
	Topic   string      `json:"topic,omitempty"` // upsert key (for the extractor's update classification)
	Path    string      `json:"path,omitempty"`  // source file, if any
	Score   float64     `json:"score,omitempty"`
}

// AgentContext is the assembled input the agent sees — the projection of
// session state, memories, skills, and resources that goes into the LLM.
// It is deliberately separate from RuntimeState (execution bookkeeping):
// AgentContext is knowledge, RuntimeState is what is happening.
//
// Skill bodies are NOT part of AgentContext: the model loads a skill's
// SKILL.md via the load_skill tool and the body enters the conversation
// as a tool result (the industry pattern — Claude Code reads SKILL.md the
// same way). A per-turn "loaded skills" prompt section would duplicate
// what the tool result already carries.
type AgentContext struct {
	Messages  []hwcloud.Message   `json:"messages"`
	Memories  []MemoryEntry         `json:"memories,omitempty"`
	Skills    []hwcloud.SkillInfo `json:"skills,omitempty"`
	Resources []resource.Resource   `json:"resources,omitempty"`
}
