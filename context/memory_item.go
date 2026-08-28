package context

// MemoryItem is a single durable knowledge item stored and recalled by a
// MemoryProvider (preference, fact, lesson).
//
// Topic is the upsert key: storing an item with a non-empty topic replaces
// any existing item with the same (scope, topic) — the extractor's
// "update" operation (Mem0-style ADD/UPDATE). Empty topic appends.
type MemoryItem struct {
	Kind    string         `json:"kind"`            // "preference" | "fact" | "lesson"
	Content string         `json:"content"`         // the knowledge text
	Topic   string         `json:"topic,omitempty"` // upsert key (scope+topic)
	Meta    map[string]any `json:"meta,omitempty"`  // source session, timestamp, confidence, ...
}
