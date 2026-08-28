package kernel

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	ctxpkg "github.com/Cloud-Developer-Department/hwcloud/context"
	"github.com/Cloud-Developer-Department/hwcloud/eventbus"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
)

// run is the 8-node mainline loop. It orchestrates; each node is a method
// so stages can be unit-tested and extended independently.
//
//	① Memory fetch (context Build: compaction + working set)
//	② Prompt build
//	③ Guard.in
//	④ Model call (streaming with retry)
//	⑤ Guard.out
//	⑥ Policy/Approval + ⑦ Tool execution (concurrent)
//	⑧ Memory store (Commit)
func (rt *Runtime) run(ctx context.Context, session hwcloud.Session, prefix []hwcloud.Message, input hwcloud.Message, ch chan<- hwcloud.StreamEvent) (_ *hwcloud.RunResult, runErr error) {
	// Snapshot the mutable config under the lock: SetModel/SetMaxTurns
	// (session config RPCs, wasm exports) can run concurrently from the
	// serve loop or tool callbacks. The current turn uses the snapshot;
	// the next turn picks up any change.
	rt.mu.RLock()
	maxTurns := rt.cfg.MaxTurns
	cfgModel := rt.cfg.Model
	rt.mu.RUnlock()
	if maxTurns <= 0 {
		maxTurns = 20 // agent.New's default; guard for zero-value configs
	}

	// Resolve model for this run (Model() reads it under the same lock).
	rt.mu.Lock()
	rt.runModel = cfgModel
	if session.Model != nil {
		rt.runModel = session.Model
	}
	rt.mu.Unlock()

	result := &hwcloud.RunResult{}
	rt.state.SessionID = session.ID

	// Trajectory identity: every StageEvent/DecisionEvent this run emits is
	// stamped with runID so consumers can reassemble the full trajectory
	// (and join it to this RunResult at the end). TurnID starts at -1
	// (pre-loop: guard.in, input commit) and is updated to the real turn
	// index at the top of each loop iteration. Inject the DecisionObserver
	// (if the configured RunObserver also implements it) into ctx so leaf
	// packages — context/, provider/ — can emit decision events without a
	// struct field, mirroring the SessionFromContext pattern.
	//
	// ParentRunID link: a team/orchestrator run stamps its own RunID into
	// ctx before calling the child's run(); the child must read it BEFORE
	// re-stamping its own RunID, because WithRunInfo shadows (replaces) the
	// ctx value rather than merging. Preserving it here lets every child
	// event carry the parent's RunID, so a multi-agent trajectory reassembles
	// via (parent_run_id → child RunID/ParentRunID). Empty for solo runs.
	// WithSession injects the session so leaf packages that have no session
	// in scope (governance/context/prepare/provider) recover it via
	// SessionFromContext for the DecisionEvent.SessionID join key.
	runID := uuid.NewString()
	result.RunID = runID
	prev := hwcloud.RunInfoFromContext(ctx)
	result.ParentRunID = prev.RunID
	ctx = hwcloud.WithRunInfo(ctx, hwcloud.RunInfo{RunID: runID, TurnID: -1, ParentRunID: prev.RunID})
	ctx = hwcloud.WithSession(ctx, session)
	if d, ok := rt.deps.Observer.(hwcloud.DecisionObserver); ok {
		ctx = hwcloud.WithDecisionObserver(ctx, d)
	}

	// Track last request/response for RunHooks.OnAgentEnd.
	var lastReq hwcloud.ChatCompletionRequest
	var lastResp *hwcloud.ChatCompletionResponse
	var agentHookState any

	// ── RunHooks.OnAgentStart ──
	// Runs BEFORE guard.in so the OTel agent.run span is in ctx when
	// stage observers fire — otherwise guard.in becomes a root span
	// disconnected from the agent.run trace.
	if rt.deps.Hooks != nil {
		var err error
		ctx, agentHookState, err = rt.deps.Hooks.OnAgentStart(ctx, lastReq)
		if err != nil {
			// Hook infrastructure failure must not kill the run, but must
			// not stay silent either.
			slog.Warn("OnAgentStart hook failed", "error", err)
		}
	}
	defer func() {
		if rt.deps.Hooks != nil {
			rt.deps.Hooks.OnAgentEnd(ctx, lastReq, lastResp, runErr, agentHookState)
		}
	}()

	// Guard.in BEFORE persisting the input: a blocked input must never
	// reach the store — it would be re-read into the model's context next
	// turn (same principle as the guard.out comment below).
	gStart := time.Now()
	rt.observe(ctx, hwcloud.StageGuardIn, "enter", nil, time.Time{}, nil)
	ok, msg := rt.guardInput(ctx, input)
	// The block reason rides in Err for blocked runs (observers read
	// Err.Message); allowed is structured so aggregators can count
	// pass/block without parsing error strings.
	rt.observe(ctx, hwcloud.StageGuardIn, "leave", map[string]any{"allowed": ok}, gStart, msg)
	if !ok {
		return nil, msg
	}

	// Append initial user input to memory.
	rt.commit(ctx, session, input)
	rt.logEvent(ctx, session.ID, eventbus.EventUserInput, input.Content, nil)

	var workingMessages []hwcloud.Message
	var ac *ctxpkg.AgentContext

	// ── Main loop ──
	for turn := 0; turn < maxTurns; turn++ {
		result.TurnCount = turn + 1
		// Refresh RunInfo so this turn's events carry the correct TurnID.
		// RunID is stable across the run; only TurnID changes per iteration.
		// ParentRunID is read back from ctx (stamped once at run() entry)
		// so it survives the per-turn re-stamp — WithRunInfo shadows.
		ri := hwcloud.RunInfoFromContext(ctx)
		ctx = hwcloud.WithRunInfo(ctx, hwcloud.RunInfo{RunID: runID, TurnID: turn, ParentRunID: ri.ParentRunID})
		// Cancel compensation: persist unresolved tool results.
		if ctx.Err() != nil {
			rt.cancelCompensation(ctx, session, workingMessages, ch)
			return nil, ctx.Err()
		}

		if turn == 0 {
			// ① Memory fetch — compaction + working set (turn 1 only).
			messages, ci, err := rt.prepareMemory(ctx, session, ch)
			if err != nil {
				runErr = err
				return nil, err
			}
			workingMessages = append(workingMessages, messages...)
			// Strip the just-appended input so history is history.
			workingMessages = ctxpkg.ExcludeInput(workingMessages, input)
			// Orphan cleanup: leading assistant tool_calls without results.
			workingMessages = ctxpkg.TrimOrphanToolCalls(workingMessages)
			rt.compressed = ci.compressed
			rt.emitCompactionResult(ctx, ch, ci, "compaction failed")
			// ② Prompt build (turn 1: prefix + input).
			promptMsgs := append(append([]hwcloud.Message{}, prefix...), input)
			workingMessages = append(workingMessages, promptMsgs...)
			// ① Context Build: assemble the AgentContext (knowledge recall,
			// skill match, resources) — the single input to prompt assembly.
			ac, err = rt.context.Build(ctx, ctxpkg.BuildRequest{
				Session:    session,
				Goal:       input.Content,
				WorkingSet: workingMessages,
			})
			if err != nil {
				return nil, fmt.Errorf("context build: %w", err)
			}
		}

		// ① Tool-turn re-compaction (turn > 0): prepareMemory only ran on
		// turn 0, so the working set has grown by raw append since. Re-run
		// prepareMemory to fetch the full post-summary history from the
		// store, compact overflow, and trim back to budget. This is the
		// same path turn 0 takes — alignment (from/ThroughIndex/globalCutoff)
		// is handled internally by prepareMemory reading from the store, NOT
		// by indexing into workingMessages (which contains un-committed prefix
		// and orphan-trimmed heads that would misalign a manual globalCutoff).
		// Without this, accumulated tool results / cross-session history grows
		// unbounded and the prompt exceeds the context window.
		if turn > 0 && rt.deps.SessionStore != nil {
			messages, ci, err := rt.prepareMemory(ctx, session, ch)
			if err != nil {
				slog.Error("tool-turn memory prepare failed", "error", err)
				// Best-effort: keep the existing working set. The hard
				// window check below surfaces any overflow (fail-loud).
			} else {
				workingMessages = ctxpkg.TrimOrphanToolCalls(messages)
				rt.compressed = ci.compressed
				rt.emitCompactionResult(ctx, ch, ci, "tool-turn compaction failed")
			}
		}

		// Keep the AgentContext in sync with the growing working set.
		ac.Messages = workingMessages

		// ② Prompt build — consumes the AgentContext (v2.0: Context is the
		// agent input; the kernel never assembles prompt fragments itself).
		pStart := time.Now()
		rt.observe(ctx, hwcloud.StagePromptBuild, "enter", nil, time.Time{}, nil)
		prompt, err := rt.buildPrompt(ctx, session, ac)
		// Composition counts (skills/memories/resources) let observers
		// attribute prompt growth to a context source without inspecting
		// the content.
		rt.observe(ctx, hwcloud.StagePromptBuild, "leave", map[string]any{
			"messages":  len(prompt),
			"tokens":    hwcloud.CountMessages(hwcloud.TokenizerModelID(rt.runModel), prompt),
			"skills":    len(ac.Skills),
			"memories":  len(ac.Memories),
			"resources": len(ac.Resources),
		}, pStart, err)
		if err != nil {
			slog.Error("prompt build failed", "error", err)
			chSend(ctx, ch, hwcloud.StreamEvent{Type: hwcloud.StreamError, Error: err})
			return nil, err
		}
		// Hard limit check: a prompt that exceeds the model's context window
		// is a configuration problem, not something to paper over by
		// silently dropping messages (dropped messages stay in the store,
		// get re-read next turn, and the summary references history the
		// model never saw). Fail loudly instead — compaction (working-set
		// budget) and MaxCompressedTokens (summary cap) are the correct
		// controls.
		if rt.runModel != nil && rt.runModel.ContextWindow() > 0 {
			modelID := hwcloud.TokenizerModelID(rt.runModel)
			if n := hwcloud.CountMessages(modelID, prompt); n > rt.runModel.ContextWindow() {
				err := fmt.Errorf("prompt exceeds model context window: %d > %d tokens (increase MaxWorkingTokens or reduce system prompts / compressed summary)", n, rt.runModel.ContextWindow())
				chSend(ctx, ch, hwcloud.StreamEvent{Type: hwcloud.StreamError, Error: err})
				return nil, err
			}
		}
		req := rt.buildModelRequest(session, prompt)
		lastReq = req

		// Trace: dump the exact request sent to the model — every message's
		// content — for debugging prompt construction. Trace level keeps
		// it out of normal logs (enable via settings log.level=trace);
		// prompt content may include user data and secrets, hence the
		// explicit opt-in level.
		slog.Log(ctx, LevelTrace, "model request detail", "model", req.Model, "messages", len(req.Messages), "tools", len(req.Tools))
		for i, m := range req.Messages {
			slog.Log(ctx, LevelTrace, "  message",
				"i", i,
				"role", m.Role,
				"content", m.Content,
				"reasoning", m.ReasoningContent,
				"tool_calls", len(m.ToolCalls),
				"tool_call_id", m.ToolCallID,
				"name", m.Name,
			)
		}

		// ④ Model call. The stage wraps the whole call — retries and
		// streaming included — so the observed duration is the real
		// model latency of the turn.
		mStart := time.Now()
		rt.observe(ctx, hwcloud.StageModelCall, "enter", map[string]any{"model": req.Model}, time.Time{}, nil)
		resp, retries, err := rt.callModel(ctx, req, ch)
		mDetail := map[string]any{"model": req.Model, "retries": retries}
		if resp != nil {
			mDetail["choices"] = len(resp.Choices)
			if len(resp.Choices) > 0 {
				if fr := resp.Choices[0].FinishReason; fr != "" {
					mDetail["finish_reason"] = fr
				}
				mDetail["tool_calls"] = len(resp.Choices[0].Message.ToolCalls)
				// The full model output is observation data — observers
				// are read-only (unlike hooks which mutate), so no
				// truncation here; consumers filter as they see fit.
				if content := resp.Choices[0].Message.Content; content != "" {
					mDetail["content_chars"] = len([]rune(content))
					mDetail["content"] = content
				}
				if rc := resp.Choices[0].Message.ReasoningContent; rc != "" {
					mDetail["reasoning_chars"] = len([]rune(rc))
				}
			}
			if u := resp.Usage; u.TotalTokens > 0 {
				mDetail["total_tokens"] = u.TotalTokens
				mDetail["prompt_tokens"] = u.PromptTokens
				mDetail["completion_tokens"] = u.CompletionTokens
				mDetail["cache_read_tokens"] = u.CacheReadTokens
			}
		}
		rt.observe(ctx, hwcloud.StageModelCall, "leave", mDetail, mStart, err)
		if err != nil {
			if ctx.Err() != nil {
				chSend(ctx, ch, hwcloud.StreamEvent{Type: hwcloud.StreamAborted, Error: ctx.Err()})
				return nil, ctx.Err()
			}
			chSend(ctx, ch, hwcloud.StreamEvent{Type: hwcloud.StreamError, Error: err})
			return nil, err
		}
		if len(resp.Choices) == 0 {
			err := fmt.Errorf("model returned no choices")
			chSend(ctx, ch, hwcloud.StreamEvent{Type: hwcloud.StreamError, Error: err})
			return nil, err
		}
		lastResp = resp
		result.Usage = resp.Usage

		choice := resp.Choices[0].Message
		finishReason := resp.Choices[0].FinishReason

		// ⑤ Guard.out — on model output. Applied BEFORE the message is
		// persisted, streamed, or added to the working set: a blocked
		// content must never reach the store (it would be re-read into the
		// model's context next turn), the result, or the tool-call event.
		// (Live-streamed text deltas are already on the wire and cannot be
		// recalled — the guard governs what is stored and reused.)
		goStart := time.Now()
		rt.observe(ctx, hwcloud.StageGuardOut, "enter", map[string]any{"target": "model"}, time.Time{}, nil)
		blocked, reason, tripwire := rt.guardOutput(ctx, choice)
		// reason + tripwire are structured for security auditing: which
		// rule blocked (or tripped) the output.
		rt.observe(ctx, hwcloud.StageGuardOut, "leave", map[string]any{"target": "model", "blocked": blocked, "reason": reason, "tripwire": tripwire}, goStart, nil)
		if blocked {
			if tripwire {
				return nil, fmt.Errorf("output guard tripwire: %s", reason)
			}
			choice.Content = "[blocked: " + reason + "]"
		}
		result.FinalOutput = choice.Content

		for _, tc := range choice.ToolCalls {
			chSend(ctx, ch, hwcloud.StreamEvent{Type: hwcloud.StreamToolCall, Message: hwcloud.Message{Role: hwcloud.RoleAssistant, Content: choice.Content, ToolCalls: []hwcloud.ToolCall{tc}}})
		}
		result.Messages = append(result.Messages, choice)
		workingMessages = append(workingMessages, choice)
		rt.commit(ctx, session, choice)
		rt.logEvent(ctx, session.ID, eventbus.EventAssistantMessage, choice.Content, nil)

		// No tool calls: stop.
		if len(choice.ToolCalls) == 0 {
			if finishReason != "" && finishReason != "stop" {
				result.StopReason = finishReason
				msg := hwcloud.Message{Role: hwcloud.RoleSystem, Content: "Model stopped with reason: " + finishReason}
				workingMessages = append(workingMessages, msg)
				rt.commit(ctx, session, msg)
			}
			break
		}

		// ⑥⑦ Tool execution (approval + concurrent execution).
		results := rt.executeTools(ctx, session, choice.ToolCalls, ch)
		for _, r := range results {
			// Guard.out on each tool result (same per-item granularity as
			// tool.execute).
			goStart := time.Now()
			rt.observe(ctx, hwcloud.StageGuardOut, "enter", map[string]any{"target": "tool"}, time.Time{}, nil)
			blocked, reason, tripwire := rt.guardOutput(ctx, r)
			rt.observe(ctx, hwcloud.StageGuardOut, "leave", map[string]any{"target": "tool", "blocked": blocked, "reason": reason, "tripwire": tripwire}, goStart, nil)
			if blocked {
				if tripwire {
					return nil, fmt.Errorf("output guard tripwire on tool result: %s", reason)
				}
				r.Content = "[blocked: " + reason + "]"
			}
			result.Messages = append(result.Messages, r)
			workingMessages = append(workingMessages, r)
			rt.commit(ctx, session, r)
			chSend(ctx, ch, hwcloud.StreamEvent{Type: hwcloud.StreamToolResult, Message: r})
		}

		// Handoff: an executed tool with EndTurn terminates the turn.
		if rt.checkHandoff(choice.ToolCalls, results) {
			result.StopReason = "handoff"
			break
		}
	}

	// The turn loop can end while the run was cancelled during the LAST
	// turn's tool execution — the loop-top cancellation check (and its
	// compensation) never runs again. Surface the cancellation instead of
	// reporting a normal completion: StreamDone below would otherwise be
	// randomly dropped (cancelled ctx) and the client never sees a
	// terminal event.
	if ctx.Err() != nil {
		rt.cancelCompensation(ctx, session, workingMessages, ch)
		return nil, ctx.Err()
	}

	result.ContextWindow = rt.runModel.ContextWindow()
	rt.state.Turn = result.TurnCount
	// Self-evolution: store durable knowledge from this finished run.
	// Knowledge is user-level (cross-session long-term memory) — the
	// session ID is NOT part of the scope, or every new session would be
	// filtered away from the knowledge it should recall.
	//
	// The call is fire-and-forget: AsyncExtractor (the standard wiring)
	// enqueues and extracts on its background worker, so this never
	// delays the run's return. Applications wire Deps.Extractor once per
	// server (never per run).
	if rt.deps.Extractor != nil && len(workingMessages) > 0 {
		rt.deps.Extractor.Extract(ctx, ctxpkg.ContextScope{
			UserID: session.UserID,
		}, workingMessages)
	}
	chSend(ctx, ch, hwcloud.StreamEvent{Type: hwcloud.StreamDone, Result: result})
	return result, nil
}

