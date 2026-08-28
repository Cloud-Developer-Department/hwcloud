# hwcloud 架构

> [English](DESIGN.md) | [README](README.md) | [README (中文)](README.zh.md)

## 概述

hwcloud 是一个 **Agent Runtime Kernel** —— Go 语言实现的事件驱动、上下文驱动的可扩展执行系统。核心是一条极简的主线循环，所有能力通过可插拔模块叠加。

**设计原则（Context Architecture v2.1）：**

- **Agent 是配置。** `agent.Agent` 结构体是纯描述（模型、提示词、守卫、子 agent、限额）。它不持有工具、记忆、审批器、hooks 或 observer —— 那些是注入 `kernel.Runtime` 的运行时依赖（`kernel.Deps`）。
- **Context 是 agent 的输入。** agent 从不直接触碰存储。Context Runtime 组装 agent 所见（工作消息 + 召回的知识）；Runtime State（执行、审批）是独立的簿记。
- **Memory 不是 Context —— memory 是 Context 的来源。** 旧的单一 `Memory` 接口按生命周期拆成三个 Provider：
  - `session.SessionStore` —— 当前会话（短期）
  - `session.Compressor` —— token 预算压缩 / 摘要
  - `context.MemoryProvider` —— 持久知识（偏好、事实、经验）
- **审批是分层策略引擎**，不是布尔门（规则 → 安全 → 记忆 → 人工）。
- **工具结果是结构化的**（`*ToolResult`），输出超过上下文预算时由运行时自动截断落盘。
- **运行时自我进化**：完成的对话扫描出持久知识，存储并在未来会话自动召回。
- **Event 是审计，不是状态。** 不做 Event Sourcing；仅日志/可观测性。
- 没配置模块 = 没有那个能力；nil 则跳过对应节点。
- 库代码不读环境变量（那是应用层的职责）。

**两种扩展方式：**

| 方式 | 适用场景 | 机制 |
|------|---------|------|
| **编译时扩展** | 平台开发者 | 实现 Go 接口 → 注入 `kernel.Deps` |
| **运行时扩展** | 社区/终端用户 | `.wasm` 插件放入目录 → 自动加载 |

两者并存，不互斥。编译时接口是"主干"，运行时插件是接口的一种实现来源。

---

## 包结构

```
hwcloud/
├── agent/        Agent 配置（纯）：Agent、options、Team、Router
├── kernel/       Runtime 引擎：8 节点循环、Runtime + Deps、按节点拆方法
├── context/      Context Runtime：AgentContext、ContextScope、MemoryProvider
│                 接口、LLMExtractor + AsyncExtractor（自我进化）
├── execution/    Execution Runtime：工具调用、内置工具、任务（job）、重试
├── governance/   策略引擎（分层审批）、ApprovalMemory、
│                 ToolClassifier（平台侧只读分类）
├── session/      SessionStore + Compressor 接口
├── session/sqlite, session/file
│                 会话后端（对话 + 压缩标记）
├── provider/     Provider 接口与后端：memory/(sqlite|file)、
│                 skill/、resource/、openviking/（远程上下文数据库）
├── tool/         内置工具实现（shell、file、grep、web、acp_*）
├── mcp/          MCP 客户端/服务端适配器
├── acp/          Agent Client Protocol 集成
├── rest/         REST + SSE API
├── orchestrate/  多 agent DAG 规划 + 执行
└── 根包           核心类型：Message、Tool、Model、Session、StreamEvent、
                  ToolResult、RunHooks、Guard、Approver、token 辅助
```

依赖方向（无环）：`根包 ← session ← session/sqlite,file`；`根包 ← governance ← guard/llm,rest,acp`；`根包 ← agent`；`根包+session ← context`；`根包 ← provider/* ← execution`；`根包+agent+context+execution+governance+session+provider ← kernel`；`… ← rest/acp/cmd`（应用层）。根包是核心类型层：只依赖 `tokenizer`。

---

## 主线 8 节点（kernel）

