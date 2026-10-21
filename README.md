# GopherHarness｜Go 原生本地编码 Agent 运行时

GopherHarness 是一个使用 Go 构建的本地编码 Agent 运行时，把模型、工具、上下文、记忆、权限、持久化、恢复、工作线程和评测统一在一条可审计的执行链路中。

它适合希望在本地运行、学习和扩展 AI 编程 Agent 的开发者：模型负责理解和规划，GopherHarness 负责安全地执行、记录和验证结果。

本仓库是独立、可直接运行的 Go 项目，不依赖其他语言版本或本地路径。

> 本 README 面向中文读者；命令、配置字段和技术名词保留英文，便于直接复制和检索。

## 项目能力

核心包含会话服务调度、整组历史选择、安全读取并发、MCP 客户端、逐次调用账本及父子阶段追踪。除本地终端外，提供飞书加密事件入口和持久收发记录。具体接线、验证范围与限制见 [能力状态](docs/GO_REWRITE_STATUS.md)。

- 支持 OpenAI-compatible Responses 和 Anthropic-compatible Messages provider。
- 支持标准文本协议：`<tool>...</tool>` 与 `<final>...</final>`。
- 显式的任务轮次状态机，包含协议重试、步数、重复调用和取消边界。
- 文件读写、搜索、Shell、图片检查、Todo、计划模式、用户提问和 Worker 生命周期等工具。
- 工作区与符号链接越界保护、写入前 fresh-read、审批模式和破坏性 Shell 检查。
- 支持 Linux Bubblewrap 和 macOS `sandbox-exec` 隔离后端。
- 会话级工作记忆、笔记召回、带作用域和来源的事实存储，以及受控记忆归并（dream consolidation）。
- Checkpoint、Session、事件 JSONL、运行轨迹、报告和长输出 artifact 全部落盘到本地运行时数据目录。
- 独立会话的 goroutine Worker（子任务），支持取消和消息队列；不是进程级或文件系统级隔离。
- 支持从 `SKILL.md` 发现项目级和用户级 Skills。
- 提供纯 REPL、流式 ANSI TUI、Go golden contract 和运行时回归评测。
- 最终答案治理、递归证据脱敏、校验后的 checkpoint 恢复和确定性的 Worker 关闭。
- 会话串行与前后台容量池，支持追加、安全点注入和打断；只读工具按显式声明有界并发。
- 飞书入口默认仅允许读取、列目录和搜索；白名单准入、群提及过滤、来源回复和持久去重。

### MCP 接入

通过 `--mcp-config servers.json` 加载受信任的服务器目录，例如：

```json
[{"name":"local","transport":"stdio","command":"your-mcp-server","args":[]}]
```

也支持 `sse` 和 `streamableHttp`，使用 `url` 指定地址。工具默认按风险动作审批，不因远端声明只读就开放并发。沙箱模式下不支持受控进程启动时，stdio 配置拒绝回退到宿主机。

模型调用记录位于 `.pico/telemetry/<session>.calls.jsonl`，正常关闭后有独立健康快照；缺少用量或价格时费用未知。它目前不是完整的缓存优化、自动模型路由或供应商账单系统。

## 环境要求

- Go 1.27 或更高版本。
- 运行模型需要 DeepSeek、OpenAI-compatible 或 Anthropic-compatible provider key。
- Linux 的强隔离模式需要 Bubblewrap（`bwrap`）；macOS 优先使用系统自带的 `sandbox-exec`。

## 编译

```bash
cd gopherharness
make build
```

生成的程序位于：

```text
bin/gopherharness
bin/gopherharness-tui
bin/gopherharness-eval
bin/gopherharness-contract
bin/gopherharness-gateway
```

## 配置 provider

复制示例配置文件为项目配置文件，只填写你实际使用的 provider；本地配置文件已被 Git 忽略。

```toml
provider = "deepseek"

[providers.deepseek]
protocol = "anthropic"
api_key = "your-key"
base_url = "https://api.deepseek.com/anthropic"
model = "deepseek-v4-pro"
```

配置优先级为：

