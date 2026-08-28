package hwcloud

import "context"

// PromptInput carries the data needed to build the final message list for a
// model call. The Runner gathers all sources before each turn and populates:
//
//	StaticContext  — never changes during a run (system prompts, project context)
//	DynamicContext — may change every turn (skills, ACP state, semantic memory, compressed)
//	WorkingMessages — conversation messages in chronological order
type PromptInput struct {
	StaticContext   string
	DynamicContext  string
	WorkingMessages []Message
}

// PromptBuilder assembles the message list for a model call from PromptInput.
type PromptBuilder func(ctx context.Context, input PromptInput) ([]Message, error)

// BuildPrompt is the default PromptBuilder.
func BuildPrompt(_ context.Context, input PromptInput) ([]Message, error) {
	var msgs []Message
	if input.StaticContext != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: input.StaticContext})
	}
	if input.DynamicContext != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: input.DynamicContext})
	}
	msgs = append(msgs, input.WorkingMessages...)
	return msgs, nil
}
