package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/agent"
	"golang.org/x/sync/errgroup"
)

// executor runs a PlanDef to completion.
type executor struct {
	config     PlanConfig
	agents     map[string]agent.AgentRunner
	agentInfos []agent.AgentInfo
	model      hwcloud.Model // for summarisation
	sessionID  string          // base session id for step isolation
	// planRunID is the plan-level run ID (#1): stable across replan
	// (all replan steps join the same planRunID) and resume (read back
	// from PlanState.PlanRunID). Stamped on ctx by Plan.execute so each
	// step's child kernel.run() reads it as ParentRunID; echoed in
	// PlanResult.PlanRunID by buildResult.
	planRunID string
}

// ── Entry point ──

func (e *executor) execute(ctx context.Context, def *PlanDef, state *PlanState, eventCh chan<- PlanEvent) (*PlanResult, error) {
	state.Status = PlanStatusRunning
	state.UpdatedAt = time.Now()

	// Resolve final steps: those explicitly marked Final, or leaf nodes.
	finalSteps := make(map[string]bool)
	for _, s := range def.Steps {
		if s.Final {
			finalSteps[s.ID] = true
		}
	}

	// Replan loop.
	maxReplans := e.config.MaxReplans
	if maxReplans <= 0 {
		maxReplans = 3
	}

	var totalUsage hwcloud.Usage

	for {
		select {
		case <-ctx.Done():
			state.Status = PlanStatusCancelled
			if eventCh != nil {
				eventCh <- PlanEvent{Type: PlanEventError, ErrText: ctx.Err().Error()}
			}
			return nil, ctx.Err()
		default:
		}

		// Reset pending steps that aren't done.
		for _, s := range def.Steps {
			if sr, ok := state.Results[s.ID]; !ok || sr == nil {
				state.Results[s.ID] = &StepResult{Status: StepStatusPending}
			}
		}

		// Execute batches.
		batchResult, gateStepID, err := e.executeBatches(ctx, def, state, finalSteps, &totalUsage, eventCh)
		if err != nil {
			state.Status = PlanStatusFailed
			return nil, err
		}
		if batchResult != nil {
			totalUsage.PromptTokens = batchResult.Usage.PromptTokens
			totalUsage.CompletionTokens = batchResult.Usage.CompletionTokens
			totalUsage.TotalTokens = batchResult.Usage.TotalTokens
		}

		// Gate pause: a gate step completed successfully, pause for human approval.
		if gateStepID != "" {
			state.UpdatedAt = time.Now()
			if eventCh != nil {
				eventCh <- PlanEvent{
					Type:   PlanEventWaitingRetry,
					StepID: gateStepID,
				}
			}
			return &PlanResult{Usage: totalUsage, PlanRunID: e.planRunID}, nil // paused — caller can resume via ExecuteWithState
		}

		// Check for failures.
		failedID := e.findFailed(def, state)
		if failedID == "" {
			// All done.
			state.Status = PlanStatusDone
			state.UpdatedAt = time.Now()
			finalResult := e.buildResult(def, state, totalUsage)
			if eventCh != nil {
				eventCh <- PlanEvent{Type: PlanEventDone, Text: finalResult.FinalOutput}
			}
			return finalResult, nil
		}

		// If auto-replan is disabled, pause for manual intervention.
		// The caller sees (nil, nil) and knows execution is paused, not failed.
		if !e.config.AutoReplan {
			state.UpdatedAt = time.Now()
			if eventCh != nil {
				failedSR := state.Results[failedID]
				errText := ""
				if failedSR != nil {
					errText = failedSR.Error
				}
				eventCh <- PlanEvent{
					Type:    PlanEventWaitingRetry,
					StepID:  failedID,
					ErrText: errText,
				}
			}
			return nil, nil // paused — caller can resume via ExecuteWithState
		}

		// Replan.
		state.ReplanCount++
		if state.ReplanCount > maxReplans {
			state.Status = PlanStatusFailed
			return nil, fmt.Errorf("max replans exceeded (%d)", maxReplans)
		}

		if eventCh != nil {
			eventCh <- PlanEvent{Type: PlanEventReplanning, StepID: failedID}
		}

		newDef, err := e.replanAfterFailure(ctx, def, state, failedID)
		if err != nil {
			state.Status = PlanStatusFailed
			return nil, fmt.Errorf("replan failed: %w", err)
		}

		def = newDef
		state.Steps = def.Steps
		state.UpdatedAt = time.Now()
	}
}