```
cfg := agent.New("name", agent.WithModel(m), ...)
deps := kernel.Deps{Tools: ..., SessionStore: ..., Policy: ...}
rt := kernel.New(cfg, deps)
rt.Run(ctx, session, input) | rt.RunStream(...) | rt.RunGoal(...)
```

```
① SessionStore 拉取（压缩 + 工作集，仅 turn 1）
② Context Build（知识召回）→ Prompt 构建（静态 + 动态 + 知识）
③ Guard.in
④ 模型调用（优先流式，RetryableError 重试）
⑤ Guard.out
⑥ Policy.Evaluate（规则 → 安全 → 记忆 → 人工）逐工具调用
⑦ Execution Runtime：Start 任务 → 按调用顺序 Wait（并行执行、顺序收集）
⑧ SessionStore Append（Commit）
```

每个节点是 `kernel.Runtime` 上的方法（run.go、prompt.go、modelcall.go、cancel.go、prepare.go、execute.go、compress.go、subagent.go），可独立单测与扩展。**取消是持久化完备的**：已完成结果用 background context 提交（已取消的 ctx 会让存储事务立即失败，留下孤儿 tool_call）；流式工具被中断时持久化显式 `cancelled` 错误结果（绝不报半截"成功"）；未决的 tool_calls 在 run 中止前写入 "cancelled by user" 补偿。

**两层 prompt 模型：**

| 层 | 来源 | 内容 |
|----|------|------|
| Static | `Agent.SystemPrompts` + `ProjectContext` | 组装时设定，不变 |
| Dynamic | `Session.DynamicContext` | 每 turn 构建：plan entries + mode 指令 |

---

## 核心类型（根包）

### Agent（纯配置）

```go
type Agent struct {
    Name, Description string
    SystemPrompts     []string
    Model             Model
    Prompt            PromptBuilder    // nil = default
    InGuard           InputGuard
    OutGuard          OutputGuard
    SubAgents         []SubAgent       // 配置声明的委托工具
    MaxTurns          int              // default 20
    MaxWorkingTokens  int              // default 0 = 上下文窗口的 70%
    MaxCompressedTokens int            // default 8192
    ReasoningEffort   string
}
```

Agent 不持有 Tools/Memory/Approver/Hooks/Observer —— 全部通过 `kernel.Deps` 注入运行时。运行时构建时把 `SubAgents` 注册为委托工具（模型只传 task，子 agent 用自己的系统提示、工具白名单，治理继承策略链）。

### StreamEvent

```go
const (
    StreamThought      = "thought"        // 推理内容
    StreamTextDelta    = "text_delta"     // 逐字符输出
    StreamToolCall     = "tool_call"      // 工具调用开始
    StreamToolProgress = "tool_progress"  // 流式工具输出 chunk
    StreamToolResult   = "tool_result"    // 工具结果（最终）
    StreamRetrying     = "retrying"       // 瞬时错误重试中
    StreamDone         = "done"           // 正常完成
    StreamError        = "error"          // 执行失败
    StreamAborted      = "aborted"        // 外部中断（cancel/timeout）
)
```

### Session

```go
type Session struct {
    ID, UserID, ModelID     string
    Temperature, MaxTokens  float64 / int
    UserProfile, ProjectContext string
    DynamicContext              string     // 每轮 plan + mode 上下文
    Turn                        int
    CreatedAt                   time.Time
    Metadata                    map[string]any
}
```

纯数据载体，应用层管理 CRUD。Runtime 不创建 Session。

---

## 模块接口

### ① Memory（三个 Provider）

```
Layer 1: Working    — SessionStore.Recent() / RecentAfter()；kernel 按预算管理
Layer 2: Compressed — Compressor.Compressed() 自动注入；Compact() 增量压缩
Layer 3: Archive    — MemoryProvider.Recall() + Store()；消息永不删除
```

