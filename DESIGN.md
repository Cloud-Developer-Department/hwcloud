# hwcloud Architecture

> [中文](DESIGN.zh.md) | [README](README.md) | [README (中文)](README.zh.md)

## Overview

hwcloud is an **Agent Runtime Kernel** — an event-driven, context-driven,
extensible execution system in Go. The core is a minimal mainline loop; all
capabilities are added through pluggable modules.

**Design principles (Context Architecture v2.1):**

- **Agent is configuration.** The `agent.Agent` struct is a pure description
  (model, prompts, guards, skills, limits). It owns no tools, memory, approver,
  hooks, or observer — those are runtime dependencies injected into
  `kernel.Runtime` via `kernel.Deps`.
- **Context is the agent's input.** The agent never touches storage. The
  Context Runtime assembles what the agent sees (working messages + recalled
  knowledge); Runtime State (executions, approvals) is separate bookkeeping.
- **Memory is not context — memory is a source of context.** The legacy
  monolithic `Memory` interface is split into three providers with different
  lifecycles:
  - `session.SessionStore` — the current conversation (short-term)
  - `session.Compressor` — token-budget compression / summary
  - `context.MemoryProvider` — durable knowledge (preferences, facts, lessons)
- **Approval is a layered policy engine**, not a boolean gate
  (rules → safety → memory → human).
- **Tool results are structured** (`*ToolResult`), with runtime-built
  truncation to disk when output exceeds the context budget.
- **The runtime is self-evolutionary**: finished runs are scanned for durable
  knowledge, stored, and auto-recalled into future sessions.
