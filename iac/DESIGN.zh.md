# iac — Terraform Wrapper

## 目标

将 Terraform 封装为一个干净的 Go 包，上层（MCP server、workflow runtime）不需要关心二进制安装、provider 镜像、命令执行细节。


## 依赖

| 库 | 用途 |
|---|---|
| `hashicorp/hc-install` | 检测/下载/安装 Terraform 二进制 |
| `hashicorp/terraform-exec` | 执行 init/plan/apply/destroy 等命令 |
| `hashicorp/terraform-json` | 解析 plan JSON 为结构化数据 |

## 目录结构

```
iac/
├── doc.md          ← 本文件
├── binary.go       — 二进制管理: Detect / Install / Ensure
├── client.go       — Client + Config + Options
├── commands.go     — Init / Validate / Format / Plan / Apply / Destroy / Output / ShowPlan
├── mirror.go       — 生成 .terraformrc provider 镜像配置
├── prewarm.go      — 预热:provider 下载进共享缓存 + 生成 lock 文件
└── types.go        — Plan / Summary / ResourceChange / ValidateResult / Output
```

## Config

```go
type Config struct {
    // ── 二进制 ──
    // 优先级: BinaryPath > Detect(PATH) > BinaryMirrors 逐个尝试 > hc-install 官方源
    BinaryPath    string   // 直接指定 terraform 路径，跳过安装
    Version       string   // 要求的版本，如 "1.9.5"；空则取最新
    BinaryMirrors []string // 二进制下载镜像 base URL，按顺序尝试

    // ── Provider 插件 ──
    // 生成 .terraformrc，设 TF_CLI_CONFIG_FILE 环境变量
    // URL → network_mirror，本地路径 → filesystem_mirror
    // Terraform 原生按顺序尝试，最后 fallback 到官方 registry
    ProviderMirrors []string // provider 镜像，URL 或本地路径

    // PluginCacheDir 设 TF_PLUGIN_CACHE_DIR — provider 首次下载进共享目录,
    // 所有 deployment 复用(配合 lock 文件,init 秒级命中,不再重复下载)
    PluginCacheDir string

    // ── 执行环境 ──
    Env    map[string]string // 云凭证等环境变量 (HW_ACCESS_KEY, HW_SECRET_KEY, ...)
    DryRun bool              // 不调二进制，返回模拟结果
}
```

### 镜像示例

```go
cfg := iac.Config{
    Version:       "1.9.5",
    BinaryMirrors: []string{
        "https://mirrors.tencent.com/terraform/",
        "https://mirrors.aliyun.com/terraform/",
    },
    ProviderMirrors: []string{
        "https://mirrors.tencent.com/terraform/",
        "https://mirrors.aliyun.com/terraform/",
        "/opt/tf-provider-mirror", // 本地缓存
    },
    Env: map[string]string{
        "HW_ACCESS_KEY": "xxx",
        "HW_SECRET_KEY": "xxx",
        "HW_REGION":     "cn-north-4",
    },
}
```

## API

### 二进制管理

```go
// Detect 在 PATH 中查找 terraform，返回路径和版本。
// 找不到返回 error，不执行安装。
func Detect() (path string, version string, err error)

// detectVersion 运行 `terraform version` 解析版本号。
func detectVersion(binaryPath string) (string, error)

// Install 从镜像或官方源下载指定版本到 destDir。
// 返回二进制完整路径。
func Install(ctx context.Context, version string, mirrors []string, destDir string) (path string, err error)

// EnsureTerraform 确保二进制可用，按优先级尝试:
//   1. BinaryPath 直接用
//   2. Detect() 找已有的（若指定 Version 则校验版本匹配）
//   3. Install() 从 BinaryMirrors 下载
//   4. Install() 从 hc-install 官方源下载
func EnsureTerraform(ctx context.Context, cfg Config) (path string, err error)

// PrewarmProviderCache 启动时预热:在临时 workspace 跑一次 init,
// provider 下载进 PluginCacheDir,生成的 .terraform.lock.hcl 写入 lockPath。
// 之后每个 deployment init 复制 lock → 命中缓存(秒级,断网可用)。
// 幂等:lock 已存在时复用,下次启动秒级。
func PrewarmProviderCache(ctx context.Context, cfg Config, providerSource, lockPath string) error
```