```go
type SessionStore interface {              // 当前会话（短期）
    Append(ctx, sessionID, msg) error
    Recent(ctx, sessionID, n, offset) ([]Message, error)
    RecentAfter(ctx, sessionID, throughIndex, n) ([]Message, error) // 摘要后的增量
    Count(ctx, sessionID) (int, error)
    DeleteSession(ctx, sessionID) error
}

type Compressor interface {                // token 预算压缩（中期）
    Compact(ctx, sessionID, throughIndex, messages) error
    Compressed(ctx, sessionID) (*CompressedContext, error)
}

type MemoryProvider interface {            // 持久知识（长期）
    Recall(ctx, scope, query, limit) ([]MemoryEntry, error)
    Store(ctx, scope, item) error
}
```

会话与知识**物理分域**：`session/sqlite` + `provider/memory/sqlite` 各自独立连接打开同一 .db（WAL 安全），知识表独立，零 schema 迁移。`RecentAfter` 只读摘要覆盖之后的增量——消息永不删除，所以摘要的 ThroughIndex 标记了要跳过的部分。`SafeCompressionBoundary` 保证 tool_call/tool_result 配对完整。

### Summarizer（Compressor 的依赖）

```go
type Summarizer interface {
    Summarize(ctx context.Context, messages []Message, previous *CompressedContext) (*CompressedContext, error)
}
```

nil = 不压缩（Compact 静默 no-op，工作集不裁剪——fail-loud 交给硬窗口检查）。实现：`summarizer/llm.go` —— LLM 增量压缩。

### Embedder（知识后端的依赖）

```go
type Embedder interface {
    Embed(ctx context.Context, text string) ([]float64, error)
    Dimensions() int
}
```

nil = 降级为 FTS5 搜索。embedding 全走外部 provider（OpenAI 兼容 /embeddings API，在 settings 配置）；默认构建纯 Go（CGO_ENABLED=0），无内嵌模型。

### ② Context Runtime（组装 agent 的输入）

Context Runtime 组装 AgentContext（工作消息 + 知识召回 + 技能匹配 + 资源），PromptBuilder 只负责把 AgentContext 格式化为 messages：

```go
type Runtime interface {
    Build(ctx, BuildRequest) (*AgentContext, error)
}
```

### ③ / ⑤ Guard

```go
type InputGuard interface {
    Check(ctx, input GuardInput) GuardResult
}
type OutputGuard interface {
    Check(ctx, output GuardOutput) GuardResult
}

type GuardResult struct {
    Allowed  bool
    Reason   string
    Tripwire bool  // true → 终止 run
}
```

InGuard 在循环前检查一次。OutGuard 检查每次 model 输出 + 每个 tool result。被拒内容**绝不落库**（guard 在 Commit 之前）。实现：`guard/llm`。

### ④ Model

```go
type Model interface {
    ChatCompletion(ctx, req) (*ChatCompletionResponse, error)
    ChatCompletionStream(ctx, req) (StreamReader, error)  // nil,nil = 不支持
    ContextWindow() int
}
```

实现：`model/openai`（openai-go v3 SDK）。`ReasoningEffort` 透传到模型的 `reasoning_effort` 参数。

### Tool（结构化结果）

```go
type Tool interface {
    Definition() FunctionDefinition
    Execute(ctx context.Context, args json.RawMessage) *ToolResult   // 单返回，错误进 Result.Error
}
```

内置工具：`shell`、`read`、`write`、`ls`、`grep`、`websearch`、`webfetch`。自动注入：`load_skill`、`reload_skills`、`recall`。子 agent 委托工具由配置声明。ACP RPC 工具：`read_client_file`、`write_client_file`、`terminal_*`。Plan 工具：`plan_create`、`plan_update`、`enter_plan_mode`、`exit_plan_mode`。

### Sandbox

```go
type Sandbox interface {
    Run(ctx context.Context, cmd Command) (Result, error)
    CWD() string    // 工具视角的工作目录（可能与宿主路径不同）
}
```

`CWD()` 返回沙箱内部视角的路径 —— bwrap 下为 `/workspace`，否则为宿主路径。实现：`sandbox/native`（Linux bwrap、macOS Seatbelt）。