// ── Batch execution ──

func (e *executor) executeBatches(ctx context.Context, def *PlanDef, state *PlanState, finalSteps map[string]bool, totalUsage *hwcloud.Usage, eventCh chan<- PlanEvent) (*PlanResult, string, error) {
	// Build adjacency and in-degree for remaining (pending) steps.
	pending := make(map[string]StepDef)
	for _, s := range def.Steps {
		sr := state.Results[s.ID]
		if sr == nil || sr.Status == StepStatusPending || sr.Status == StepStatusFailed {
			pending[s.ID] = s
		}
	}

	if len(pending) == 0 {
		return nil, "", nil
	}

	// Topological sort of pending steps.
	batches := topoSortPending(pending)

	for _, batch := range batches {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		default:
		}

		// Run batch in parallel.
		batchUsage, batchFailed, err := e.runBatch(ctx, batch, state, eventCh)
		if err != nil {
			return nil, "", err
		}
		totalUsage.PromptTokens += batchUsage.PromptTokens
		totalUsage.CompletionTokens += batchUsage.CompletionTokens
		totalUsage.TotalTokens += batchUsage.TotalTokens
		if batchFailed {
			// A step failed — stop batch execution, let caller check and replan.
			return &PlanResult{Usage: *totalUsage, PlanRunID: e.planRunID}, "", nil
		}

		// Check if any completed step in this batch is a gate.
		for _, sid := range batch {
			s, ok := pending[sid]
			if !ok || !s.Gate {
				continue
			}
			sr := state.Results[sid]
			if sr != nil && sr.Status == StepStatusDone {
				return &PlanResult{Usage: *totalUsage}, sid, nil
			}
		}
	}

	return nil, "", nil
}

// runBatch executes a single topological batch concurrently via errgroup.
// Returns accumulated usage from all steps in the batch, a failed flag,
// and an error (only for context cancellation, never for step failures).
// When one step fails, errgroup cancels the derived context so sibling
// goroutines check gCtx.Err() and exit early — no manual TOCTOU flags.
func (e *executor) runBatch(ctx context.Context, batch []string, state *PlanState, eventCh chan<- PlanEvent) (hwcloud.Usage, bool, error) {
	concurrency := e.config.MaxConcurrency
	if concurrency <= 0 {
		concurrency = 8
	}

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	var (
		mu     sync.Mutex
		usage  hwcloud.Usage
		failed bool
	)

	for _, stepID := range batch {
		stepID := stepID
		g.Go(func() error {
			// Early exit if a sibling already failed — errgroup cancelled the derived ctx.
			if err := gCtx.Err(); err != nil {
				return err
			}

			sr := e.executeStep(gCtx, stepID, state, eventCh)

			mu.Lock()
			usage.PromptTokens += sr.Usage.PromptTokens
			usage.CompletionTokens += sr.Usage.CompletionTokens
			usage.TotalTokens += sr.Usage.TotalTokens
			if sr.Status == StepStatusFailed {
				failed = true
			}
			mu.Unlock()

			if sr.Status == StepStatusFailed {
				return fmt.Errorf("step %q failed: %s", stepID, sr.Error)
			}
			return nil
		})
	}

	// Wait reports the first error. When ctx is cancelled before any
	// goroutine reaches executeStep (e.g. timeout), failed remains false
	// and all step statuses stay pending. Without this check, the caller
	// sees "all done" for steps that never ran.
	if err := g.Wait(); err != nil {
		// Parent context cancelled — propagate immediately. Step-level
		// statuses may be inconsistent (some goroutines never executed)
		// but the caller discards state on cancellation anyway.
		if ctx.Err() != nil {
			return usage, failed, ctx.Err()
		}
		// Otherwise a step failed — failed was already set by the
		// goroutine that returned the first error; gCtx was cancelled to
		// bail out siblings, which is expected.
	}
	return usage, failed, nil
}

