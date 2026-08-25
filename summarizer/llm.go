// Package summarizer provides LLM-based conversation compression
// implementing hwcloud.Summarizer.
//
// The Compressor uses the agent's Model to generate incremental,
// rolling summaries of older messages. Each summary subsumes the
// previous one so the context window stays compact without losing
// long-term history.
package summarizer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// Compressor implements hwcloud.Summarizer by calling the configured
// Model to produce incremental summaries.
type Compressor struct {
	mu        sync.RWMutex
	model     hwcloud.Model
	maxTokens int // 0 = no hint; non-zero = prompt the model to keep the summary under this
	// backoff returns the wait before retry attempt N (1-based). nil uses
	// the default exponential backoff. Overridable in tests to avoid sleeps.
	backoff func(attempt int, re *hwcloud.RetryableError) time.Duration
}

// New creates a Compressor backed by m.
func New(m hwcloud.Model) *Compressor {
	return &Compressor{model: m}
}

// SetModel updates the model used for summarization. Safe to call
// concurrently with Summarize; the next call uses the new model.
func (c *Compressor) SetModel(m hwcloud.Model) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.model = m
}

// WithMaxTokens sets a SOFT target for the summary size: the budget is
// passed to the model as a prompt hint, NOT as ChatCompletionRequest
// MaxTokens. A hard output cap truncates the JSON envelope mid-stream,
// which parseSummary then rejects — the real length limit is enforced
// when the summary enters the prompt (kernel's MaxCompressedTokens
// truncation). Default is 0 (no hint).
func (c *Compressor) WithMaxTokens(n int) *Compressor {
	c.maxTokens = n
	return c
}

// Summarize implements hwcloud.Summarizer.
//
// When previous is nil this is the first compression pass — a fresh
// summary is generated. Otherwise the new messages are folded into the
// existing summary, producing an updated CompressedContext whose
// ThroughIndex is left at zero (the caller sets it).
func (c *Compressor) Summarize(ctx context.Context, messages []hwcloud.Message, previous *hwcloud.CompressedContext) (*hwcloud.CompressedContext, error) {
	c.mu.RLock()
	model := c.model
	c.mu.RUnlock()
	if model == nil {
		return nil, fmt.Errorf("summarizer: no model configured")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("summarizer: no messages to summarize")
	}

	prompt := c.buildSummarizePrompt(messages, previous)
	// No MaxTokens on purpose: a hard output cap truncates the JSON
	// envelope mid-stream and parseSummary rejects it. Length control is
	// the prompt hint below plus the prompt-side truncation in kernel.

	// Retry transient failures (429/5xx incl. 504 Gateway Timeout) with
	// backoff. The per-call timeout below is the primary defense against a
	// hung gateway: without it a single 504 blocks the run for up to the
	// model client's HTTP timeout (5m), and with retries that compounds to
	// ~10m of dead time before the error surfaces. Retries stay modest
	// (2, not 5) because compaction runs on the prepare-memory critical
	// path every turn — a persistently failing gateway is better fast-
	// failed (degrade to tail-trim in prepare.go) than retried into a
	// multi-minute stall.
	//
	// Backoff sequence (seconds): 2, 4 — 2 retries, ~6s total.
	// A provider-supplied RetryAfter (e.g. 429 Retry-After header) overrides
	// the computed value for that attempt.
	const (
		maxRetries    = 2
		callTimeout   = 90 * time.Second
	)
	var lastErr error
	var resp *hwcloud.ChatCompletionResponse
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(c.backoffFor(attempt, lastErr)):
			case <-ctx.Done():
				return nil, fmt.Errorf("summarizer: model call: %w", ctx.Err())
			}
		}
		// Per-call timeout: a 504 Gateway Timeout can take minutes to
		// surface (the gateway waits on its own upstream first). Cap each
		// attempt so a hung gateway fails fast instead of monopolizing the
		// run. The derived ctx is scoped to this single model call; the
		// parent ctx (cancel/deadline) is still honored via Done().
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		var err error
		resp, err = model.ChatCompletion(callCtx, hwcloud.ChatCompletionRequest{
			Messages: []hwcloud.Message{
				{Role: hwcloud.RoleSystem, Content: summarizeSystemPrompt},
				{Role: hwcloud.RoleUser, Content: prompt},
			},
		})
		cancel()
		if err == nil {
			if resp == nil {
				// Defend against a third-party Model returning (nil, nil)
				// — a contract violation that would panic below. Not
				// retryable: retrying won't fix a broken implementation.
				return nil, fmt.Errorf("summarizer: model returned nil response")
			}
			break
		}
		// A per-call timeout surfaces as context.DeadlineExceeded, which is
		// NOT a RetryableError — it fails fast instead of retrying into
		// another 90s stall. This is intentional: a gateway slow enough to
		// trip 90s is unlikely to recover on the immediate next attempt.
		var re *hwcloud.RetryableError
		if !errors.As(err, &re) {
			return nil, fmt.Errorf("summarizer: model call: %w", err)
		}
		lastErr = err
	}
	if resp == nil {
		// Retries exhausted on a transient error.
		return nil, fmt.Errorf("summarizer: model call: %w", lastErr)
	}

	content := ""
	if len(resp.Choices) > 0 {
		content = resp.Choices[0].Message.Content
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("summarizer: model returned empty summary")
	}
	return &hwcloud.CompressedContext{Summary: content}, nil
}

