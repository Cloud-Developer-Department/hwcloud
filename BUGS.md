# BUGS.md — Known Issues & Technical Debt

> Last updated 2026-08-10 (B20 prompt 超窗口上限 — 已修复).
> Format: `[P0]` = critical, `[P1]` = high, `[P2]` = medium, `[P3]` = low.

---

## [P0] 严重

### B1. ~~取消时已完成工具结果从存储丢失（H2）~~ ✅ 已修复（2026-08-04）

原问题：`kernel/run.go` 结果循环用**已取消的 ctx** 提交已完成工具结果——sqlite `BeginTx(ctx)` 对已取消 ctx 立即失败，存储里留下带 `tool_calls` 无结果的 assistant 消息；cancelCompensation 用内存消息建 covered 表（真实结果已在其中）不补写。下一轮历史出现中段孤儿 tool_call。

修复：`commit`（kernel/run.go:345）在 `ctx.Err() != nil` 时改用 `context.Background()` 提交（与 cancelCompensation 同模式）——运行中已产生的消息（模型输出、真实工具结果）在取消时仍落库，cancelCompensation 的 covered 判断恢复正确。

### B2. 审批持久化丢失更新（lost update）

`governance/persistent_memory.go:72-98`：`Remember` = Get → 改内存 → `Save` 整行（sqlite `INSERT OR REPLACE` 全行替换）。`m.mu` 只串行化同实例调用，但 ACP 层 7 处 `Runtime.Save`（标题/cwd 更新）与它无共享锁——交错序列可把 approvals 字段静默抹掉；跨进程共享同一 sqlite 文件时锁完全失效（last-writer-wins）。"Allow-Always 跨重启存活"契约静默失效。

修复：approvals 落独立表/行，或所有 session meta 写操作经单一写者/事务串行化。

### B3. ~~OpenViking 技能能力实际不可用~~ ✅ 已修复（2026-08-04）

- ~~`provider/openviking/client.go:169-177` `Search` 只构造 `{Kind, ID, Content, Score}`，`Item.Meta` **恒为 nil**；`skill.go:39-44` 读 `it.Meta["path"]/["name"]` 恒为空 → `Match` 返回的每个 SkillInfo `Name="skill"`（全部同名）、`Path=""`~~。修复：`Match` 不再依赖 `Meta`，直接从 `it.ID`（URI）提取 skill 名和根 URI。
- ~~`client.go:110-123` `doJSON` 从不校验 envelope `Status`~~。修复：`doJSON` 在 `env.Error` 检查后新增 `env.Status != "" && env.Status != "ok"` 校验。
- ~~`skill.go:69-74` `Load` 在 `s.loader==nil` 时返回 `skill.Description`（一句话摘要），非完整 SKILL.md body~~。修复：`Load` 改用 `client.Read(skill.Path+"/SKILL.md")` 拉取完整内容，Read 失败时回退到 description。
- ~~`skill.go:52` `Discover` 调用 `client.Search(ctx, "", 100, "skill")` 传空 query~~。修复（2026-08-04 早些时候）：`Discover` 改用 `GET /api/v1/skills` 列举接口。

修复仅涉及 `provider/openviking/skill.go` + `provider/openviking/client.go`，与本地 FS provider（`provider/skill/fs.go`）零交集，两种接入场景（有/无 OpenViking）均可用。

### B4. 模型上下文窗口表误判

`utils/context_window.go:34`：`gpt-4.1` 子串命中 `has(lower,"gpt-4")` → 128K，官方实际 **1M**（在 1/8 容量处就触发压缩）；`:40-41`：`deepseek v2.5` → 8K，实际 **128K**（16 倍提前压缩）。且 `TokenizerModelID` 对 gpt-4.1 走 cl100k → CJK 超计 ~60%。

修复：`"gpt-4"` 匹配之前加 `"gpt-4.1" → Window1M`；`v2.5 → Window128K`；`TokenizerModelID` 把 gpt-4.1 归入 o200k。**并给窗口表加映射测试**（此表已多次出错：GLM-5、本次两处）。