// logEvent records an audit event (no-op without a logger).
func (rt *Runtime) logEvent(ctx context.Context, sessionID string, typ eventbus.EventType, payload any, meta map[string]string) {
	if rt.deps.EventLogger == nil {
		return
	}
	ri := hwcloud.RunInfoFromContext(ctx)
	rt.deps.EventLogger.Append(ctx, eventbus.Event{
		SessionID: sessionID,
		Type:      typ,
		Payload:   payload,
		Metadata:  meta,
		RunID:     ri.RunID,
		TurnID:    ri.TurnID,
	})
}

// chSend is a cancellable blocking event send (bounded backpressure):
// a slow consumer backpressures the run instead of silently dropping
// events. The run context cancels on client disconnect (the REST layer
// derives it from the request context), so a dead consumer cannot
// deadlock the producer — the send aborts on ctx.Done().
func chSend(ctx context.Context, ch chan<- hwcloud.StreamEvent, ev hwcloud.StreamEvent) {
	if ch == nil {
		return
	}
	select {
	case ch <- ev:
	case <-ctx.Done():
	}
}

// emitCompactionResult sends the StreamThought follow-up that closes the
// "context compacting..." hint prepareMemory emitted before the Compact
// call. It is a no-op when no compaction was attempted this pass
// (ci.attempted == false — budget fit, nothing ran), so turns that never
// stalled produce no compaction chatter at all.
//
// The success message mirrors the /compact slash echo (acp/commands.go:
// "Compacted N messages → summary (freed ~K tokens).") so ACP clients see
// the same wording whether compaction ran automatically or on demand.
// On failure it names the error and notes the degraded tail-trim fallback
// prepare.go already applied, so the user understands why older history
// is absent from this turn's prompt.
func (rt *Runtime) emitCompactionResult(ctx context.Context, ch chan<- hwcloud.StreamEvent, ci compactionInfo, slogMsg string) {
	if !ci.attempted {
		return
	}
	if ci.err != nil {
		slog.Error(slogMsg, "error", ci.err)
		chSend(ctx, ch, hwcloud.StreamEvent{
			Type: hwcloud.StreamThought,
			Text: fmt.Sprintf("context compaction failed: %v (degraded — older messages dropped from prompt)\n", ci.err),
		})
		return
	}
	if ci.count > 0 {
		chSend(ctx, ch, hwcloud.StreamEvent{
			Type: hwcloud.StreamThought,
			Text: fmt.Sprintf("Compacted %d messages → summary (freed ~%d tokens)\n", ci.count, ci.freedTokens),
		})
	}
}

