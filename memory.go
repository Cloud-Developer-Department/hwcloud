package hwcloud

// The legacy monolithic Memory interface has been split (P2, Context
// Architecture): short-term conversation storage lives in
// session.SessionStore, token-budget compression in session.Compressor,
// and durable knowledge in provider/memory.MemoryProvider. The root
// package keeps only the shared types below.

// CompressedContext bundles a summary with its coverage marker.
type CompressedContext struct {
	Summary      string `json:"summary"`
	ThroughIndex int    `json:"through_index"`
	// ThroughIndex marks how many messages have been covered by this summary.
	// The next compression pass only compresses messages after this index.
	// 0 means no compression has occurred (or the summary was produced by
	// an older version that didn't track this value).
}

// SafeCompressionBoundary adjusts the overflow index so compression doesn't
// break tool_call/tool_result pairs. If the last message in the compression
// range is an assistant with tool_calls, the boundary extends forward to
// include all consecutive tool results so the summary captures the complete
// tool exchange. all is in chronological order.
//
// Returns the adjusted overflow index (may be larger than input).
func SafeCompressionBoundary(all []Message, overflow int) int {
	if overflow <= 0 || overflow >= len(all) {
		return overflow
	}

	lastCompressed := all[overflow-1]

	// If the last compressed message is an assistant with tool_calls,
	// its tool results (RoleTool) are in the working window. Extend
	// the boundary to include them so the summary captures the complete
	// tool exchange.
	if lastCompressed.Role == RoleAssistant && len(lastCompressed.ToolCalls) > 0 {
		for i := overflow; i < len(all); i++ {
			if all[i].Role == RoleTool {
				overflow = i + 1
			} else {
				break
			}
		}
	}

	return overflow
}
