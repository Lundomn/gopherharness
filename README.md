# GopherHarness｜Go 原生本地编码 Agent 运行时

GopherHarness 是一个使用 Go 构建的本地编码 Agent 运行时，把模型、工具、上下文、记忆、权限、持久化、恢复、工作线程和评测统一在一条可审计的执行链路中。

它适合希望在本地运行、学习和扩展 AI 编程 Agent 的开发者：模型负责理解和规划，GopherHarness 负责安全地执行、记录和验证结果。

本仓库是独立、可直接运行的 Go 项目，不依赖其他语言版本或本地路径。

> 本 README 面向中文读者；命令、配置字段和技术名词保留英文，便于直接复制和检索。

## 项目能力

核心包含会话服务调度、多步历史管理、安全读取并发、MCP 客户端、持久费用预算及父子阶段追踪。除本地终端外，提供飞书长连接/加密回调入口和持久收发记录，以及受控候选验证与人工激活。具体接线、验证范围与限制见 [能力状态](docs/GO_REWRITE_STATUS.md)。

本轮代码补齐与离线验证结果见 [本地验收记录](docs/VALIDATION.md)。

- 支持 OpenAI-compatible Responses 和 Anthropic-compatible Messages provider。
- 两类模型接口使用原生消息、工具调用与结果协议，保留推理字段；旧文本客户端兼容 `<tool>...</tool>` 与 `<final>...</final>`。
- 显式的任务轮次状态机，包含协议重试、步数、重复调用和取消边界。
- 文件读写、搜索、Shell、图片检查、Todo、计划模式、用户提问和 Worker 生命周期等工具。
- 工作区与符号链接越界保护、写入前 fresh-read、审批模式和破坏性 Shell 检查。
- 支持 Linux Bubblewrap 和 macOS `sandbox-exec` 隔离后端。
- 记忆输入固定两路：宿主 `user.md` 长期背景与后端按当前问题召回，统一形成一个记忆段；工作状态单独维护。
- 常驻技能与本地 BM25 技能路由：最多 5 个候选、明确激活最多 2 份正文，其余引用可由获准的 `skill_read` 读取。
- 带作用域和来源的事实存储，以及受控记忆归并（dream consolidation）。
- Checkpoint、Session、事件 JSONL、运行轨迹、报告和长输出 artifact 全部落盘到本地运行时数据目录。
- 独立会话的 goroutine Worker（子任务）：默认 4 并发、每小时 30 次创建、50 条跟进消息；完成结果持久保存，安全点去重回注，服务空闲时做最多 2 次无工具汇总。重启不复活旧子进程；不是文件系统隔离。
- 持久异步审批：冻结具体工具参数、目录版本和文件前置条件；等待时释放执行容量但暂停原会话，批准后重新检查再单次领取。控制面显式查询与重试，未知副作用不自动重放。
- 支持从 `SKILL.md` 发现项目级和用户级 Skills。
- 提供纯 REPL、流式 ANSI TUI、Go golden contract 和运行时回归评测。
- 最终答案治理、递归证据脱敏、校验后的 checkpoint 恢复和确定性的 Worker 关闭。
- 会话串行与前后台容量池，支持追加、安全点注入和打断；只读工具按显式声明有界并发。
- 飞书入口默认仅允许读取、列目录和搜索；白名单准入、群提及过滤、来源回复和持久去重。
- 词元预算：模型窗口与输出预留分开配置，固定材料先占位、历史六成触发管理，所有选择路径统一复算完整输入。
- 费用预算：每次请求先持久预留再发送，主模型、历史管理器和子任务共享额度；未知用量保留占用，支持人工对账。
- 维护闭环：固定训练/未见用例、候选独立工作树、有限尝试、人工激活及版本回滚，不自动提交或部署。

### 上下文预算

窗口跟随 provider 配置；未配置时使用项目默认 **65,536 词元**、输出预留 **8,192**，完整输入上限 **57,344**，不自动探测服务商容量。规则、工作区、技能、记忆、工作状态、工具 Schema 和当前请求先占位，再扣 8 个协议余量词元；历史严格小于剩余额度的 60% 才走快速路径，等于阈值就进入管理。