- **Event is audit, not state.** No event sourcing; logging/observability only.
- No module configured = capability absent; nil means skip that node.
- Library code never reads environment variables (application layer's job).

## Package Layout

```
hwcloud/
├── agent/        Agent configuration (pure): Agent, options, Team, Router
├── kernel/       Runtime engine: 8-node loop, Runtime + Deps, per-node methods
├── context/      Context Runtime: AgentContext, ContextScope, MemoryProvider
│                 interface, LLMExtractor + AsyncExtractor (self-evolution)
├── execution/    Execution Runtime: tool calls, built-in tools, jobs, retry
├── governance/   Policy engine (layered approval), ApprovalMemory,
│                 ToolClassifier (platform-side read-only classification)
├── session/      SessionStore + Compressor interfaces
├── session/sqlite, session/file
│                 Session backends (conversation + compression markers)
├── provider/     Provider interfaces + backends: memory/(sqlite|file),
│                 skill/, resource/, openviking/ (remote context DB)
├── tool/         Built-in Tool implementations (shell, file, grep, web, acp_*)
├── mcp/          MCP client/server adapters
├── acp/          Agent Client Protocol integration
├── rest/         REST + SSE API
├── orchestrate/  Multi-agent DAG planning + execution
└── root package  Core types: Message, Tool, Model, Session, StreamEvent,
                  ToolResult, RunHooks, Guard, Approver, tokens helpers
```

Dependency direction (acyclic): `root ← session ← session/sqlite,file`;
`root ← governance ← guard/llm,rest,acp`; `root ← agent`;
`root+session ← context`; `root ← provider/* ← execution`;
`root+agent+context+execution+governance+session+provider ← kernel`;
`… ← rest/acp/cmd` (application layer). The root package is the core type
layer: it imports only `tokenizer`.

## The 8-Node Mainline Loop (kernel)

```
cfg := agent.New("name", agent.WithModel(m), ...)
deps := kernel.Deps{Tools: ..., SessionStore: ..., Policy: ...}
rt := kernel.New(cfg, deps)
rt.Run(ctx, session, input) | rt.RunStream(...) | rt.RunGoal(...)
```

```
① SessionStore fetch (compaction + working set, turn 1)
② Context Build (knowledge recall) → Prompt build (static + dynamic + knowledge)
③ Guard.in
④ Model call (streaming preferred, retry on RetryableError)
⑤ Guard.out
⑥ Policy.Evaluate (rules → safety → memory → human) per tool call
⑦ Execution Runtime: Start jobs → Wait in call order (parallel, ordered)
⑧ SessionStore Append (Commit)
```

Each node is a method on `kernel.Runtime` (run.go, prompt.go, modelcall.go,
cancel.go, prepare.go, execute.go, compress.go, subagent.go) so stages can
be unit-tested and extended independently. Cancellation is
persistence-complete: completed results are committed with a background
context (a cancelled ctx would fail the store transaction and orphan the
tool_call), a streaming tool interrupted mid-flight persists an explicit
`cancelled` error result (never a truncated "success"), and unresolved
tool_calls get a "cancelled by user" compensation before the run aborts.

## Layered Approval (governance)

```
Policy.Evaluate(call) →
  1. Rules      settings-driven: tool+args pattern → allow/deny/ask
  2. Safety     runtime classification (read-only auto-allow)
  3. Memory     session-scoped approval memory (Allow-Always persists)
  4. Human      Ask → Allow / Deny / Always / ModifiedArgs
```

`Decision{Action, Reason, ModifiedArgs}` replaces the boolean approver. The
default engine (no `Deps.Policy`) auto-allows `transfer_to_*` handoffs and
delegates the human layer to the configured `Approver` (nil = allow all).
Read-only classification lives on the platform side (`governance.ToolClassifier`),
not on tools — the legacy `SelfApproving.CanSelfApprove` self-declaration
pattern was removed; the runtime keeps final say.

## Structured Tool Results

```go
type ToolResult struct {
    Content   string          // display text (truncated when oversized)
    JSON      json.RawMessage // optional structured data
    Metadata  map[string]any  // exit code, duration, mime, ...
    Truncated bool
    FileRef   string          // pointer to saved artifact
    Error     *ToolError      // {Message, Retryable, Code}
}
```

The runtime applies a `ResultPolicy` after hooks and before memory: output
exceeding 5% of the model's context window is saved to
`<ArtifactRoot()>/sess-<id>/` and replaced with a short pointer (the model
reads or greps the file on demand). Artifacts are made read-friendly:
single lines longer than 32K runes are broken at rune boundaries (both
`\n` and `\r` count as line terminators), each artificial break marked
`[line wrapped; continues below]` so the model can tell a wrapped
continuation from a real newline — the `read`/`grep` tools cap a single
line at 1MB, and an unwrapped minified blob would be unreadable.
Retryable errors trigger automatic retries with backoff (declare
`Retryable` only on idempotent tools). `Message.Result` carries the
structured outcome; `RunHooks.OnToolEnd` receives `*ToolResult` so hooks
can mutate it (redaction, etc.).

## Memory (Three Providers)

| Provider | Lifecycle | Methods |
|---|---|---|
| `session.SessionStore` | current conversation | Append / Recent / RecentAfter / Count / DeleteSession |
| `session.Compressor` | summary layer | Compact / Compressed (ThroughIndex contract) |
| `context.MemoryProvider` | durable knowledge | Recall(scope, query) / Store(scope, item) |

Session and knowledge live in separate packages over the same physical
storage (`session/sqlite` + `provider/memory/sqlite`, WAL-safe, no schema
migration). `RecentAfter` reads only the post-summary increment (messages
are never deleted, so the summary's ThroughIndex marks what to skip).
`SafeCompressionBoundary` keeps tool_call/tool_result pairs intact.

## Self-Evolution (Knowledge Loop)

```
finished run
  → context.LLMExtractor (Mem0-style ADD/UPDATE/SKIP classification)
  → AsyncExtractor (background worker, dedupes per user)
  → MemoryProvider.Store(scope, item)
  → next session: ContextRuntime.Build recalls (query = goal/input)
  → prompt "## Recalled Knowledge" section (kind-tagged)
```

Scope (`ContextScope{UserID, ProjectID, SessionID, Partition}`) keeps
knowledge owned by the right user/project. The extractor is an interface —
swap in another classifier without touching store/recall.

## Event Model

Events are Audit/Observability aids, not state. `RunHooks`
(start/end pairs with opaque state) cover agent/tool lifecycles;
`RunObserver` covers loop stages; the REST layer bridges to SSE via
`eventbus`. Stream events (`text_delta`, `tool_call`, `tool_result`, ...)
are emitted non-blocking — bounded loss under backpressure is by design.

## ACP v1 Protocol

The agent speaks the Agent Client Protocol natively. `acp.NewAgentServer(cfg,
deps, store, models)` wraps a config + deps as an ACP-compliant handler. The
session-scoped `kernel.Runtime` is built once per session and reused across
turns — per-turn changes are incremental (config/model/mode live-swaps under
the session lock, plan tools rebound per prompt). Mode transitions swap the
tool set and approver via `applyModeTools` (modeMu + runtime locks, safe
from the serve loop, the prompt goroutine, and tool callbacks alike).
Plan mode uses `plan_create`/`plan_update` tools.

## Slash Commands

Server-side slash commands intercepted before they reach the agent:
`/help /mode /model /context /cwd /clear /rename /sessions` — registered via
`slash/` Registry, dispatched from OnPrompt.

## Two Extension Paths

| Path | For | Mechanism |
|---|---|---|
| Compile-time | Platform developers | Implement Go interface → inject via `WithXxx()` / `kernel.Deps` |
| Runtime | Community / end users | Drop `.wasm` files into plugin dir → auto-loaded |

WASM plugins expose tools/observers/sessions via the `plugin/agent/wasm`
host (wazero), bridging to `kernel.Runtime` through `wasm.BuildAgentRuntime`.
