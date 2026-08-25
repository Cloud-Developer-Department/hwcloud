// Package context implements the Context Runtime: building the
// [AgentContext] an agent sees each turn (goal, working messages, memories,
// skills, resources) and committing new state back. It owns the compaction
// pipeline and knowledge recall — the agent only consumes the resulting
// context; it never touches storage directly.
//
// The package depends only on the root package (core types) and
// session/provider interfaces, so kernel/ and the application layers can
// consume it without cycles.
package context
