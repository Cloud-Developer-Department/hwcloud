package wecom

import (
	"encoding/json"
	"fmt"
)

// buildApprovalCard constructs a WeCom button_interaction template card
// for a tool call approval request. The card shows the tool title in the
// desc field (smaller font), then three action buttons.
//
// The approvalID is used as the card's task_id — the template_card_event
// callback carries this task_id so the approver can correlate clicks
// back to the pending approval.
func buildApprovalCard(approvalID, toolTitle string) (json.RawMessage, error) {
	card := map[string]any{
		"card_type": "button_interaction",
		"main_title": map[string]any{
			"title": "需要权限",
			"desc":  toolTitle,
		},
		"button_list": []map[string]any{
			{"text": "允许", "style": 1, "key": "allow_once"},
			{"text": "始终允许", "style": 1, "key": "allow_always"},
			{"text": "拒绝", "style": 4, "key": "deny"},
		},
		"task_id": approvalID,
	}

	b, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("wecom approval card marshal: %w", err)
	}
	return b, nil
}

// buildResolvedCard constructs the updated template card shown after the
// user has made a decision. The buttons are replaced with a single
// disabled button showing the outcome, so the user cannot click again.
func buildResolvedCard(approvalID, toolTitle, decision string) (json.RawMessage, error) {
	buttonText := "✅ 已同意"
	buttonStyle := 1
	if decision == "deny" {
		buttonText = "❌ 已拒绝"
		buttonStyle = 4
	}

	card := map[string]any{
		"card_type": "button_interaction",
		"main_title": map[string]any{
			"title": "需要权限",
			"desc":  toolTitle,
		},
		"button_list": []map[string]any{
			{"text": buttonText, "style": buttonStyle, "key": "done"},
		},
		"task_id": approvalID,
	}

	b, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("wecom resolved card marshal: %w", err)
	}
	return b, nil
}