// ── Single step execution ──

func (e *executor) executeStep(ctx context.Context, stepID string, state *PlanState, eventCh chan<- PlanEvent) *StepResult {
	step := e.findStep(state.Steps, stepID)
	if step == nil {
		return &StepResult{Status: StepStatusFailed, Error: fmt.Sprintf("step %q not found", stepID)}
	}

	runner, ok := e.agents[step.Agent]
	if !ok {
		return &StepResult{Status: StepStatusFailed, Error: fmt.Sprintf("agent %q not found", step.Agent)}
	}

	sr := state.Results[stepID]
	if sr == nil {
		// executeBatches pre-seeds every step's result before the batch
		// runs. Never write the map from a batch goroutine: concurrent
		// map writes crash the runtime (fatal: concurrent map writes).
		return &StepResult{Status: StepStatusFailed, Error: fmt.Sprintf("step %q not pre-seeded", stepID)}
	}

	maxRetries := step.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	for retry := 0; retry <= maxRetries; retry++ {
		sr.Retries = retry
		sr.Status = StepStatusRunning
		sr.StartTime = time.Now()

		if eventCh != nil {
			eventCh <- PlanEvent{Type: PlanEventStepStart, StepID: stepID, Agent: step.Agent}
		}

		// Build step context from dependency results.
		stepCtx := e.buildStepContext(state.Steps, state.Results, stepID)

		// Isolated session so this step doesn't see other steps' internal history.
		stepSession := hwcloud.Session{
			ID: e.sessionID + "/steps/" + stepID,

			CreatedAt: time.Now(),
		}

		// Build prefix: system message with plan context.
		prefix := []hwcloud.Message{
			{Role: hwcloud.RoleSystem, Content: formatStepContext(stepCtx)},
		}
		input := hwcloud.UserMessage("Complete your task as described above.")

		// Execute with timeout.
		var result *hwcloud.RunResult
		var runErr error

		// Create a per-iteration timeout context. Cancel is called explicitly
		// at the end of each iteration rather than deferred — defer in a loop
		// stacks callbacks to function exit, leaking timers across retries.
		var runCtx context.Context
		var cancel context.CancelFunc
		if e.config.StepTimeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, e.config.StepTimeout)
		} else {
			runCtx = ctx
		}

		if eventCh != nil {
			result, runErr = e.runStepStreaming(runCtx, runner, stepSession, prefix, input, stepID, eventCh)
		} else {
			result, runErr = runner.RunWithPrefix(runCtx, stepSession, prefix, input)
		}

		if cancel != nil {
			cancel()
		}

		if runErr != nil {
			sr.Status = StepStatusFailed
			sr.Error = runErr.Error()
			sr.EndTime = time.Now()
			if eventCh != nil {
				eventCh <- PlanEvent{Type: PlanEventStepFailed, StepID: stepID, Agent: step.Agent, Result: sr, ErrText: sr.Error}
			}
			if isPermanentError(runErr) {
				return sr // don't retry permanent failures
			}
			continue // retry transient errors
		}

		// Generate summary.
		summary, sumErr := e.summarize(ctx, step, result.FinalOutput)
		if sumErr != nil {
			// Non-fatal: emit a warning then fall back to truncated output.
			if eventCh != nil {
				eventCh <- PlanEvent{
					Type:   PlanEventTextDelta,
					StepID: stepID,
					Text:   fmt.Sprintf("[warning: summarization failed (%v), using truncated output]", sumErr),
				}
			}
			summary = truncateStr(result.FinalOutput, 500)
		}

		sr.Status = StepStatusDone
		sr.Summary = summary
		sr.FinalOutput = result.FinalOutput
		sr.Usage = result.Usage
		sr.Error = ""
		sr.EndTime = time.Now()
		// #1: capture the child agent's run ID so the step result joins
		// its agent trajectory (RunResult.RunID). result.ParentRunID ==
		// e.planRunID (the child kernel.run() read the plan-stamped ctx
		// as its ParentRunID) — a consumer reassembles by filtering
		// child events on ParentRunID == PlanResult.PlanRunID.
		sr.RunID = result.RunID

		if eventCh != nil {
			eventCh <- PlanEvent{Type: PlanEventStepDone, StepID: stepID, Agent: step.Agent, Result: sr}
		}

		return sr
	}

	// All retries exhausted.
	sr.Status = StepStatusFailed
	sr.EndTime = time.Now()
	if eventCh != nil {
		eventCh <- PlanEvent{Type: PlanEventStepFailed, StepID: stepID, Agent: step.Agent, Result: sr, ErrText: sr.Error}
	}
	return sr
}

