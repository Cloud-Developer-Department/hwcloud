# iac-server — Cloud IaC MCP Server

一个通过 MCP 协议暴露云基础设施部署能力的 server。任何 MCP client(Claude Code、opencode、Cursor、hwcloud)配置后,即可在对话中完成"部署一个系统到云上"的全流程:需求分析、资源规划、费用估算、部署、排错、销毁。

## 设计约束

1. **消费方不固定。** 任何 MCP client 都能用,server 不能依赖消费方有 workflow / event / approval / checkpoint 能力,也不能假设 client 的工具调用超时。长任务用**异步 job 模式**(工具立即返回 job_id,client 轮询),与 client 的超时策略无关。
2. **server 不是 dumb wrapper。** 资源推荐、规格选型、费用优化、错误排错是域专家推理,需要 LLM。server 自带一个 LLM,与消费方的 LLM 各司其职。
3. **云不固定。** server 依赖 `CloudProvider` 接口(见 `provider/doc.md`),华为云是一个实现。加一个云 = 实现接口 + 提供技能树 + 提供角色提示词,不改 server 核心。
4. **`iac/` 是纯执行单元。** 只管 terraform 二进制 + 命令执行 + 结构化结果。不管模板、不管费用、不管 LLM、不管 workflow(见 `iac/DESIGN.zh.md`)。
5. **状态在磁盘,不在 server 内存。** DAG 契约在 `dag.json`、报价门槛在 `cost.json`、部署进度在 terraform state——server 进程无状态,死了不丢。

## 分层

```
任意 MCP Client (Claude / opencode / Cursor / hwcloud)
    │  只看到上层语义 tool,不感知 terraform、不感知具体云
    ▼
cmd/mcp/iac-server              ← 本 server
    ├── mcp/                    MCP stdio server + tool 注册
    ├── agent/                  server 侧 LLM 推理(planner + job 执行)
    ├── provider/               CloudProvider 接口 + huaweicloud 实现
    │                           (技能树、角色提示词、http_request、terraform 就绪)
    └── 依赖 iac/               terraform 二进制管理 + 命令执行
         ▼
    terraform binary
```

## 两个 LLM

server 自带一个 LLM,用于域专家推理。消费方也有自己的 LLM。两者职责不同:

| | client LLM (Claude / opencode / ...) | server LLM (iac-server 内部) |
|---|---|---|
| **知道什么** | 用户在说什么、该调哪个 tool、结果怎么讲给用户 | 云资源目录、规格可用性、定价、AZ 约束、terraform 错误模式 |
| **不知道什么** | 云 flavor 名、AZ 约束、定价表 | 用户完整对话上下文、用户其他意图 |
| **输入** | 自然语言对话 | 结构化需求/错误 + 云 API 查询能力 |
| **输出** | 调 tool、跟用户对话 | 架构 DAG、规格方案、.tf、诊断修复建议 |

原则:**域知识下沉到 server LLM**。client LLM 传"高可用、预算 500、需要 MySQL",不传"ECS s7.large.2 × 2"。具体规格由 server LLM 通过真实 API 查询后推理决定——规格和价格永远来自云 API,不硬编码。

## 部署流程(DAG 驱动)

**DAG 是步骤间的结构化契约**,落盘在 deployment 目录的 `dag.json`,每一步读、丰富、写回——不依赖对话历史,上下文压缩丢不了。

```
propose_architecture     需求 → 架构 DAG(节点=资源,depends_on=依赖边) → dag.json
    │  信息不足 → need_input + questions → client 带答案重调
    ▼
specify_resources        load_skill(服务技能) → http_request 查真实规格
    │  (ListFlavors/ListImages...) → 每节点填 spec → dag.json(status=specified)
    │  选择过多 → need_input + questions → answers 参数重调
    ▼
generate_terraform_plan  读 DAG → LLM 写 .tf → terraform init(缓存命中)+ plan
    │  重试 3 次(plan 失败让 LLM 修 .tf;init 失败直接报错——环境问题)
    ▼
estimate_cost            读 DAG → bss 技能 → 查价(按需/包月,产品级回退)
    │  → cost.json(apply 门槛标记)+ dag.json(status=cost_estimated)
    ▼
apply_deployment         ⚠️ 门槛:cost.json 必须存在(未报价拒绝)
    │  terraform apply
    ▼
troubleshoot_deployment  任一步失败:对比正确 .tf 模式 + 搜索 → 诊断建议
```

