// Package memory is the home of durable knowledge providers — the
// "what we have learned" layer, distinct from short-term session storage
// (session.SessionStore) and compression (session.Compressor).
//
// The MemoryProvider interface is defined in package context (at the
// consumer, per Go convention) as context.MemoryProvider. Concrete
// providers live in this package and implement it:
//
//   - sqlite: knowledge table + vector index over the shared database file
//   - file:   knowledge.jsonl, zero external dependencies
//   - openviking: remote knowledge service (protocol skeleton, adjust to
//     the deployed API)
//
// Providers store and retrieve durable knowledge items scoped by context
// (user/project/session): user preferences, project facts, lessons
// learned, past decisions. The Context Runtime recalls them when building
// the agent context for a new turn and stores them when the knowledge
// extractor produces new items.
package memory