// backoffFor returns the wait before retry attempt N (1-based), honoring
// a RetryAfter hint when the provider supplies one.
func (c *Compressor) backoffFor(attempt int, lastErr error) time.Duration {
	var re *hwcloud.RetryableError
	errors.As(lastErr, &re)
	if c.backoff != nil {
		return c.backoff(attempt, re)
	}
	// 2, 4 seconds for attempts 1, 2 — matches kernel/modelcall.go's
	// backoff base (2<<uint(attempt-1)).
	b := time.Duration(2<<uint(attempt-1)) * time.Second
	if re != nil && re.RetryAfter > 0 {
		b = re.RetryAfter
	}
	return b
}

// ── Prompt ──

const summarizeSystemPrompt = `You are a conversation summarizer. Your job is to produce a concise,
structured summary of a conversation so an AI assistant can resume the
thread without re-reading every message.

The summary is the assistant's OWN memory of the conversation (it is
injected back to the same assistant). Refer to the user as "the user";
narrate your own actions with no third-person subject ("searched memory,
found...", NOT "the assistant searched memory") — the reader is yourself.

Structure the summary text with exactly these eight sections, in order
(the Claude Code compaction format; user messages are NOT listed
verbatim — recent messages stay full-fidelity in the working set, older
ones are summarized by intent within the sections below):
1. Primary Request and Intent — what the user originally wanted and the deeper goal
2. Key Technical Concepts — frameworks, patterns, algorithms, architectures
3. Files and Code Sections — every relevant file by path and why it matters
4. Errors and Fixes — errors, how they were resolved, user reactions
5. Problem Solving — reasoning chains, alternatives, debugging strategies
6. Pending Tasks — unfinished or deferred work
7. Current Work — precise description of work in progress at conversation end
8. Optional Next Step — aligned with the user's most recent explicit requests

Be concise. The summary is injected into a system prompt.

Output your summary as plain text only: the eight numbered sections, no
intro label, no JSON, no markdown code fences — the text is injected
directly into a system prompt and JSON escaping would corrupt it.`

func (c *Compressor) buildSummarizePrompt(messages []hwcloud.Message, prev *hwcloud.CompressedContext) string {
	var b strings.Builder
	if c.maxTokens > 0 {
		fmt.Fprintf(&b, "Target length: keep the summary under %d tokens.\n\n", c.maxTokens)
	}
	if prev != nil && prev.Summary != "" {
		b.WriteString("## Existing Summary\n")
		b.WriteString(prev.Summary)
		b.WriteString("\n\n## New Messages (incorporate into the summary)\n\n")
	} else {
		b.WriteString("## Messages to Summarize\n\n")
	}
	for _, m := range messages {
		switch m.Role {
		case hwcloud.RoleUser:
			b.WriteString("User: ")
		case hwcloud.RoleAssistant:
			b.WriteString("Assistant: ")
		case hwcloud.RoleTool:
			if m.ToolCallID != "" {
				fmt.Fprintf(&b, "Tool result (%s): ", m.ToolCallID)
			} else {
				b.WriteString("System: ")
			}
		case hwcloud.RoleSystem:
			b.WriteString("System: ")
		}
		b.WriteString(truncateContent(m.Content, 300))
		b.WriteString("\n")
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "  [called tool %s]\n", tc.Function.Name)
			}
		}
	}
	return b.String()
}

func truncateContent(s string, n int) string {
	s = strings.TrimSpace(s)
	// Truncate by rune (byte-slicing cuts multi-byte UTF-8 in half).
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return "..."
	}
	return string(runes[:n-3]) + "..."
}

