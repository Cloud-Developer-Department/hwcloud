package utils

import "strings"

const (
	Window1M   = 1_000_000
	Window400K = 400_000
	Window256K = 256_000
	Window224K = 224_000
	Window200K = 200_000
	Window128K = 128_000
	Window32K  = 32_000
	Window28K  = 28_000
	Window16K  = 16_000
	Window8K   = 8_000
	Window4K   = 4_000

	DefaultWindow = Window128K
)

// ModelContextWindow returns the context window size for the given model
// ID. Uses case-insensitive substring matching against official vendor
// documentation. Falls back to [DefaultWindow] (128K) for unknown models.
func ModelContextWindow(modelID string) int {
	lower := strings.ToLower(modelID)

	switch {
	// ── OpenAI ──
	// https://platform.openai.com/docs/models
	case has(lower, "o1") || has(lower, "o3") || has(lower, "o4"):
		return Window200K // o-series reasoning: 200K context
	case has(lower, "gpt-5"):
		return Window400K // GPT-5 family: 400K context
	case has(lower, "gpt-4o") || has(lower, "gpt-4"):
		return Window128K

	// ── DeepSeek ──
	// https://api-docs.deepseek.com/zh-cn/quick_start/pricing
	case has(lower, "deepseek"):
		if has(lower, "v2.5") {
			return Window8K
		}
		return Window1M // v4-flash, v4-pro, chat, reasoner all 1M

	// ── Claude / Anthropic ──
	// https://docs.anthropic.com/en/docs/about-claude/models/overview
	case has(lower, "claude"):
		switch {
		case has(lower, "haiku-4.5") || has(lower, "haiku-4-5"):
			return Window200K
		case has(lower, "sonnet-4.5") || has(lower, "sonnet-4-5"):
			return Window200K
		case has(lower, "opus-4.5") || has(lower, "opus-4-5"):
			return Window200K
		case has(lower, "opus-4.1") || has(lower, "opus-4-1"):
			return Window200K
		default:
			return Window1M // Fable 5, Opus 5, Sonnet 5, Opus 4.6-4.8, Sonnet 4.6
		}

	// ── Hunyuan ──
	// https://cloud.tencent.com/document/product/1729/104753
	case has(lower, "hunyuan"):
		if has(lower, "a13b") {
			return Window224K
		}
		if has(lower, "translation") {
			return Window4K
		}
		if has(lower, "vision") {
			return Window28K
		}
		if has(lower, "role") {
			return Window28K
		}
		return Window32K // t1, turbos, lite etc. ~32K

	// ── Qwen (Tongyi Qianwen) ──
	// https://help.aliyun.com/zh/model-studio/models
	case has(lower, "qwen"):
		switch {
		case has(lower, "long"):
			return Window1M
		case has(lower, "turbo"):
			return Window1M
		case has(lower, "plus") || has(lower, "max"):
			return Window128K
		case has(lower, "qwen3") || has(lower, "qwen2"):
			return Window128K
		default:
			return Window32K
		}

	// ── GLM (Zhipu) ──
	// https://open.bigmodel.cn/pricing
	case has(lower, "glm"):
		if has(lower, "4v") || has(lower, "4-v") {
			return Window8K
		}
		if has(lower, "glm-5") || has(lower, "glm5") {
			return Window1M // GLM-5 / 5.1 / 5.2 series are 1M context
		}
		return Window128K // GLM-4, GLM-4-Flash, GLM-3-Turbo all 128K

	// ── Kimi / Moonshot ──
	// https://platform.moonshot.cn/docs/pricing/chat
	case has(lower, "kimi"):
		return Window128K
	case has(lower, "moonshot"):
		if has(lower, "128k") {
			return Window128K
		}
		if has(lower, "32k") {
			return Window32K
		}
		if has(lower, "8k") {
			return Window8K
		}
		return Window32K

	// ── Doubao (ByteDance) ──
	// https://www.volcengine.com/docs/82379/1330310
	case has(lower, "doubao"):
		if has(lower, "256k") {
			return Window256K
		}
		if has(lower, "128k") {
			return Window128K
		}
		if has(lower, "32k") {
			return Window32K
		}
		return Window32K

	// ── Step ──
	// https://platform.stepfun.com/docs/llm/text
	case has(lower, "step-"):
		if has(lower, "256k") {
			return Window256K
		}
		if has(lower, "128k") {
			return Window128K
		}
		if has(lower, "32k") {
			return Window32K
		}
		if has(lower, "16k") {
			return Window16K
		}
		if has(lower, "8k") || has(lower, "flash") {
			return Window8K
		}
		return Window32K

	// ── Ernie / Baidu ──
	// https://ai.baidu.com/ai-doc/WENXINWORKSHOP/Wm9cvy6rl
	case has(lower, "ernie"):
		if has(lower, "128k") {
			return Window128K
		}
		if has(lower, "8k") {
			return Window8K
		}
		return Window32K

	// ── Spark (iFlytek) ──
	case has(lower, "spark"):
		if has(lower, "128k") {
			return Window128K
		}
		if has(lower, "pro") || has(lower, "max") {
			return Window32K
		}
		return Window8K

	// ── Yi (Lingyiwanwu) ──
	case has(lower, "yi-"):
		return Window16K

	// ── LLaMA / Meta ──
	case has(lower, "llama"):
		if has(lower, "3.1") || has(lower, "3.3") {
			return Window128K
		}
		if has(lower, "3.2") {
			return Window128K
		}
		if has(lower, "4") {
			return Window128K
		}
		return Window128K

	// ── Command R / Cohere ──
	// https://docs.cohere.com/v2/docs/models#Command
	case has(lower, "command-r") || has(lower, "command-r-plus"):
		return Window128K
	case has(lower, "command") || has(lower, "cohere"):
		return Window4K
	}

	return DefaultWindow
}

func has(s, substr string) bool {
	return strings.Contains(s, substr)
}
