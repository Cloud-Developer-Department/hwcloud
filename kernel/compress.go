package kernel

import (
	"context"

	"github.com/google/uuid"
	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// CompressStats reports the outcome of a manual compaction.
type CompressStats struct {
	Compressed    int // messages covered by the new summary
	FreedTokens   int // prompt tokens removed from the working set (approx)
	SummaryTokens int // tokens the new summary occupies
}

// compactionOutcome derives the DecisionEvent outcome + detail payload from
// a compactionInfo. Shared by the auto path (prepareMemory) so manual and
// auto compaction report the same vocabulary: Freed when the summary
// advanced, Failed when Compact errored, Skipped when it ran but produced
// no new summary. The first return is the freed-token count (0 unless freed).
func compactionOutcome(ci compactionInfo) (freed int, outcome string, detail map[string]any) {
	if ci.err != nil {
		return 0, hwcloud.OutcomeFailed, map[string]any{"error": ci.err.Error()}
	}
	if ci.count == 0 {
		// Compact succeeded but the summary did not advance (silent no-op).
		return 0, hwcloud.OutcomeSkipped, map[string]any{"reason": "no new messages compressed"}
	}
	return ci.freedTokens, hwcloud.OutcomeFreed, map[string]any{
		"compressed":     ci.count,
		"freed_tokens":   ci.freedTokens,
		"from":           ci.from,
		"to":             ci.to,
		"summary_tokens": ci.summaryTokens, // auto path 0; manual path sets from CompressStats
		"through_index":  ci.throughIndex,  // auto path 0; manual path sets from cc.ThroughIndex
	}
}

// CompressAll compacts the ENTIRE session history into a summary — the
// manual counterpart to prepareMemory's overflow-only auto-compaction.
// The next run starts from summary + working set (the summary is loaded
// unconditionally in prepareMemory, so it survives even when the remaining
// messages fit the budget).
//
// Returns nil stats when there is no compressor (compression unavailable).
// No-op on an empty session.
func (rt *Runtime) CompressAll(ctx context.Context, sessionID string) (*CompressStats, error) {
	// Same nil contract as prepareMemory: no store = no persistence.
	if rt.deps.SessionStore == nil {
		return nil, nil
	}
	if rt.deps.Compressor == nil {
		return nil, nil
	}

	// Manual /compact is NOT a kernel run — the ctx arrives from the ACP slash
	// callback and was never stamped by run(), so the ObserveDecision helper
	// would read empty RunID/TurnID (breaking the grouping invariant) and an
	// empty SessionFromContext (the WithSession the kernel injects at run()
	// entry never ran). Stamp a fresh RunInfo + WithSession here so the 4
	// compaction decision events below join one /compact pass and carry the
	// session ID. TurnID is -1 (manual compaction has no turn loop).
	runID := uuid.NewString()
	ctx = hwcloud.WithRunInfo(ctx, hwcloud.RunInfo{RunID: runID, TurnID: -1})
	ctx = hwcloud.WithSession(ctx, hwcloud.Session{ID: sessionID})

	total, err := rt.deps.SessionStore.Count(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		return &CompressStats{}, nil
	}
	fetchN := total
	if fetchN > 5000 {
		fetchN = 5000
	}
	msgs, err := rt.deps.SessionStore.Recent(ctx, sessionID, fetchN, 0)
	if err != nil || len(msgs) == 0 {
		return nil, err
	}
	globalOffset := total - len(msgs)

	// Record the pre-compaction marker so a repeat /compact (all history
	// already compressed) reports zero instead of claiming a fresh pass.
	oldTI := 0
	if cc, err := rt.deps.Compressor.Compressed(ctx, sessionID); err == nil && cc != nil {
		oldTI = cc.ThroughIndex
	}

	// Compress everything. SafeCompressionBoundary is a no-op at the full
	// range (all tool exchanges are already inside), so pairs stay intact.
	// messages=nil: the backend re-fetches from the session head, so the
	// global throughIndex always aligns (passing the recent-window slice
	// here would misalign when the session exceeds the fetch cap).
	globalCutoff := globalOffset + len(msgs)
	hwcloud.ObserveDecision(ctx, rt.deps.Observer, hwcloud.DecisionEvent{
		Layer: hwcloud.DecisionCompactionManual, Outcome: hwcloud.OutcomeAttempted,
		Subject: sessionID, Detail: map[string]any{"total": total, "cutoff": globalCutoff, "prev_through_index": oldTI},
	})
	if err := rt.deps.Compressor.Compact(ctx, sessionID, globalCutoff, nil); err != nil {
		_, outcome, detail := compactionOutcome(compactionInfo{err: err})
		hwcloud.ObserveDecision(ctx, rt.deps.Observer, hwcloud.DecisionEvent{
			Layer: hwcloud.DecisionCompactionManual, Outcome: outcome,
			Subject: sessionID, Detail: detail,
		})
		return nil, err
	}

	// Compact is a silent no-op without a summarizer (or nothing new to
	// compress) — verify the summary actually advanced before claiming
	// success.
	cc, err := rt.deps.Compressor.Compressed(ctx, sessionID)
	if err != nil || cc == nil || cc.ThroughIndex <= oldTI {
		// Compact ran but produced no new summary (nothing to compress).
		// Report Skipped so a consumer can tell a no-op from a real pass.
		_, outcome, detail := compactionOutcome(compactionInfo{})
		hwcloud.ObserveDecision(ctx, rt.deps.Observer, hwcloud.DecisionEvent{
			Layer: hwcloud.DecisionCompactionManual, Outcome: outcome,
			Subject: sessionID, Detail: detail,
		})
		return &CompressStats{}, nil
	}

	// Resolve the tokenizer from the effective run model (session override
	// wins), falling back to the config model outside a run. Config model
	// read under the lock: SetModel can run concurrently from the serve
	// loop while /compact is in flight.
	rt.mu.RLock()
	cfgModel := rt.cfg.Model
	rt.mu.RUnlock()
	model := rt.runModel
	if model == nil {
		model = cfgModel
	}
	modelID := hwcloud.TokenizerModelID(model)

	// Stats report only what THIS pass newly compressed: compaction is
	// incremental (Compact starts at the previous ThroughIndex), so a
	// second /compact after N messages + one exchange covers just the 2
	// new messages — reporting the whole session count would claim
	// already-summarized messages were compressed again.
	newStart := oldTI - globalOffset
	if newStart < 0 {
		newStart = 0
	}
	if newStart > len(msgs) {
		newStart = len(msgs)
	}
	newMsgs := msgs[newStart:]
	st := &CompressStats{
		Compressed:  cc.ThroughIndex - oldTI,
		FreedTokens: hwcloud.CountMessages(modelID, newMsgs),
	}
	st.SummaryTokens = hwcloud.CountMessageTokens(modelID, hwcloud.Message{
		Role:    hwcloud.RoleSystem,
		Content: cc.Summary,
	})
	st.FreedTokens -= st.SummaryTokens
	if st.FreedTokens < 0 {
		st.FreedTokens = 0
	}
	// Route the Freed outcome through compactionOutcome so the manual path's
	// detail shape matches the auto path's exactly (same key set). The helper
	// reads count/freedTokens/from/to/summaryTokens/throughIndex — all set
	// here. `from` is the pre-compaction ThroughIndex (the global start of
	// this pass); `to` is the new ThroughIndex (the exclusive end).
	_, outcome, detail := compactionOutcome(compactionInfo{
		count:         st.Compressed,
		freedTokens:   st.FreedTokens,
		from:          oldTI,
		to:            cc.ThroughIndex,
		summaryTokens: st.SummaryTokens,
		throughIndex:  cc.ThroughIndex,
	})
	hwcloud.ObserveDecision(ctx, rt.deps.Observer, hwcloud.DecisionEvent{
		Layer: hwcloud.DecisionCompactionManual, Outcome: outcome,
		Subject: sessionID, Detail: detail,
	})
	return st, nil
}

// ContextUsage reports the current per-layer context usage for a session:
// summary tokens (compressed history), working tokens (uncompressed
// messages), and the model's context window. Powers /context.
func (rt *Runtime) ContextUsage(ctx context.Context, sessionID string) (summary, working, window int, err error) {
	rt.mu.RLock()
	cfgModel := rt.cfg.Model
	rt.mu.RUnlock()
	model := rt.runModel
	if model == nil {
		model = cfgModel
	}
	modelID := hwcloud.TokenizerModelID(model)
	var throughIndex int
	if rt.deps.Compressor != nil {
		if cc, err := rt.deps.Compressor.Compressed(ctx, sessionID); err == nil && cc != nil {
			throughIndex = cc.ThroughIndex
			summary = hwcloud.CountMessageTokens(modelID, hwcloud.Message{
				Role:    hwcloud.RoleSystem,
				Content: cc.Summary,
			})
		}
	}
	if rt.deps.SessionStore != nil {
		total, err := rt.deps.SessionStore.Count(ctx, sessionID)
		if err != nil {
			return 0, 0, window, err
		}
		// Post-summary increment only — messages never deleted, and the
		// summary covers up to throughIndex, so fetching all would
		// double-count summarized history.
		msgs, err := rt.deps.SessionStore.RecentAfter(ctx, sessionID, throughIndex, total-throughIndex)
		if err == nil {
			working = hwcloud.CountMessages(modelID, msgs)
		}
	}
	if model != nil {
		window = model.ContextWindow()
	}
	return summary, working, window, nil
}