### B5. toSDKContentParts 未知类型留 `null` 元素炸掉请求

`model/openai/openai.go:257-261`：注释称"drop it explicitly"，但 `out[i]` 是**索引赋值**而非 append——未知 part 类型的零值元素保留原位，序列化为 `content: [..., null, ...]` → OpenAI 兼容 API 400，run 硬失败。注释（"序列化为空 {} 被 API 静默丢弃"）与实现双重错误。

修复：默认分支 `continue`（append 已知类型）。

### B20. ~~压缩触发后 prompt 仍超上下文窗口~~ ✅ 已修复（2026-08-10）

现象：多轮对话报 `prompt exceeds model context window: 370053 > 131072`，第二次 `730119 > 131072`（≈2×，跨 session 全量读回翻倍）。

两个独立根因，任一可单独触发超窗：

- **根因 A（主）**：`Compressor.Compact` 失败时（summarizer 模型 429/超时/网络），`prepareMemory` 原设计 fail-loud——`overflow = len(msgs)` 全量返回所有 post-summary 消息。summarizer 持续失败时每次都全量返回，跨 session 新 run 读回旧历史 + 自身新增 ≈ 2× 增长，精确解释 370053→730119 翻倍。修复：Compact 失败时降级裁剪到 budget（保留最新消息，丢最旧的；消息在 store 里不删，未来 summarizer 恢复仍可压缩）。
- **根因 B（次）**：`prepareMemory` 只在 turn 0 调用（`kernel/run.go:100`）。turn>0 的 `workingMessages` 纯 append 无压缩无裁剪，多轮小工具结果累积无界增长。修复：turn>0 且有 SessionStore 时也调 `prepareMemory`（从 store fetch + 压缩 + 裁剪）。对齐（from/ThroughIndex/globalCutoff）由 prepareMemory 内部从 store 读保证，不索引 workingMessages（回避 prefix 不 commit / TrimOrphanToolCalls 削头 / input 重复三条对齐破坏路径）。

**不是根因（已排除）**：

- summary 无限膨胀：`prompt.go:74-77` 将 summary 截到 `MaxCompressedTokens`（默认 8192）后才进 prompt，有界。8192 解释不了 370053 量级。
- tokenizer CJK 误差：cl100k 对 CJK 高估 ~60%，高估方向是提前触发压缩/提前报超窗（缓解），不加剧。60% 量级解释不了 370K 的数字。
- `estimatePromptOverhead` 漏算 skills 目录（见下"残留"）。

**残留（未修，有界）**：

`estimatePromptOverhead`（`kernel/prompt.go:190-217`）只扣 static + DynamicContext + summary，不扣 skills 目录 + recalled memories + resources + 动态 preamble 模板。227 个真实 skill 的 frontmatter ≈ 18K tokens 不从 budget 扣，使 working set budget 偏大 ~18K，降低超窗阈值。不产生 370053 量级（skills 有界），但让"离超窗有多近"少 18K 裕度。未修原因：`estimatePromptOverhead` 在 `prepareMemory` 内部调（`prepare.go:88`），早于 `context.Build`（`run.go:121`，产生 ac.Skills），此时 skills 尚未发现。需处理时序问题，作为独立后续优化。

---

## [P1] 高

### B6. MCP 连接无超时阻塞 ACP serve 循环

`acp/server.go:825` `connectMCP` 在 serve 循环内同步执行，ctx 是 `context.Background()`（acp/sdk/server.go:567）且 stdio 握手无超时。坏 MCP 子进程不响应时 `mcp.Client.Connect` 无限阻塞——session/cancel、session/close、session/list 全部停摆，用户无法取消运行中的 prompt。mux.request 的 5 分钟上限不覆盖此路径。

修复：`context.WithTimeout(ctx, 15s)`（超时 kill 子进程，mcp/client.go:96-99 已有该逻辑）+ 独立 goroutine 执行。

### B7. 流式工具取消报"部分内容成功"