每轮任务完整组装一次，后续工具结果和安全点注入消息增量追加。历史管理器最多 12 步、默认总计 30 秒，可查找/读取历史、归档、评分和维护工作状态；保护开头 6 条消息覆盖的整组与最新组，出错回退规则。计数错误或固定与保护材料仍超限就不调用主模型。原生请求后续膨胀时缩减旧工具正文并保留最新 3 条结果，不重跑工具。

本地使用嵌入词表的 `cl100k_base` 编码估算，也可配置 `o200k_base`；不保证与所有供应商的分词和协议开销完全一致。可用 `--history-selector rule` 关闭辅助模型管理。

```sh
./bin/gopherharness --context-window-tokens 65536 --max-new-tokens 8192 \
  --context-tokenizer cl100k_base --history-selector rule "检查项目"
```

`--prompt-budget` 仅为可选额外字节上限，默认 0（关闭）；不能代替词元预算。子任务继承预算，备用模型按最小窗口和各编码计数最大值保守准入。具体分配、元数据和异常边界见[上下文设计](docs/CONTEXT_BUDGET.md)。

### 记忆与技能

宿主资料按二级标题选最多 2 个正分章节并保留 `Notes`；后端携带用户、仓库和当前查询召回最多 5 条，两路合成一个记忆段。工作状态和本会话历史不算其中一路。合法无命中为空，已启用后端的硬失败会中止本轮。

默认使用本地记忆；`[memory]` 或 `--memory-url` 可接实现本项目 JSON v1 协议的外部服务。完成后的记忆写入通过持久待写队列异步处理，跨用户资料隔离；不是任意记忆产品的通用 URL 接口。共享服务默认不加载启动账户的个人技能，本地可用 `--no-personal-skills` 关闭。

技能用本地 BM25 Okapi：`k1=1.5`、`b=0.75`，索引两次名称、一次描述、正文前 4,000 个字符；英文按词、中文按单字。常驻最多 5 份，查询候选最多 5 份，明确点名最多激活 2 份完整正文，其余给引用，不调用额外模型门控。读取技能只是获得方法，不等于执行或授权。详见[记忆与技能数据流](docs/MEMORY_AND_SKILLS.md)。

### MCP 接入

通过 `--mcp-config servers.json` 加载受信任的服务器目录，例如：

```json
[{"name":"local","transport":"stdio","command":"your-mcp-server","args":[]}]
```

也支持 `sse` 和 `streamableHttp`，使用 `url` 指定地址。工具默认按风险动作审批，不因远端声明只读就开放并发。沙箱模式下不支持受控进程启动时，stdio 配置拒绝回退到宿主机。

模型调用记录位于 `.pico/telemetry/<session>.calls.jsonl`，正常关闭后有独立健康快照；缺少用量或价格时费用未知。配置 `[runtime] provider_retries`、`fallback_providers` 可启用有界重试与备用模型；已输出流式片段后不透明重试。

### 费用准入

在 provider 下明确填写四种价格后，通过 `[runtime] cost_limit_nanousd` 或 `--cost-limit-nanousd` 开启共享费用预算；1 美元等于 1,000,000,000 纳美元，0 关闭。每个实际请求分别预留、发送、结算；重启后发送结果未知的请求不会假退款。使用 `/budget` 查看额度，核对供应商记录后用 `/budget reconcile` 人工结算，详见[费用预算](docs/COST_BUDGET.md)。

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
bin/gopherharness-loadtest
bin/gopherharness-maintain
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
context_window_tokens = 65536
max_output_tokens = 8192
tokenizer = "cl100k_base"
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
/help /session /memory /dream /skills /todo /workers /usage /budget /reset /exit
```

### 飞书消息入口

按 [飞书部署与安全说明](docs/FEISHU.md) 配置应用凭证、允许用户和模型配置。长连接接收事件是默认入口；使用加密回调时明确选择 webhook：

```bash
./bin/gopherharness-gateway --cwd /path/to/dedicated-workspace \
  --transport websocket --listen 127.0.0.1:8080 --busy append