// guardInput runs the input guard once per run. Returns (ok, error message).
func (rt *Runtime) guardInput(ctx context.Context, input hwcloud.Message) (bool, error) {
	if rt.cfg.InGuard == nil {
		return true, nil
	}
	res := rt.cfg.InGuard.Check(ctx, governance.GuardInput{Input: input})
	if !res.Allowed {
		return false, fmt.Errorf("input guard blocked: %s", res.Reason)
	}
	if res.Tripwire {
		return false, fmt.Errorf("input guard tripwire: %s", res.Reason)
	}
	return true, nil
}

// guardOutput runs the output guard on a model output or tool result.
// Returns (blocked, reason, tripwire).
func (rt *Runtime) guardOutput(ctx context.Context, msg hwcloud.Message) (bool, string, bool) {
	if rt.cfg.OutGuard == nil {
		return false, "", false
	}
	res := rt.cfg.OutGuard.Check(ctx, governance.GuardOutput{Output: msg})
	if !res.Allowed {
		return true, res.Reason, res.Tripwire
	}
	if res.Tripwire {
		return true, res.Reason, true
	}
	return false, "", false
}

// checkHandoff reports whether an executed tool carried EndTurn.
func (rt *Runtime) checkHandoff(calls []hwcloud.ToolCall, results []hwcloud.Message) bool {
	for i := range calls {
		if i < len(results) {
			if d := rt.toolDef(calls[i].Function.Name); d != nil && d.EndTurn {
				return true
			}
		}
	}
	return false
}