### ⑥ 审批策略引擎（governance）

```
Policy.Evaluate(call) →
  1. Rules      设置驱动：工具+参数模式 → allow/deny/ask
  2. Safety     运行时分类（只读自动放行，平台侧 ToolClassifier）
  3. Memory     会话级审批记忆（Allow-Always 持久化）
  4. Human      Ask → Allow / Deny / Always / ModifiedArgs
```

`Decision{Action, Reason, ModifiedArgs}` 取代布尔审批器。默认引擎（无 `Deps.Policy`）自动放行 `transfer_to_*` 交接，人工层委托给配置的 `Approver`（nil = 全放行）。只读分类在平台侧（`governance.ToolClassifier`），工具不自报——旧的 `SelfApproving.CanSelfApprove` 自声明模式已删除。

### ⑦ RunHooks

Start 方法返回不透明 `any` 值，Runtime 传递给对应的 End 方法。实现：`hooks/slog`、`hooks/otel`、`hooks/redact`。

### RunObserver

每个阶段 enter/leave 事件，含耗时与 detail 元数据。**观察者必须线程安全**（`tool.execute` 事件来自并行工具 goroutine）。detail 只放元数据（计数/状态/长度），不放内容全文。多个 observer 通过 `MultiObserver()` 组合。

---

## 结构化工具结果

```go
type ToolResult struct {
    Content   string          // 展示文本（超限时替换为指针）
    JSON      json.RawMessage // 可选结构化数据
    Metadata  map[string]any  // 退出码、耗时、mime ...
    Truncated bool
    FileRef   string          // 落盘 artifact 的路径
    Error     *ToolError      // {Message, Retryable, Code}
}
```

运行时在 hooks 之后、落库之前应用 `ResultPolicy`：输出超过模型上下文窗口 5% 时保存到 `<ArtifactRoot()>/sess-<id>/`，替换为短指针（模型按需 read/grep 该文件）。**Artifact 可读性保障**：超过 32K rune 的单行在 rune 边界拆行（`\n` 与 `\r` 都是行终止符），每处人工断点标记 `[line wrapped; continues below]`——`read`/`grep` 工具单行上限 1MB，不拆行的 minified 单行 blob 会完全不可读。Retryable 错误触发退避重试（仅在幂等工具上声明 `Retryable`）。`Message.Result` 携带结构化结果；`RunHooks.OnToolEnd` 收到 `*ToolResult`，hooks 可修改（脱敏等）。

**token 计数**：`tokenizer/`（tiktoken，模型感知）对超过 8KB 的文本改用头部抽样线性外推（BPE 密度稳定，误差 ~1-3%）——纯 Go 的 tiktoken-go 全量编码约 72µs/字节，4MB 工具结果的截断判断全量编码需 ~5 分钟，抽样后 ~0.6s。精确计数对预算/阈值决策不敏感，可接受。

---

## 自我进化（知识闭环）

```
完成的 run
  → context.LLMExtractor（Mem0 式 ADD/UPDATE/SKIP 分类）
  → AsyncExtractor（后台 worker，按 user 合并去重）
  → MemoryProvider.Store(scope, item)
  → 下一会话：ContextRuntime.Build 召回（query = goal/input）
  → prompt "## Recalled Knowledge" 段落（kind 标记）
```

Scope（`ContextScope{UserID, ProjectID, SessionID, Partition}`）保证知识归属正确的用户/项目。Extractor 是接口——换别的分类器不动存储/召回。

---

## 事件模型

事件是审计/可观测性辅助，不是状态。`RunHooks`（start/end 对携带不透明状态）覆盖 agent/工具生命周期；`RunObserver` 覆盖循环各阶段；REST 层经 `eventbus` 桥接 SSE。流事件（`text_delta`、`tool_call`、`tool_result`...）非阻塞发送——背压下有界丢失是设计使然。

---

## ACP v1 协议