# 需要可达 HTTPS 回调和事件校验/加密配置
./bin/gopherharness-gateway --cwd /path/to/dedicated-workspace \
  --transport webhook --listen 127.0.0.1:8080 --busy append
```

webhook 回调路径为 `/events/feishu`，存活检查为 `/healthz`；长连接不要求公网事件回调。默认只开放读文件、目录和搜索，不因接入群聊就授权 Shell、文件修改或任意 MCP 工具；本地 CLI/TUI 的权限策略保持独立。当前测试使用本地模拟平台，不代表真实飞书应用已验收。

## 安全模型

高风险工具支持 `--approval ask|auto|never`。文件修改前必须先读取已有文件；所有路径（包括符号链接目标）都会解析并检查是否越出工作区。Shell 策略会拦截已知的破坏性命令。

沙箱模式：

```bash
--sandbox off
--sandbox best_effort
--sandbox required --sandbox-backend auto
```

`required` 在隔离后端不可用时会失败关闭；`auto` 会在 Linux 选择 Bubblewrap，在 macOS 选择 `sandbox-exec`。只读场景可以使用 `workspace_write = false`。

`--deny-network` / `deny_network = true` 禁止 Shell 子进程联网，必须有 OS 沙箱；不影响主进程访问模型和已配置的外部服务。子进程输出总捕获上限 1 MiB，分别保留 stdout/stderr 头尾并标记省略。当前没有 cgroup CPU/内存/进程配额，隔离配置不能当成完整容器安全策略。

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
  --offline --allow-host-verifier \
  --benchmark benchmarks/benchmark.json \
  --fixtures benchmarks \
  --artifact artifacts/go-benchmark.json
```

仓库内置 20 个确定性的 Runtime Regression 任务，覆盖工作区 I/O、Shell、状态、计划治理、协议恢复、工具白名单、路径安全、fresh-read、超时、长输出 artifact、记忆、checkpoint、脱敏和步数限制负向控制。无需 provider key 即可运行：

```bash
make benchmark
```

见 [回归基线](benchmarks/baseline.md) 查看预期的 20/20 运行时结果。普通任务必须正常返回最终答案，产物和独立校验通过且未超步数；仅生成正确文件但异常停止不会判通过。负向控制也必须满足产物、预算和校验条件。这套任务不等同于公开模型能力分数；公开评测接入约束见 [benchmark 体系](benchmarks/README.md)。

`--allow-host-verifier` 明确允许在宿主运行受信任校验命令；未授权时必须配置 required 沙箱。公开用例另有本地 SWE-bench / Terminal-Bench 导入、补丁导出和官方结果读取接口，导入不会下载或执行测试，也不自动产生官方成绩。

离线负载工具可测试队列、容量、取消、错误和资源收束，报告本机 Mock（模拟模型）请求的延迟分位数：

```sh
./bin/gopherharness-loadtest --requests 200 --concurrency 16 \
  --sessions 16 --user-slots 4 --system-slots 2 \
  --artifact artifacts/offline-load.json
```

它不调用真实模型，不能把报告当成真实模型吞吐、线上群聊容量或公开任务成功率。

## 候选维护与激活

`gopherharness-maintain run` 接收本地补丁和固定训练/未见验证契约，必须明确 `--approve-verifiers` 才执行校验命令。通过训练改进与未见验证后仍需 `activate --approve-activation`；版本检查与补丁摘要防止过期确认或修改后的候选被直接启用。激活只切换本地工作树指针，下一次运行显式指定 `--active-candidate` 才使用，不热替换运行进程。完整契约、命令和回滚见[维护说明](docs/MAINTENANCE.md)。

## 开发与验证

```bash
make check
make contract
make build
```

架构和学习资料：[请求链路](docs/REQUEST_FLOW.md)、[系统架构](docs/ARCHITECTURE.md) 和 [Go 学习路径](docs/LEARNING.md)。

GitHub Actions 在 Linux 和 macOS 上执行格式检查、静态检查、竞争检测、协议契约、编译及 20 个离线回归任务，并保留任务报告；不配置或调用收费模型。