### Client

```go
type Client struct {
    // 内部字段，不暴露
}

// NewClient 创建一个绑定到 workDir 的 Terraform 客户端。
// 内部完成: 确保二进制 → 生成 .terraformrc → 合并环境变量 → 初始化 tfexec
func NewClient(ctx context.Context, workDir string, cfg Config) (*Client, error)

// WorkDir 返回工作目录路径
func (c *Client) WorkDir() string
```

### 命令

所有命令返回结构化结果，不返回格式化字符串。格式化由上层负责。

```go
// Init — terraform init，下载 provider 插件
func (c *Client) Init(ctx context.Context) error

// Validate — terraform validate，语法检查
func (c *Client) Validate(ctx context.Context) (*ValidateResult, error)

// Format — terraform fmt，格式化 .tf 文件
// write=true 时写回文件，=false 时只返回差异
func (c *Client) Format(ctx context.Context, write bool) ([]string, error)

// Plan — terraform plan -out=tfplan，生成并保存计划
func (c *Client) Plan(ctx context.Context) (*Plan, error)

// ShowPlan — 读取已保存的 tfplan 文件，返回结构化计划
// 用于 plan 生成后、apply 之前再次检查
func (c *Client) ShowPlan(ctx context.Context) (*Plan, error)

// PlanDestroy — terraform plan -destroy -out=tfdestroy，生成并保存销毁计划
// 用于 destroy 之前预览将销毁哪些资源
func (c *Client) PlanDestroy(ctx context.Context) (*Plan, error)

// ShowDestroyPlan — 读取已保存的 tfdestroy 文件，返回结构化销毁计划
func (c *Client) ShowDestroyPlan(ctx context.Context) (*Plan, error)

// Apply — terraform apply tfplan，应用已保存的计划
// 调用前必须先调用 Plan() 生成 tfplan 文件，否则返回清晰错误
func (c *Client) Apply(ctx context.Context) (*ApplyResult, error)

// Destroy — terraform destroy，销毁所有资源
// 返回被销毁资源的地址列表
func (c *Client) Destroy(ctx context.Context) ([]string, error)

// Output — terraform output，获取 stack 输出值
func (c *Client) Output(ctx context.Context) (map[string]Output, error)
```

## 结构化类型

```go
type Plan struct {
    Summary  Summary            // 汇总: +n ~n -n
    Changes  []ResourceChange   // 逐资源变更明细
    Raw      *tfjson.Plan       // 原始 JSON，高级用途
}

type Summary struct {
    Create int
    Update int
    Delete int
    Noop   int
}

type ResourceChange struct {
    Address string          // "huaweicloud_compute_instance.web"
    Type    string          // "huaweicloud_compute_instance"
    Action  Action          // create / update / delete / noop
    Before  json.RawMessage // 变更前状态 (destroy 时有值)
    After   json.RawMessage // 变更后状态 (create/update 时有值)
}

type Action string

const (
    ActionCreate Action = "create"
    ActionUpdate Action = "update"
    ActionDelete Action = "delete"
    ActionNoop   Action = "noop"
)

type ValidateResult struct {
    Valid      bool
    Errors     []string
    Warnings   []string
}

type ApplyResult struct {
    Outputs   map[string]Output
    Resources []string // 已创建/修改的资源地址
}

// Destroy 返回被销毁的资源地址列表

type Output struct {
    Type      string         // "string", "number", "list", ...
    Value     json.RawMessage
    Sensitive bool
}
```

## 二进制安装流程