Agent 原生支持 ACP 协议。`acp.NewAgentServer(cfg, deps, store, models)` 把配置 + 依赖包装为 ACP 兼容 handler。**会话级 `kernel.Runtime` 每个会话构建一次、跨 turn 复用**——每轮变更都是增量的（config/model/mode 在会话锁内热切换，plan 工具每 prompt 重绑）。模式切换通过 `applyModeTools` 换工具集与审批器（modeMu + 运行时锁，serve 循环、prompt goroutine、工具回调三流并发安全）。Plan 模式使用 `plan_create`/`plan_update` 工具。

**协议层次：**

| 层 | 包 | 角色 |
|----|----|------|
| 类型 | `acp/sdk/` | ACP v1 schema，零依赖 |
| 传输 | `acp/sdk/` | JSON-RPC 2.0 over stdio — mux、client session、Agent→Client RPC |
| 集成 | `acp/server.go` | AgentServer — session CRUD、prompt turns、plan mode、MCP、slash 命令 |

---

## Plan 模式

使用 `plan_create` 和 `plan_update` 工具 —— LLM 通过 function-calling 参数直接输出结构化 plan 条目：

```
用户目标 → OnPrompt
  → agent 调用 plan_create(goal, steps[{id, content, priority}])
  → plan 文本进入对话上下文
  → agent 调用 plan_update(updates[{id, status}]) 更新进度
  → plan entries 持久化到 SessionStore._meta["plan"]
  → 每轮：DynamicContext 注入当前 plan 状态到 system prompt
```

模式状态机（auto/manual/plan）在 modeMu 下维护；`enter_plan_mode`/`exit_plan_mode` 工具让 agent 自主切换。

---

## Slash 命令

服务端 slash 命令在传递给 agent 之前拦截：

```
/help      — 列出可用命令
/mode      — 切换会话模式 (auto/manual/plan)
/model     — 列出或切换模型
/context   — 显示 token 使用（分 layer：摘要/工作集/窗口）
/cwd       — 显示工作目录
/clear     — 重置会话消息
/rename    — 重命名会话标题
/sessions  — 列出所有会话
/compact   — 手动全量压缩
```

命令通过 `slash/` Registry 注册，从 `OnPrompt` 分发。未知 `/` 命令传递给 agent 处理。

---

## 关键设计决策

**1. 为什么 Agent 是纯配置？** 能力（工具/存储/策略）是运行时依赖，与"agent 是谁"正交。配置与执行分离让同一个配置能跑在 ACP/REST/CLI 不同宿主下，也让运行时可以被单测驱动。

**2. 为什么 Runtime 按 8 节点拆方法？** 每个节点可独立单测；run() 只编排。取代了旧的 1700 行 runner.go 巨型函数。

**3. 为什么 Memory 一拆三？** 会话（短期）、压缩（中期）、知识（长期）生命周期不同、访问模式不同。一个接口扛三种职责导致方法死亡（Search 被知识 Recall 取代）。

**4. 为什么审批是策略引擎而不是布尔门？** 布尔 `Approve() (bool, string)` 无法表达"编辑参数后放行"、"always allow"、"只读自动放行"的组合。分层链让每层各司其职，Decision 携带 ModifiedArgs。

**5. 为什么工具结果是单返回 `*ToolResult`？** 错误进 `Result.Error` 单通道——调用方不必处理 (string, error) 双返回的中间态；截断/落盘是运行时内建行为而非 hooks hack。

**6. 为什么超长结果落盘而不是截断字符串？** 截断丢失信息；落盘保留全文，模型按需 read/grep。指针文案 + 行包装标记让 artifact 可读、可区分人工断点。

**7. 为什么 token 计数对超长文本抽样？** 纯 Go tiktoken 全量编码 ~72µs/字节（4MB 需 ~5 分钟）。BPE 密度在单一文本内稳定，8KB 样本外推误差 ~1-3%——预算/阈值决策完全不敏感。若未来需要精确计数（计费对齐），换 cgo + tiktoken-rs 内核即可。