旁路:`update_deployment`(重跑 specify+generate,**失效 cost.json** 需重新报价)、`query_cloud`(只读查询现有资源/账单/规格)、`get_deployment_status`/`list_deployments`/`destroy_deployment`(纯执行)。

## 异步 job 模式

MCP 无原生异步工具调用,client 工具超时各异——而我们的 agent 常跑 2-3 分钟。**job 模式**解决:

```
tools/call propose_architecture
  → 立即返回 {"job_id": "...", "status": "started", "hint": "轮询 get_job_result"}

get_job_result(job_id, wait_seconds=0-60)   ← long-poll
  → {"status": "running"|"done"|"failed", "progress_msg", "outputs", "result"}
```

- **outputs**:server LLM 每轮文本输出流(经 runtime 的 stage observer,只读观察通道,无自定义 hooks)——client 看到模型在写什么。
- **progress_msg**:planner 阶段消息(查规格中 / 生成 .tf 中...)。
- job 持久化到 `<cloudHome>/jobs/`(重启可查),**同 deployment 新提交取代旧 job**(client 重试永远作用于最新请求),全局并发上限 4,panic 恢复,15 分钟超时,24h TTL 清理。

## 云抽象

server 依赖 `CloudProvider` 接口(5 个方法),不绑定具体云:

```go
type CloudProvider interface {
    Name() string                       // 云标识,如 "huaweicloud"
    Env() map[string]string             // terraform 子进程的凭据环境变量
    Skills() fs.FS                      // 嵌入的技能目录(go:embed,启动解压)
    Agents() map[PromptRole]AgentConfig // 6 个 agent 角色的云特定提示词
    ProviderSource() string             // terraform provider source(预热缓存用)
}
```

- **角色提示词与输出契约分工**:provider 给云特定专家知识(API 名、流程、约束);JSON 输出契约(被 planner 程序化解析)留在 server 核心。
- **技能树 = 云知识库**:`<cloud>-deploy`/`-bss`/`-troubleshoot` 静态注入,206 个服务技能(`huaweicloud-ecs` 等)由 query_cloud/specify_resources 动态 `load_skill`——每个技能带 `references/`(该服务全部 API 的精简 swagger 定义,约 15,700 个),server LLM 用 read/grep/ls 按需查阅。
- **规格/价格永远来自真实 API**:specify 查 ListFlavors/ListImages,estimate 查 BSS 定价——经 http_request 工具(自动 SDK-HMAC-SHA256 签名,凭据不进 LLM 上下文;SSRF 防御;大响应落盘 + 分页指引)。
- 详见 `provider/doc.md`。

## terraform 就绪

- **默认 provider 镜像**:`mirrors.huaweicloud.com/terraform/`(华为云社区镜像),用户 `TF_PROVIDER_MIRRORS` 追加按序 fallback——国内网络零配置可 init。
- **provider 缓存**:`TF_PLUGIN_CACHE_DIR` → `<iacHome>/plugins`,一次下载所有 deployment 复用。
- **lock 预热**:启动时后台生成 `terraform.lock.hcl`(锁 provider 版本)→ deployment init 复制 lock → 命中缓存(秒级,无版本漂移)。预热不阻塞 MCP initialize,失败降级 warning。

## 配置

```
CLOUD              云: huaweicloud(默认)/ aliyun
IAC_API_KEY         server 侧 LLM 的 API key
IAC_BASE_URL        server 侧 LLM 的 base URL(OpenAI-compatible)
IAC_MODEL           server 侧 LLM 的模型 ID
IAC_HOME            server home(默认 ~/.hwcloud/mcp/iac-server)
IAC_DRY_RUN         "true" = 模拟,不调 terraform
TF_BINARY_MIRRORS    terraform 二进制镜像(逗号分隔)
TF_PROVIDER_MIRRORS  provider 镜像(追加在默认镜像后)
HW_ACCESS_KEY / HW_SECRET_KEY / HW_REGION   云凭据(terraform 子进程环境)
```

两组凭证:云凭证部署用(注入子进程),LLM 凭证推理用(server 进程内)。

## Tool 列表(12 个)

