package hwcloud

import (
	"github.com/Cloud-Developer-Department/hwcloud/tokenizer"
)

// CountTokens counts the tokens of a text using the tokenizer for the given
// model id. Unknown model ids fall back to a heuristic estimate inside the
// tokenizer package, so this never panics.
func CountTokens(modelID, text string) int {
	return tokenizer.Count(modelID, text)
}

// TokenizerModelID returns the canonical encoding name for token counting
// for a model. Uses the optional TokenizerModeler interface, falling back
// to "gpt-4" (cl100k_base).
func TokenizerModelID(model Model) string {
	if tm, ok := model.(TokenizerModeler); ok {
		if name := tm.TokenizerModel(); name != "" {
			return name
		}
	}
	return "gpt-4"
}

// CountMessageTokens returns the token count for a message using the
// model-specific tokenizer. Falls back to a heuristic if unavailable.
func CountMessageTokens(modelID string, m Message) int {
	n := tokenizer.Count(modelID, m.Content)
	n += tokenizer.Count(modelID, m.ReasoningContent)
	for _, tc := range m.ToolCalls {
		n += tokenizer.Count(modelID, tc.Function.Name)
		n += tokenizer.Count(modelID, tc.Function.Arguments)
	}
	// Message formatting overhead: role prefix, JSON structure (~4 tokens).
	return n + 4
}

// CountMessages returns the total token count for a set of messages.
// Used by the kernel's hard context-window check (fail loudly instead of
// silently dropping messages).
func CountMessages(modelID string, msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += CountMessageTokens(modelID, m)
	}
	return n
}