// observe emits a stage event to the observer (no-op if none). StageEvent
// is stamped with the RunID/TurnID recovered from ctx so every event groups
// into its run trajectory; events emitted before the turn loop (guard.in,
// input commit) carry TurnID=-1.
func (rt *Runtime) observe(ctx context.Context, stage string, phase string, detail map[string]any, start time.Time, err error) {
	if rt.deps.Observer == nil {
		return
	}
	// "enter" events pass a zero start (callers mark the start separately
	// and pass it to the matching "leave"). time.Since(zero) is ~1.7e9
	// years — a nonsense duration that observers must ignore today; make
	// it explicit zero instead so nothing has to special-case enter.
	d := time.Duration(0)
	if !start.IsZero() {
		d = time.Since(start)
	}
	ri := hwcloud.RunInfoFromContext(ctx)
	rt.deps.Observer.ObserveStage(ctx, hwcloud.StageEvent{
		Name:        stage,
		Phase:       phase,
		Detail:      detail,
		Duration:    d,
		Err:         err,
		RunID:       ri.RunID,
		TurnID:      ri.TurnID,
		ParentRunID: ri.ParentRunID,
	})
}

// commit appends a message to memory (Transient messages and nil memory skip).
func (rt *Runtime) commit(ctx context.Context, session hwcloud.Session, msg hwcloud.Message) {
	if msg.Transient || rt.deps.SessionStore == nil {
		return
	}
	// A cancelled run still persists what it already produced. The loop
	// commits a message only after it EXISTS (the model call returned, the
	// tool job finished with a real result — executeTools waits for jobs
	// to complete even when cancelled). A cancelled ctx would fail the
	// backend's transaction immediately and the message would be lost:
	// the assistant tool_calls message lands in the store but its result
	// does not, and cancelCompensation skips it (it sees the result in the
	// in-memory working set) — an orphan tool_call in history, re-read by
	// the model next turn. Same deliberate background-ctx pattern as
	// cancelCompensation. The write is a bounded local operation.
	if ctx.Err() != nil {
		ctx = context.Background()
	}
	// Stamp wall-clock at commit time (UTC RFC3339 on the wire). Only set
	// when unset so test fixtures and the cancel-compensation path (which
	// builds its own messages) can supply their own. Uses a *time.Time so
	// omitempty actually works: nil is omitted on the wire, whereas a zero
	// value time.Time would serialize as 0001-01-01T00:00:00Z.
	if msg.CreatedAt == nil {
		now := time.Now().UTC()
		msg.CreatedAt = &now
	}
	start := time.Now()
	rt.observe(ctx, hwcloud.StageMemoryAppend, "enter", nil, time.Time{}, nil)
	err := rt.deps.SessionStore.Append(ctx, session.ID, msg)
	// Metadata only (role + size), never the content: the audit trail
	// must show WHAT was written without duplicating the payload.
	rt.observe(ctx, hwcloud.StageMemoryAppend, "leave", map[string]any{
		"role":  string(msg.Role),
		"chars": len([]rune(msg.Content)),
	}, start, err)
	if err != nil {
		slog.Error("memory append failed", "error", err)
	}
}