```
EnsureTerraform(cfg)
  │
  ├─ cfg.BinaryPath 有值？
  │    └─ 是 → 检查文件存在 → 返回
  │
  ├─ Detect() 在 PATH 中找到？
  │    └─ 是,且版本匹配 (或未指定版本) → 返回
  │
  ├─ 遍历 cfg.BinaryMirrors
  │    └─ 逐个尝试下载:
  │         URL = <mirror>/terraform/<version>/terraform_<version>_<os>_<arch>.zip
  │         下载 → 解压 → 返回
  │
  └─ hc-install 官方源
       └─ 指定版本 → ExactVersion → 下载 → 返回
       └─ 未指定版本 → LatestVersion → 下载最新 → 返回
```

安装位置: `~/.hwcloud/bin/terraform-<version>`($HOME 缺失时 Install 会强制转绝对路径,避免相对路径 exec 失败)

## Provider 镜像配置

`NewClient` 内部，如果 `ProviderMirrors` 非空，生成 `.terraformrc`:

```hcl
provider_installation {
  network_mirror {
    url = "https://mirrors.tencent.com/terraform/"
    include = ["registry.terraform.io/*/*"]
  }
  network_mirror {
    url = "https://mirrors.aliyun.com/terraform/"
    include = ["registry.terraform.io/*/*"]
  }
  filesystem_mirror {
    path = "/opt/tf-provider-mirror"
    include = ["registry.terraform.io/*/*"]
  }
  direct {
    // 只排除镜像覆盖的 provider — 辅助 provider(random/tls 等)仍走官方源
    exclude = ["registry.terraform.io/huaweicloud/*"]
  }
}
```

写入 `<workDir>/.terraformrc`，设 `TF_CLI_CONFIG_FILE=<workDir>/.terraformrc`。

URL 和本地路径的区分: 以 `http://` 或 `https://` 开头 → `network_mirror`，否则 → `filesystem_mirror`。

## DryRun 模式

`Config.DryRun = true` 时不调 terraform 二进制，返回模拟结果:

- `Init` → 返回 nil
- `Plan` → 扫描 workDir 下 `.tf` 文件，返回模拟 Plan
- `Apply` → 返回模拟 ApplyResult
- `Destroy` → 返回 nil
- `Output` → 返回模拟 outputs

用于开发、测试、CI。

## 上层使用示例

```go
// cmd/mcp/iac-server — 启动时预热 + generate_terraform_plan 内部

// 1. 启动时预热(后台 goroutine,不阻塞 MCP initialize):
cfg := iac.Config{
    Env:             cloud.Env(), // HW_ACCESS_KEY, HW_SECRET_KEY, HW_REGION, ...
    ProviderMirrors: []string{huaweicloud.DefaultProviderMirror}, // 默认镜像,用户配置追加
    PluginCacheDir:  filepath.Join(iacHome, "plugins"),           // 共享 provider 缓存
}
iac.PrewarmProviderCache(ctx, cfg, cloud.ProviderSource(), filepath.Join(cloudHome, "terraform.lock.hcl"))

// 2. generate_terraform_plan:写 .tf 后 init + plan
//    (复制 <cloudHome>/terraform.lock.hcl 进 deployment → init 命中缓存)
client, err := iac.NewClient(ctx, workDir, cfg)
if err := client.Init(ctx); err != nil { ... }   // init 失败直接报错(环境/网络),不重试
plan, err := client.Plan(ctx)
// plan.Summary.Create = 3, plan.Changes = [...]

// 3. apply_deployment:
client, _ := iac.NewClient(ctx, workDir, cfg)
result, err := client.Apply(ctx)
// result.Outputs = {ip: "123.60.x.x", ...}
```

## 不做的事

- **不管模板生成** — 上层 (MCP server) 负责 .tf 文件内容
- **不管费用估算** — 上层根据 Plan 结构自行计算
- **不管状态存储** — 默认 local state，远程 backend 由 .tf 配置决定
- **不管 workflow** — 后续单独做 workflow runtime，iac 只是执行单元
- **不管多 workspace** — 如需要，上层通过不同 workDir 隔离