// runStepStreaming forwards agent stream events to the plan event channel.
func (e *executor) runStepStreaming(ctx context.Context, runner agent.AgentRunner, session hwcloud.Session, prefix []hwcloud.Message, input hwcloud.Message, stepID string, eventCh chan<- PlanEvent) (*hwcloud.RunResult, error) {
	ch := runner.RunStreamWithPrefix(ctx, session, prefix, input)
	var result *hwcloud.RunResult
	for evt := range ch {
		switch evt.Type {
		case hwcloud.StreamThought:
			eventCh <- PlanEvent{Type: PlanEventTextDelta, StepID: stepID, Text: evt.Text}
		case hwcloud.StreamTextDelta:
			eventCh <- PlanEvent{Type: PlanEventTextDelta, StepID: stepID, Text: evt.Text}
		case hwcloud.StreamToolCall:
			pe := PlanEvent{Type: PlanEventToolCall, StepID: stepID}
			if len(evt.Message.ToolCalls) > 0 {
				tc := evt.Message.ToolCalls[0]
				pe.ToolID = tc.ID
				pe.ToolName = tc.Function.Name
				pe.ToolArgs = tc.Function.Arguments
			}
			eventCh <- pe
		case hwcloud.StreamToolProgress:
			eventCh <- PlanEvent{Type: PlanEventToolProgress, StepID: stepID, Text: evt.Text}
		case hwcloud.StreamToolResult:
			eventCh <- PlanEvent{
				Type: PlanEventToolResult, StepID: stepID,
				Text: evt.Message.Content, // tool result text
			}
		case hwcloud.StreamRetrying:
			// non-fatal, silently continue
		case hwcloud.StreamDone:
			result = evt.Result
		case hwcloud.StreamError:
			return nil, evt.Error
		case hwcloud.StreamAborted:
			return nil, fmt.Errorf("step %q aborted: %w", stepID, evt.Error)
		}
	}
	if result == nil {
		return nil, fmt.Errorf("step %q stream ended without result", stepID)
	}
	return result, nil
}

// ── StepContext assembly ──

func (e *executor) buildStepContext(steps []StepDef, results map[string]*StepResult, stepID string) StepContext {
	var self StepDef
	for _, s := range steps {
		if s.ID == stepID {
			self = s
			break
		}
	}

	sc := StepContext{
		Goal: "", // filled in later if needed
		Task: self.Task,
	}

	for _, depID := range self.DependsOn {
		sr, ok := results[depID]
		if !ok || sr == nil {
			continue
		}

		var depStep StepDef
		for _, s := range steps {
			if s.ID == depID {
				depStep = s
				break
			}
		}

		output := sr.FinalOutput
		// Only include full output if it's short enough.
		if len(output) > 2000 {
			output = truncateStr(output, 2000)
		}

		dr := DepResult{
			StepID:    depID,
			AgentName: depStep.Agent,
			Task:      depStep.Task,
			Summary:   sr.Summary,
			Output:    output,
			Success:   sr.Status == StepStatusDone,
			Error:     sr.Error,
		}
		sc.Dependencies = append(sc.Dependencies, dr)
	}

	return sc
}

