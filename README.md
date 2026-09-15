# GopherHarness｜Go 原生本地编码 Agent 运行时

GopherHarness 是一个使用 Go 构建的本地编码 Agent 运行时。它以 Pico v3 的行为契约为参考，把模型、工具、上下文、记忆、权限、持久化、恢复、工作线程和评测统一在一条可审计的执行链路中。

它适合希望在本地运行、学习和扩展 AI 编程 Agent 的开发者：模型负责理解和规划，GopherHarness 负责安全地执行、记录和验证结果。

本仓库是独立的 Go 实现，不依赖本地 Python 项目路径。Python v3 仅作为行为参考，便于进行 parity 和 golden contract 校验。

> 本 README 面向中文读者；命令、配置字段和技术名词保留英文，便于直接复制和检索。

## 项目能力

- 支持 OpenAI-compatible Responses 和 Anthropic-compatible Messages provider。
- 兼容 Pico 文本协议：`<tool>...</tool>` 与 `<final>...</final>`。
- 完整的 turn 状态机，包含重试、步数、重复调用和取消边界。
- 文件读写、搜索、Shell、图片检查、Todo、计划模式、用户提问和 Worker 生命周期等工具。
- 工作区与符号链接越界保护、写入前 fresh-read、审批模式和破坏性 Shell 检查。
- 支持 Linux Bubblewrap 和 macOS `sandbox-exec` 隔离后端。
- 工作记忆、文件摘要、相关笔记召回、持久事实晋升和受控 dream consolidation。
- Checkpoint、Session、事件 JSONL、运行轨迹、报告和长输出 artifact 全部落盘到 `.pico/`。
- 隔离的 goroutine Worker，支持取消和实时消息队列。
- 支持从 `SKILL.md` 发现项目级和用户级 Skills。
- 提供纯 REPL、流式 ANSI TUI、Python v3/Go golden contract 和运行时回归评测。
- 最终答案治理、递归证据脱敏、校验后的 checkpoint 恢复和确定性的 Worker 关闭。

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
```

## 配置 provider

复制 `.pico.toml.example` 为 `.pico.toml`，只填写你实际使用的 provider；`.pico.toml` 已被 Git 忽略。

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
CLI 参数 > 环境变量 > 项目 .pico.toml > 默认值
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

## 评测

benchmark schema 参考 Python 项目的任务结构：fixture repo、prompt、允许的工具、步数预算、预期 artifact 和 verifier 命令。

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

见 [benchmark baseline](benchmarks/baseline.md) 查看预期的 20/20 运行时结果。这套任务不等同于公开模型能力分数；真实公开 benchmark 的接入约束见 [benchmark 体系](benchmarks/README.md)。

## 开发与验证

```bash
go vet ./...
go test -race ./...
make contract
make build
```

架构和学习资料：[请求链路](docs/REQUEST_FLOW.md)、[系统架构](docs/ARCHITECTURE.md)、[Python v3 到 Go 的 parity map](docs/PARITY.md) 和 [Go 学习路径](docs/LEARNING.md)。