```text
CLI 参数 > 环境变量 > 项目配置文件 > 默认值
```

## 运行

```bash
# 交互式终端界面
./bin/gopherharness-tui --cwd /path/to/repository

# 纯 REPL
./bin/gopherharness --repl --cwd /path/to/repository

# 一次性任务
./bin/gopherharness --cwd /path/to/repository \
  "检查失败的测试并提出修复方案"

# 恢复最近一次会话
./bin/gopherharness --cwd /path/to/repository --resume latest
```

REPL/TUI 常用命令：

```text
/help /session /memory /dream /skills /todo /workers /usage /reset /exit
```

### 飞书消息入口

按 [飞书部署与安全说明](docs/FEISHU.md) 配置应用凭证、事件加密、允许用户和模型配置后：

```bash
./bin/gopherharness-gateway --cwd /path/to/dedicated-workspace \
  --listen 127.0.0.1:8080 --busy append
```

回调路径为 `/events/feishu`，存活检查为 `/healthz`。对外访问需使用 HTTPS 反向代理。此入口不开放 Shell、文件写入或任意 MCP 工具；本地 CLI/TUI 的权限策略保持独立。当前测试使用本地模拟平台，不代表真实飞书应用已验收。

## 安全模型

高风险工具支持 `--approval ask|auto|never`。文件修改前必须先读取已有文件；所有路径（包括符号链接目标）都会解析并检查是否越出工作区。Shell 策略会拦截已知的破坏性命令。

沙箱模式：

```bash
--sandbox off
--sandbox best_effort
--sandbox required --sandbox-backend auto
```

`required` 在隔离后端不可用时会失败关闭；`auto` 会在 Linux 选择 Bubblewrap，在 macOS 选择 `sandbox-exec`。只读场景可以使用 `workspace_write = false`。

图片检查默认使用主 provider。也可以单独指定支持视觉的 profile，而不改变主模型：

```bash
./bin/gopherharness-tui --vision-provider openai --vision-model gpt-5.4
```

自动 dream consolidation 默认在累计五个 session 后启动，且每 24 小时最多一次。可使用 `--dream-min-sessions` 和 `--dream-interval` 调整，使用 `--no-auto-dream` 禁用，或手动执行 `/dream`。

使用 `--final-readiness off|warn|soft|strict` 控制未验证的工作区变更在最终回答前是记录、提醒一次还是阻止提交。

审批和路径检查不是完整安全沙箱，读后再写仍存在并发竞态；不要将不可信仓库与生产密钥放在同一运行环境。详细边界见 [系统架构](docs/ARCHITECTURE.md)。

## 评测

benchmark schema 使用 fixture repo、prompt、允许的工具、步数预算、预期 artifact 和 verifier 命令。

```bash
./bin/gopherharness-eval \
  --benchmark benchmarks/benchmark.json \
  --fixtures benchmarks \
  --artifact artifacts/go-benchmark.json
```

仓库内置 20 个确定性的 Runtime Regression 任务，覆盖工作区 I/O、Shell、状态、计划治理、协议恢复、工具白名单、路径安全、fresh-read、超时、长输出 artifact、记忆、checkpoint、脱敏和步数限制负向控制。无需 provider key 即可运行：

```bash
make benchmark
```

见 [回归基线](benchmarks/baseline.md) 查看预期的 20/20 运行时结果。普通任务必须正常返回最终答案，产物和独立校验通过且未超步数；仅生成正确文件但异常停止不会判通过。负向控制也必须满足产物、预算和校验条件。这套任务不等同于公开模型能力分数；公开评测接入约束见 [benchmark 体系](benchmarks/README.md)。

## 开发与验证

```bash
make check
make contract
make build
```

架构和学习资料：[请求链路](docs/REQUEST_FLOW.md)、[系统架构](docs/ARCHITECTURE.md) 和 [Go 学习路径](docs/LEARNING.md)。

GitHub Actions 在 Linux 和 macOS 上执行格式检查、静态检查、竞争检测、协议契约、编译及 20 个离线回归任务，并保留任务报告；不配置或调用收费模型。