`execution/runtime.go:196-203`：`toolCtx.Done()` 时 flush 后把部分流内容包装成**无 Error 的成功 ToolResult**，提交进会话历史，下一轮模型还读得到；阻塞路径在取消时返回错误结果，两路径语义不一致。且 `ErrorResult(chunk.Error, false, "")` 硬编码 retryable=false，瞬态流错误永不重试。

修复：取消/错误时给结果打错误或 cancelled 标记（保留 chunk.Error 的 Retryable 语义）。

### B8. truncateToolArg 按字节截断 UTF-8

`acp/server.go:2498` `s[:n-3]` 可能切断多字节字符 → 工具标题（shell description/goal/query 含中文/emoji 时）含非法 UTF-8，客户端显示乱码。与已修先例 a736ac7（会话标题截断）同类，同文件 `firstLine` 已用 `[]rune` 修复，此处漏修。

修复：同 firstLine 改为 rune 截断。

### B9. ~~summarizer 无重试 + 无 nil 防御~~ ✅ 已修复（2026-08-07）

- ~~主模型调用有重试，但 Compact→Summarize 单发：429/503 瞬时限流 → 溢出区间既不在摘要也不在工作集 → **当轮上下文空洞**（`kernel/run.go` 仅 `slog.Error` 后继续，prepareMemory 仍推进工作集起点）。~~
- ~~`summarizer/llm.go:70-74` 未判 `resp == nil`（extractor_llm.go:137 有检查，两处不对称）；接口未禁止第三方 Model 返回 (nil,nil)，违规即 panic。~~

修复：`summarizer/llm.go` 的 `Summarize` 对 `RetryableError` 重试一次（默认 1s 指数退避，尊重 `RetryAfter`，cancel 安全），镜像 `kernel/modelcall.go` 的重试契约；补 `resp == nil` 防御（返回明确错误，不重试、不 panic）。新增 `summarizer/llm_test.go` 覆盖重试成功/耗尽/nil 响应/非重试错误/cancel 路径。

### B10. SSE 连接永久悬挂（team 端点）

`rest/team.go:352-359`：`for se := range sub.C` 仅靠 done/error 或写失败退出；eventbus Publish 满则**静默丢弃**（bus.go:266-273），慢客户端写满 256 槽后终态 "done"/"error" 被丢 → SSE 循环永远阻塞（无 `r.Context().Done()` select、无心跳），HTTP 连接与 goroutine 泄漏到进程退出。对照 rest/handler.go:470-477 有同样洞。

修复：SSE 循环同时 select `r.Context().Done()`；或 run 结束关闭 done chan 作为终止信号。

### B11. extractor 防护缺失

- `context/extractor_llm.go:141-158`：`op != "skip"` 即 Store——`{"op":"update","topic":""}` 或幻觉 topic 时 sqlite 端按空 topic 直接 INSERT → 重复条目膨胀（Mem0 式去重要防的正是这个），完全静默。要求 update 的 topic 非空且在 existing 中命中，否则降级 skip/add。
- `context/async_extractor.go:71-89`：worker 无 recover——一次 panic 后 queue 里的 key 无人处理，此后所有会话的知识提取**静默永久失效**到进程重启。
- `extractor_llm.go:105`：Recall 错误被吞 → DB 故障时模型把已有知识全标 add。

### B12. checkpoint 派生自已取消的 ctx

`acp/server.go`（OnPrompt 收尾）：`cpCtx, cancel := context.WithTimeout(ctx, 5*time.Second)`——客户端 $/cancel_request 恰好触发后 cpCtx 立即 done，被取消的 turn 的 checkpoint 必然失败（仅 warn），恢复点永远丢失。同函数 saveTotalTokens 同样问题（静默失败）。

修复：`context.WithTimeout(context.Background(), 5*time.Second)`。

### B13. OpenViking 客户端资源与安全