func formatStepContext(sc StepContext) string {
	var b strings.Builder
	b.WriteString("## Plan Context\n\n")
	if sc.Goal != "" {
		b.WriteString(fmt.Sprintf("**Goal:** %s\n\n", sc.Goal))
	}
	b.WriteString(fmt.Sprintf("**Your task:** %s\n", sc.Task))

	if len(sc.Dependencies) > 0 {
		b.WriteString("\n## Results from Previous Steps\n\n")
		for _, dr := range sc.Dependencies {
			b.WriteString(fmt.Sprintf("### Step %q (%s)\n", dr.StepID, dr.AgentName))
			b.WriteString(fmt.Sprintf("Task: %s\n", dr.Task))
			if dr.Success {
				b.WriteString(fmt.Sprintf("Result: %s\n", dr.Summary))
				if dr.Output != "" && dr.Output != dr.Summary {
					b.WriteString(fmt.Sprintf("\nFull output:\n%s\n", dr.Output))
				}
			} else {
				b.WriteString(fmt.Sprintf("⚠️ This step failed: %s\n", dr.Error))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("---\n\nComplete your task and produce your final answer.")
	return b.String()
}

// ── Summarisation ──

func (e *executor) summarize(ctx context.Context, step *StepDef, output string) (string, error) {
	if e.model == nil {
		return truncateStr(output, 500), nil
	}
	if len(output) < 500 {
		return output, nil
	}

	prompt := fmt.Sprintf(
		"Summarize the following agent output in 2-3 sentences. "+
			"Focus on key decisions, results, and outputs produced.\n\n"+
			"Agent: %s\nTask: %s\n\nOutput:\n%s",
		step.Agent, step.Task, output,
	)

	resp, err := e.model.ChatCompletion(ctx, hwcloud.ChatCompletionRequest{
		Messages: []hwcloud.Message{
			{Role: hwcloud.RoleSystem, Content: "You are a concise summarizer. Return only the summary, no preamble."},
			{Role: hwcloud.RoleUser, Content: prompt},
		},
		MaxTokens: 300,
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return truncateStr(output, 500), nil
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

// ── Helpers ──

func (e *executor) findStep(steps []StepDef, id string) *StepDef {
	for i := range steps {
		if steps[i].ID == id {
			return &steps[i]
		}
	}
	return nil
}

func (e *executor) findFailed(def *PlanDef, state *PlanState) string {
	for _, s := range def.Steps {
		sr := state.Results[s.ID]
		if sr != nil && sr.Status == StepStatusFailed {
			return s.ID
		}
	}
	return ""
}

func (e *executor) buildResult(def *PlanDef, state *PlanState, usage hwcloud.Usage) *PlanResult {
	var finalOutputs []string
	for _, s := range def.Steps {
		sr := state.Results[s.ID]
		if sr == nil || sr.Status != StepStatusDone {
			continue
		}
		// Check if it's a final step (explicitly marked or leaf node with no dependents).
		isFinal := s.Final
		if !isFinal {
			isFinal = true
			for _, other := range def.Steps {
				for _, dep := range other.DependsOn {
					if dep == s.ID {
						isFinal = false
						break
					}
				}
				if !isFinal {
					break
				}
			}
		}
		if isFinal && sr.FinalOutput != "" {
			finalOutputs = append(finalOutputs, sr.FinalOutput)
		}
	}

	finalOutput := strings.Join(finalOutputs, "\n\n")

	steps := make([]StepResult, 0, len(state.Results))
	for _, s := range def.Steps {
		if sr, ok := state.Results[s.ID]; ok && sr != nil {
			steps = append(steps, *sr)
		}
	}

	return &PlanResult{
		Goal:        def.Goal,
		FinalOutput: finalOutput,
		Steps:       steps,
		Usage:       usage,
		ReplanCount: state.ReplanCount,
		// #1: echo the plan-level run ID so a consumer can filter all
		// child DecisionEvents/StageEvents by ParentRunID == PlanRunID.
		// Stable across replan (the replan loop reuses this executor
		// instance, so planRunID is unchanged) and resume (ExecuteWithState
		// re-stamps ctx with state.PlanRunID before re-entering execute).
		PlanRunID: e.planRunID,
	}
}

func (e *executor) replanAfterFailure(ctx context.Context, def *PlanDef, state *PlanState, failedID string) (*PlanDef, error) {
	if e.model == nil {
		// Auto-replan needs a model; without one, fail loudly instead of
		// calling a nil interface (panic).
		return nil, fmt.Errorf("auto-replan requires a model (configure WithModel)")
	}

	// Determine the affected subtree: the failed step + all transitive dependents.
	affected := affectedSteps(def, failedID)

	// Collect context from successful steps.
	var successContext []string
	for _, s := range def.Steps {
		sr := state.Results[s.ID]
		if sr == nil || sr.Status != StepStatusDone {
			continue
		}
		if affected[s.ID] {
			continue
		}
		successContext = append(successContext, fmt.Sprintf(
			"Step %q (%s): %s", s.ID, s.Agent, sr.Summary,
		))
	}

	// Collect failure info.
	sr := state.Results[failedID]
	failureInfo := fmt.Sprintf("Step %q failed: %s", failedID, sr.Error)

	// Build a replan prompt.
	var b strings.Builder
	b.WriteString("## Replanning\n\n")
	b.WriteString(fmt.Sprintf("**Original goal:** %s\n\n", def.Goal))
	b.WriteString(fmt.Sprintf("**Failure:** %s\n\n", failureInfo))

	if len(successContext) > 0 {
		b.WriteString("## Completed Steps (do not regenerate these)\n\n")
		for _, sc := range successContext {
			b.WriteString(fmt.Sprintf("- %s\n", sc))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Steps that Need Replanning\n\n")
	for _, s := range def.Steps {
		if affected[s.ID] {
			b.WriteString(fmt.Sprintf("- %s (agent: %s, original task: %s)\n", s.ID, s.Agent, s.Task))
		}
	}
	// List surviving step IDs so the LLM doesn't reuse them.
	var survivingIDs []string
	for _, s := range def.Steps {
		if !affected[s.ID] {
			survivingIDs = append(survivingIDs, s.ID)
		}
	}
	if len(survivingIDs) > 0 {
		b.WriteString("\n## Surviving Step IDs (DO NOT reuse these)\n")
		b.WriteString(strings.Join(survivingIDs, ", "))
		b.WriteString("\n")
	}

	b.WriteString("\nGenerate a replacement plan for only the failed and affected steps. ")
	b.WriteString("Use NEW unique step IDs — do NOT reuse any surviving IDs listed above. ")
	b.WriteString("The replacement steps should accomplish the same goal as the original affected steps. ")
	b.WriteString("Return ONLY the replacement steps as a JSON array (not a full plan):\n")
	b.WriteString(`[{"id": "...", "agent": "...", "task": "...", "depends_on": [...], "final": false}]`)

	messages := []hwcloud.Message{
		{Role: hwcloud.RoleSystem, Content: plannerSystemPrompt},
		{Role: hwcloud.RoleUser, Content: b.String()},
	}

	fullText, err := modelSyncCall(ctx, e.model, messages)
	if err != nil {
		return nil, fmt.Errorf("replan model call failed: %w", err)
	}

	newSteps, err := parseStepsJSON(fullText)
	if err != nil {
		return nil, fmt.Errorf("replan: %w", err)
	}

	// Merge: remove affected steps, add new ones.
	merged := make([]StepDef, 0, len(def.Steps)-len(affected)+len(newSteps))
	for _, s := range def.Steps {
		if !affected[s.ID] {
			merged = append(merged, s)
		}
	}
	merged = append(merged, newSteps...)

	newDef := &PlanDef{Goal: def.Goal, Steps: merged}

	// Validate the merged plan.
	agentNames := make(map[string]bool)
	for _, a := range e.agentInfos {
		agentNames[a.Name] = true
	}
	if err := Validate(newDef, agentNames); err != nil {
		return nil, fmt.Errorf("replan validation failed: %w", err)
	}

	// Reset results for affected steps.
	for id := range affected {
		delete(state.Results, id)
	}

	return newDef, nil
}

// isPermanentError returns true for errors that won't be fixed by retrying.
// It mirrors the Runner's retry logic: context cancellation is always permanent,
// and non-RetryableError is permanent (the model/runner layer already classifies
// transient errors via hwcloud.RetryableError).
// isPermanentError reports whether a step failure should skip retry and go
// straight to replan. Transient failures — RetryableError (rate-limit/5xx
// wrapped by the model layer), timeouts, network errors — are retried up
// to MaxRetries first; only deterministic failures (logic/argument errors)
// are permanent. Timeouts were previously permanent, so a slow step (the
// step-level deadline is re-created per retry, giving each attempt a full
// window) or a flaky network skipped retry and triggered the much more
// expensive replan instead.
func isPermanentError(err error) bool {
	if err == nil {
		return false // no error is not permanent
	}
	if errors.Is(err, context.Canceled) {
		// Parent cancelled — retrying cannot help.
		return true
	}
	var re *hwcloud.RetryableError
	if errors.As(err, &re) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return false
	}
	return true
}

// ── DAG helpers ──

// topoSortPending sorts pending step IDs into topological batches.
func topoSortPending(pending map[string]StepDef) [][]string {
	// Build in-degree map (only counting pending dependencies).
	inDegree := make(map[string]int)
	adj := make(map[string][]string)

	for id := range pending {
		if _, ok := inDegree[id]; !ok {
			inDegree[id] = 0
		}
	}

	for id, s := range pending {
		for _, dep := range s.DependsOn {
			if _, ok := pending[dep]; ok {
				inDegree[id]++
				adj[dep] = append(adj[dep], id)
			}
		}
	}

	var batches [][]string

	for len(inDegree) > 0 {
		// Collect all nodes with in-degree 0.
		var batch []string
		for id, deg := range inDegree {
			if deg == 0 {
				batch = append(batch, id)
			}
		}

		if len(batch) == 0 {
			// Cycle detected among pending nodes — shouldn't happen if Validate passed.
			break
		}

		batches = append(batches, batch)

		// Remove batch nodes and update in-degrees.
		for _, id := range batch {
			delete(inDegree, id)
			for _, next := range adj[id] {
				if _, ok := inDegree[next]; ok {
					inDegree[next]--
				}
			}
		}
	}

	return batches
}

// affectedSteps returns the set of step IDs that are transitively dependent on startID.
func affectedSteps(def *PlanDef, startID string) map[string]bool {
	affected := map[string]bool{startID: true}

	// Build reverse dependency map: step → steps that depend on it.
	dependents := make(map[string][]string)
	for _, s := range def.Steps {
		for _, dep := range s.DependsOn {
			dependents[dep] = append(dependents[dep], s.ID)
		}
	}

	// BFS/DFS to find all transitive dependents.
	queue := []string{startID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range dependents[current] {
			if !affected[next] {
				affected[next] = true
				queue = append(queue, next)
			}
		}
	}

	return affected
}