| tool | LLM | 异步 | 性质 | 说明 |
|---|---|---|---|---|
| `propose_architecture` | 是 | ✅ | 只读 | 需求 → 架构 DAG;不足 → need_input |
| `specify_resources` | 是 | ✅ | 只读 | 查真实规格 → DAG 丰富化;answers 重调 |
| `generate_terraform_plan` | 是 | ✅ | 只读 | DAG → .tf → init + plan |
| `estimate_cost` | 是 | ✅ | 只读 | 报价 → cost.json(apply 门槛) |
| `update_deployment` | 是 | ✅ | 只读 | 重跑 specify+generate,失效报价 |
| `troubleshoot_deployment` | 是 | ✅ | 只读 | 错误 → 诊断建议 |
| `query_cloud` | 是 | ✅ | 只读 | 查询现有资源/账单/规格 |
| `get_job_result` | 否 | — | 只读 | 轮询异步 job 状态(outputs/result) |
| `apply_deployment` | 否 | — | **有副作用** | ⚠️ 门槛:必须先 estimate_cost |
| `destroy_deployment` | 否 | — | **有副作用** | terraform destroy |
| `get_deployment_status` | 否 | — | 只读 | 读 terraform.tfstate(不调二进制) |
| `list_deployments` | 否 | — | 只读 | 扫部署目录 |

- LLM 工具(前 7 个)返回 job_id,client 轮询 `get_job_result`;执行工具同步。
- 只读工具不需审批;有副作用工具(apply/destroy)靠消费方审批机制——那是消费方的事。
- 报价语义边界:`estimate_cost` 是"未来成本预估"(apply 前置门槛),`query_cloud` 是"已发生账单查询"——两个工具描述明确区分,防止 client 混用。

## 状态与恢复

**状态全在磁盘,server 无状态:**

```
<cloudHome>/
├── dag.json → 部署目录  (DAG 契约:proposed→specified→planned→cost_estimated→applied)
├── cost.json → 部署目录  (报价标记,apply 门槛)
├── terraform.lock.hcl    (provider 版本锁定)
├── plugins/              (provider 缓存)
├── jobs/                 (异步 job 状态)
├── memory.db             (会话历史,压缩摘要)
└── deployments/d-XXX/    (.tf + .terraform + terraform.tfstate)
```

进程死、client 关都不丢进度,重连能续。`get_deployment_status` 读 state 文件秒级返回。

## 目录结构

```
cmd/mcp/iac-server/
├── DESIGN.zh.md                本文件(设计思路)
├── main.go                      入口:读 env → 选 CloudProvider → 启动 MCP server
├── mcp/                         MCP stdio server + tool 注册
│   └── tools.go                 12 个工具 + 异步 job 提交
├── agent/                       server 侧 LLM 推理
│   ├── planner.go               6 个 agent(architect/specifier/planner/pricer/...)
│   ├── job.go                   JobManager(持久化/取代/并发上限/锁)
│   ├── dag.go                   DAG 契约(load/save/validate)
│   └── observer.go              jobObserver(模型输出 → job,经 stage observer)
└── provider/
    ├── doc.md                   云抽象设计思路
    ├── provider.go              CloudProvider 接口 + PromptRole 类型
    ├── huaweicloud/             CloudProvider 实现(技能树/http_request/提示词)
    └── aliyun/                  占位实现
```

## 与 iac/ 的关系

`iac/` 在这整套里的角色不变(见 `iac/DESIGN.zh.md`):

- server handler 调 `iac.NewClient` → `client.Init/Plan/Apply/Destroy`
- `iac/` 不知道 deployment_id、不管 .tf 谁生成、不管 LLM、不管费用、不管云是哪个
- **LLM 和 CloudProvider 在 server 层,不在 `iac/`**

## 不做的事

- **不做 workflow runtime。** 对话即编排,tool call 即 step,terraform state 即 checkpoint。workflow runtime 是消费方的事。
- **不做审批。** 审批靠消费方自己的机制。server 只标记 tool 有无副作用。
- **不绑定具体云。** 依赖 `CloudProvider` 接口。
- **不硬编码规格或定价。** 规格/价格永远来自真实云 API(http_request 查询),硬编码会给出错误引导——涉及钱和资源的事,给用户错误信息是危险的。
- **不让 LLM 参与执行决策。** apply/destroy 是纯执行,要不要执行是用户的事(server 只强制"先报价"门槛)。
- **不在 tool 执行过程中向用户提问。** 信息不完整时返回 `need_input` 状态 + 待补充问题,client 负责提示用户并带 answers 再次调用。