- `client.go:105-108` `io.ReadAll` 无大小上限：异常/被攻陷服务端返回 GB 级响应直接耗尽进程内存。修：`io.LimitReader`（如 32MB）。
- ~~客户端零认证通道（`OpenVikingConfig` 只有 Endpoint，无 header 注入点）：远程部署时任何能触达端点地址的进程可读可写全部记忆。修：可选 Bearer token / 自定义 header。~~ ✅ 已修复（2026-08-05）：`OpenVikingConfig` 新增 `APIKey` 字段，`doJSON` 注入 `Authorization: Bearer <key>` 头。
- `Remember`（client.go:199-209）中途失败残留孤儿会话（创建后 commit 失败不清理）。

### B14. 工具截断产物权限过宽

`result.go:147-156`：工具输出（可能含密钥）写入 `/tmp/hwcloud` **0755 目录、0644 文件**，本机其他用户可读。修：0700/0600。

### B15. 重试不区分幂等性 + hooks 每尝试重复触发

`execution/runtime.go:96-113`：任何 `Retryable=true` 结果以完全相同参数重跑（最多 2 次，500ms/1s 退避无抖动）——对已产生部分副作用的工具（写盘后超时）重复执行副作用；每次尝试独立走完整管线，`OnToolStart/End` hooks 每尝试触发一遍。当前无内置工具设 Retryable，属潜在危险基础设施。

修复：文档明确 Retryable 仅允许幂等工具；hooks 只在首次尝试触发；退避加抖动。

---

## [P2] 中

### B16. slash Handle 无 panic 兜底 + 参数重组

`slash/slash.go:140-167`：handler 是任意闭包（接 server 方法），panic 直接冒泡到 ACP 请求 goroutine（唯一分发点，acp/server.go 调用处无 recover）。且 `Fields`+`Join` 与文档"raw argument string"矛盾：多行/引号/连续空白被改写（`/note "a  b"` → `"a b"`）。修：Handle 内 defer recover 转错误响应；传原始字符串。

### B17. GET 读端点复活已删除会话

`rest/team.go:232/268/412/436/500` 全部走 `getOrCreate`（rest/session.go:398-446，缺失时 newEntry + syncMeta 持久化）——GET /messages 对已删除 id 返回 200 空数组并**在 store 里创建新记录**，前端删除后轮询会复活会话；对照 rest/handler.go 的 messages() 不创建，行为不一致。修：读取路径先 `Exists(id)`，仅 chat 保留 getOrCreate。

### B18. 团队会话 approvalMemory 未初始化

`rest/team.go:533-585` newEntry 不创建 approvalMemory（对照 Handler.newEntry 创建了 PersistentApprovalMemory）——团队模式下 "always allow" 决策只活在内存，重启即忘，与单 agent 模式行为不一致。

### B19. 窗口表映射测试缺失

手写模型表（utils/context_window.go）已多次出错（GLM-5、gpt-4.1、deepseek v2.5），无测试锚定。修：每个已知模型断言预期窗口的映射测试。

---

## [P3] 低

- `acp/server.go:2299-2305`（同 B8 先例之外）其余字节截断点：`truncate`（provider/openviking/client.go:226-231）、`extractor_llm.go:213` recallQuery `c[:200]`、`:132` 日志 `raw[:300]`、`extractor_llm.go:149` `len(content) < 12` 按字节。
- `truncateTokens`（kernel/prompt.go:143-156）O(n²)（每加一个 rune 全量 BPE）且硬编码 "gpt-4" cl100k（CJK 超计 ~60%）。
- `checkHandoff`（kernel/run.go:310-319）对被拒绝/失败的工具也生效——transfer_to_ 被拒仍以 "handoff" 结束本轮，模型无法重试。
- 子代理 approver 构造时捕获而非运行时解析（kernel/as_tool.go:72）——SetHumanApprover 中途切换后旧子代理工具持旧 approver。
- 硬窗口检查（kernel/run.go:143-150）未预留输出 token——prompt 恰好等于窗口时 provider 拒绝。
- modelcall.go:127-132 稀疏 tool-call index：`len(map)` 是条目数而非最大 index，不连续 index 的调用被静默丢弃。
- `baseURL` 字符串拼接遇尾斜杠双斜杠（provider/openviking/client.go:87-90）。
