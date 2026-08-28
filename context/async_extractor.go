package context

import (
	"context"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// AsyncExtractor runs extraction in the background with a single worker:
// submissions are coalesced per user — the latest messages replace any
// queued job for the same user — so N finished runs produce at most one
// extraction pass. Industry practice: write-heavy LLM extraction runs
// asynchronously (Mem0: extraction at write time, typically background)
// and must never delay the agent loop.
//
// The worker is bounded: one goroutine, one job at a time, 60s timeout
// per extraction, queue capacity 64 (a saturated backlog drops new
// submissions — extraction is best-effort). Callers build one shared
// instance per server (the kernel never constructs per-run workers).
type AsyncExtractor struct {
	inner Extractor

	mu      sync.Mutex
	pending map[string]extractJob // key: scope.UserID — coalesced to latest
	queue   chan string           // user keys ready for extraction
	done    chan struct{}         // worker stop (tests)
}

type extractJob struct {
	scope    ContextScope
	messages []hwcloud.Message
}

// NewAsyncExtractor creates an extractor that submits to a background
// worker. A nil inner extractor yields a no-op.
func NewAsyncExtractor(inner Extractor) *AsyncExtractor {
	e := &AsyncExtractor{
		inner:   inner,
		pending: make(map[string]extractJob),
		queue:   make(chan string, 64),
		done:    make(chan struct{}),
	}
	go e.worker()
	return e
}

// SetModel updates the model on the inner LLMExtractor if present.
// Safe to call concurrently with Extract; the next background pass
// uses the new model.
func (e *AsyncExtractor) SetModel(m hwcloud.Model) {
	if e == nil {
		return
	}
	if llm, ok := e.inner.(*LLMExtractor); ok {
		llm.SetModel(m)
	}
}

// Extract implements Extractor: enqueue (non-blocking, ~µs). The actual
// extraction runs on the background worker.
func (e *AsyncExtractor) Extract(ctx context.Context, scope ContextScope, messages []hwcloud.Message) {
	if e == nil || e.inner == nil || len(messages) == 0 {
		return
	}
	key := scope.UserID
	e.mu.Lock()
	_, queued := e.pending[key]
	e.pending[key] = extractJob{scope: scope, messages: messages}
	if !queued {
		// First submission for this user: announce the key. A saturated
		// backlog drops the announcement — best-effort by design.
		select {
		case e.queue <- key:
		default:
		}
	}
	e.mu.Unlock()
}

// worker drains the queue and extracts the latest job per user.
func (e *AsyncExtractor) worker() {
	for {
		select {
		case key := <-e.queue:
			e.mu.Lock()
			job, ok := e.pending[key]
			delete(e.pending, key)
			e.mu.Unlock()
			if !ok {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			e.inner.Extract(ctx, job.scope, job.messages)
			cancel()
		case <-e.done:
			return
		}
	}
}

// stop halts the worker (tests only; a process exits with pending jobs
// unextracted — best-effort).
func (e *AsyncExtractor) stop() {
	close(e.done)
}
