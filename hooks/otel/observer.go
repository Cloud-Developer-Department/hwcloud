package otel

import (
	"context"
	"fmt"
	"strings"
	"sync"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Observer implements hwcloud.RunObserver via OpenTelemetry spans. Each
// stage of the 8-node loop (memory.fetch, guard.in, prompt.build, model.call,
// guard.out, tool.execute, memory.append) becomes a child span under the
// "agent.run" span created by Hooks. This gives the trace full per-turn
// granularity — you can see how long each stage took, not just the aggregate.
//
// Enter/leave pairing: ObserveStage is called with Phase="enter" at stage
// start and Phase="leave" at stage end. The enter call starts a span; the
// leave call ends it and records duration + error. The span is stashed in a
// concurrent-safe map keyed by (runID, turnID, stageName) so the leave call
// can find it — ObserveStage does not return a context, so we cannot thread
// the span through ctx.
//
// Thread safety: ObserveStage may be called from concurrent goroutines
// (tool.execute runs on job goroutines). The spanStore is mutex-protected;
// each enter/leave pair is keyed uniquely.
type Observer struct {
	tracer trace.Tracer
}

// NewObserver creates an Observer that creates stage spans with the given
// tracer. The tracer should be the same one passed to New (Hooks) so stage
// spans attach to the agent.run span.
func NewObserver(tracer trace.Tracer) *Observer {
	return &Observer{tracer: tracer}
}

// ObserveStage implements hwcloud.RunObserver. On "enter" it starts a span;
// on "leave" it ends the span with duration and error.
func (o *Observer) ObserveStage(ctx context.Context, event hwcloud.StageEvent) {
	if o.tracer == nil {
		return
	}

	switch event.Phase {
	case "enter":
		// Start a span named after the stage (e.g. "model.call").
		// Attributes carry run/turn join keys so the trace can be
		// cross-referenced with slog observer logs.
		attrs := []attribute.KeyValue{
			attribute.String("stage.name", event.Name),
		}
		if event.RunID != "" {
			attrs = append(attrs, attribute.String("agent.run_id", event.RunID))
		}
		if event.ParentRunID != "" {
			attrs = append(attrs, attribute.String("agent.parent_run_id", event.ParentRunID))
		}
		if event.TurnID >= 0 {
			attrs = append(attrs, attribute.Int("agent.turn_id", event.TurnID))
		}
		// Merge stage-specific detail attributes.
		attrs = append(attrs, detailAttrs(event.Detail)...)
		_, span := o.tracer.Start(ctx, event.Name,
			trace.WithAttributes(attrs...),
		)
		stageSpans.store(eventKey(event), span)

	case "leave":
		span := stageSpans.load(eventKey(event))
		if span == nil {
			return
		}
		stageSpans.delete(eventKey(event))
		defer span.End()

		// Record duration as an attribute (span duration is automatic,
		// but this makes it queryable in backends that don't show
		// span duration as a first-class field).
		if event.Duration > 0 {
			span.SetAttributes(attribute.Int64("stage.duration_ms", event.Duration.Milliseconds()))
		}
		// Record leave-phase detail (e.g. tokens, tool count).
		span.SetAttributes(detailAttrs(event.Detail)...)
		if event.Err != nil {
			span.SetStatus(codes.Error, event.Err.Error())
			span.RecordError(event.Err)
		}
	}
}

// detailAttrs converts a StageEvent.Detail map into OTel attributes,
// handling string, int, int64, float64, and bool values. Other types are
// skipped (they can be added if needed). The key is prefixed with "stage."
// to namespace stage-specific metadata.
func detailAttrs(detail map[string]any) []attribute.KeyValue {
	if len(detail) == 0 {
		return nil
	}
	attrs := make([]attribute.KeyValue, 0, len(detail))
	for k, v := range detail {
		key := fmt.Sprintf("stage.%s", k)
		switch val := v.(type) {
		case string:
			attrs = append(attrs, attribute.String(key, val))
		case int:
			attrs = append(attrs, attribute.Int(key, val))
		case int64:
			attrs = append(attrs, attribute.Int64(key, val))
		case float64:
			attrs = append(attrs, attribute.Float64(key, val))
		case bool:
			attrs = append(attrs, attribute.Bool(key, val))
		}
	}
	return attrs
}

// eventKey produces a unique key for an enter/leave pair: runID + turnID +
// stageName. This handles concurrent stages (the same stage name can appear
// on different turns or job goroutines simultaneously).
func eventKey(event hwcloud.StageEvent) string {
	return fmt.Sprintf("%s:%d:%s", event.RunID, event.TurnID, event.Name)
}

// stageSpans is a process-wide map from eventKey to the span started on
// "enter", so "leave" can find and end it. ObserveStage is called from
// multiple goroutines (tool.execute runs on job goroutines), so this must
// be concurrent-safe.
var stageSpans = &spanStore{}

type spanStore struct {
	mu    sync.Mutex
	spans map[string]trace.Span
}

func (s *spanStore) store(key string, span trace.Span) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spans == nil {
		s.spans = make(map[string]trace.Span)
	}
	s.spans[key] = span
}

func (s *spanStore) load(key string) trace.Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spans[key]
}

func (s *spanStore) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.spans, key)
}

// loadDecisionSpan finds any active span for the given runID+turnID, ignoring
// the stage name. Decisions happen inside a stage but we don't know which one
// from the DecisionEvent alone — this scans for any match. Returns nil if no
// span is active for that run+turn.
func (s *spanStore) loadDecisionSpan(runID string, turnID int) trace.Span {
	prefix := fmt.Sprintf("%s:%d:", runID, turnID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, span := range s.spans {
		if strings.HasPrefix(key, prefix) {
			return span
		}
	}
	return nil
}

// ObserveDecision implements hwcloud.DecisionObserver. It records each
// governance/policy decision as a span event on the currently active stage
// span (looked up by runID+turnID). If no stage span is active, the event is
// dropped — decisions always happen inside a stage.
func (o *Observer) ObserveDecision(ctx context.Context, event hwcloud.DecisionEvent) {
	if o.tracer == nil {
		return
	}
	// Find the active stage span for this run+turn. Decisions happen inside
	// a stage (guard.in, tool.execute, ...), so there should be one. We try
	// the most likely stages in order.
	span := stageSpans.loadDecisionSpan(event.RunID, event.TurnID)
	if span == nil {
		// No active span — fall back to the current context span.
		span = trace.SpanFromContext(ctx)
		if !span.IsRecording() {
			return
		}
	}

	attrs := []attribute.KeyValue{
		attribute.String("decision.layer", event.Layer),
		attribute.String("decision.outcome", event.Outcome),
	}
	if event.Subject != "" {
		attrs = append(attrs, attribute.String("decision.subject", event.Subject))
	}
	if event.CallID != "" {
		attrs = append(attrs, attribute.String("decision.call_id", event.CallID))
	}
	if event.SessionID != "" {
		attrs = append(attrs, attribute.String("decision.session_id", event.SessionID))
	}
	attrs = append(attrs, detailAttrs(event.Detail)...)

	span.AddEvent(event.Layer, trace.WithAttributes(attrs...))
}

var _ hwcloud.RunObserver = (*Observer)(nil)
var _ hwcloud.DecisionObserver = (*Observer)(nil)
