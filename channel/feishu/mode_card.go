package feishu

import (
	"encoding/json"
	"fmt"
)

// modeDescription is the shared Manual/Auto explanation shown on mode cards.
const modeDescription = "• **Manual**：每个非只读工具调用需点击审批卡片\n" +
	"• **Auto**：自动执行，无需审批（高风险）"

// buildModeCardResolved constructs the card JSON shown after the user has
// switched modes. The header colour reflects the new mode (green for auto,
// blue for manual). The body shows a confirmation message and the updated
// button row with the new current mode highlighted.
func buildModeCardResolved(currentMode string) (string, error) {
	template := "blue"
	if currentMode == "auto" {
		template = "green"
	}

	body := fmt.Sprintf(
		"✅ 已切换到 **%s** 模式\n\n"+
			modeDescription+"\n\n"+
			"当前模式：**%s**",
		modeLabel(currentMode),
		modeLabel(currentMode),
	)

	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": "模式切换"},
			"subtitle": map[string]any{"tag": "plain_text", "content": ""},
			"template": template,
		},
		"body": map[string]any{
			"direction": "vertical",
			"elements": []map[string]any{
				{
					"tag":     "markdown",
					"content": body,
				},
				{"tag": "hr"},
				modeButtonRow(currentMode),
			},
		},
	}

	b, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("feishu mode card resolved marshal: %w", err)
	}
	return string(b), nil
}

// modeButtonRow builds the column_set element containing the two mode
// buttons (Manual / Auto). The current mode uses primary_filled (bold
// colour); the other uses default (grey). Button values carry
// {"type":"mode_switch","mode":"..."} so handleCardAction can route
// mode-switch clicks separately from approval clicks.
func modeButtonRow(currentMode string) map[string]any {
	btn := func(label, mode, btnType string) map[string]any {
		return map[string]any{
			"tag":   "button",
			"text":  map[string]any{"tag": "plain_text", "content": label},
			"type":  btnType,
			"width": "default",
			"name":  "mode_" + mode,
			"value": map[string]any{"type": "mode_switch", "mode": mode},
		}
	}

	manualType := "primary_filled"
	autoType := "default"
	if currentMode == "auto" {
		manualType = "default"
		autoType = "primary_filled"
	}

	manualLabel := "Manual"
	autoLabel := "Auto"
	if currentMode == "manual" {
		manualLabel = "Manual (当前)"
	} else if currentMode == "auto" {
		autoLabel = "Auto (当前)"
	}

	return map[string]any{
		"tag":                "column_set",
		"flex_mode":          "flow",
		"horizontal_spacing": "8px",
		"columns": []map[string]any{
			{"tag": "column", "width": "auto", "elements": []map[string]any{btn(manualLabel, "manual", manualType)}},
			{"tag": "column", "width": "auto", "elements": []map[string]any{btn(autoLabel, "auto", autoType)}},
		},
	}
}

// modeLabel returns a human-readable label for a mode identifier.
func modeLabel(mode string) string {
	switch mode {
	case "auto":
		return "Auto"
	case "manual":
		return "Manual"
	default:
		return "Manual"
	}
}