**8. 为什么 Runtime 跨 turn 复用而不是每轮重建？** 技能缓存、沙箱状态、工具集值得跨 turn 存活；每轮变化（模型/模式/plan 工具）都是增量热切换。代价是并发安全要求——会话状态在 modeMu 下、运行时可变字段在 rt.mu 下。

**9. 为什么取消要持久化？** 取消的 turn 已产生的输出（模型回答、工具结果）是真实进展；用已取消 ctx 提交会让存储事务立即失败，留下孤儿 tool_call 在历史里，下一轮模型读到破坏配对的格式。

**10. 为什么 Session 上有 DynamicContext？** Plan entries 和 mode 每 turn 变化。Runtime 不应知道 ACP 或 plan —— Session 是中性传输通道。

---

## 运行时扩展（WASM 插件）

**插件类型：**

| 类型 | 用途 | ABI 导出 |
|------|------|---------|
| `agent:tools` | 向 agent 添加新工具 | `hwcloud_agent_tools()` / `hwcloud_execute()` |
| `agent:observers` | 观测 pipeline 阶段（stage enter/leave） | `hwcloud_on_stage(event_json)` |
| `cli:settings` | 注入凭证、修改 settings JSON | `hwcloud_cli_init()` |
| `cli:commands` | 添加自定义 cobra 子命令 | `hwcloud_cli_commands()` / `hwcloud_cli_run()` |
| `cli:observers` | 生命周期事件记录 | `hwcloud_cli_on_startup()` / `..._on_shutdown()` |

WASM 运行时：[wazero](https://github.com/tetratelabs/wazero) —— 纯 Go，零 CGO。宿主函数（`log_info`、`keyring_get`、`http_request`、`utc_now`）由 `plugin/wasmhost/` 提供。

---

## Team（多 Agent 编排）

```go
team := hwcloud.NewTeam(
    hwcloud.WithTeamAgent("researcher", "分析问题", researcher),
    hwcloud.WithTeamAgent("calculator", "执行计算", calculator),
)
```

Handoff = Tool with `EndTurn: true`。每个 agent 有独立的 Memory、Tools、Guard。

### Orchestrate（LLM 驱动 DAG 执行）

```go
p := orchestrate.NewPlan(
    orchestrate.WithPlanner(orchestrate.NewLLMPlanner(model)),
    orchestrate.WithAgent("coder", "写代码", coderAgent),
    orchestrate.WithAgent("reviewer", "审查代码", reviewerAgent),
)
```

| | Team | Orchestrate |
|---|------|------------|
| 决策 | 运行时，agent 自主发起 | 执行前，LLM 生成 DAG |
| 并行 | 无（串行交接链） | 拓扑批量自动并行 |
| 失败 | agent 自行处理 | LLM 子树 replan |

---

## 沙箱

OS 原生安全：macOS Seatbelt、Linux Bubblewrap。

```go
sb, _ := native.New("./workspace")
cwd := sb.CWD()  // bwrap 下为 "/workspace"，否则为宿主路径
```

---

## 对比

| | openai-agents | Claude Code | hwcloud |
|---|---|---|---|
| 协议 | — | — | ACP v1 (JSON-RPC 2.0) |
| 沙箱 | Docker SDK + macOS sandbox-exec | seccomp + namespaces | macOS Seatbelt / Linux bwrap |
| 文件工具 | read/write/ls | Read/Write/Glob | ReadFile/WriteFile/ListDir/Grep |
| 流式 | PTY-based | Bash tool | Shell tool (line streaming) |
| 多 agent | Handoff chain | — | Team + Orchestrate |
| Plan 模式 | — | 工具驱动 | plan_create/plan_update 工具 |
| 可观测性 | — | — | RunObserver + StageEvent（含 detail 元数据） |
| 插件 | — | — | WASM (wazero, 零 CGO) |
| Slash 命令 | — | — | 注册表 + 内置 + 可扩展 |
| Memory 压缩 | — | — | LLM 增量 summarizer |
