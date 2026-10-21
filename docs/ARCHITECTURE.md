# GopherHarness 系统架构

GopherHarness 按职责划分为三个明确的平面：

```text
控制平面
  cmd/gopherharness、cmd/gopherharness-tui
    -> internal/app
    -> internal/runtime.Agent
    -> internal/runtime.run 回合状态机
    -> provider / protocol / tools / governance

  cmd/gopherharness-gateway
    -> gatewayapp -> feishu -> gateway -> scheduler
    -> runtime.Service -> runtime.Agent
    -> journal Inbox/Outbox -> delivery -> 平台回复

状态平面
  session -> working memory -> checkpoint -> todo / plan / worker

证据平面
  runtime event -> evidence.Recorder -> redactor
    -> task_state.json / trace.jsonl / session events / report.json
```

## 请求生命周期

1. `app.Run` 解析命令行参数、项目配置、环境变量和 provider。
2. `runtime.New` 校验恢复 checkpoint，并创建 workspace、session、memory、工具注册表、plan、todo、worker、skills、sandbox 和 redactor。
3. `Agent.Ask` 为主 session 串行化回合。
4. `run` 创建运行目录，并在调用模型前写入第一份任务状态。
5. 在安全点取出中途补充消息；`contextbuilder.BuildChecked` 组装稳定指令、工具、skills、workspace、memory、历史记录和当前请求，检查工具关联闭包及最终文本字节上限。
6. provider 返回 JSON 或 SSE 文本；`protocol.Parse` 将完整响应分类为工具调用、最终回答或重试。TUI 只流式展示最终回答文本。
7. 每个工具执行前经过 allowlist、plan mode、approval、JSON Schema 参数校验和 handler；文件和进程操作分别进入 workspace、sandbox 边界。仅显式声明安全的只读工具连续段并发，默认四路，写操作形成屏障。
8. 工具输出加入 transcript；过大的输出转为 run artifact。runtime 同步更新 memory、checkpoint、trace 和 session event。
9. final readiness 检查变更路径、验证证据和活动 worker，决定是否接受回答。
10. 接受最终回答后，写入最新 checkpoint、晋升持久事实、关闭任务状态和报告，并可在门控通过后提交后台 dream consolidation。

## 并发模型

主 session 由 mutex 保护，避免两个调用者同时修改 transcript。Service 为不同会话分配独立实例，调度器提供顺序和容量边界。Worker 在独立 session 中运行于 goroutine，拥有步数上限、取消 context 和 inbox channel；不是独立操作系统进程。Dream 由门控、文件锁和 wait group 保护。`Agent.Close` 会取消并等待 worker，再等待记忆维护。

会话隔离不等于文件隔离：多会话仍共享工作区。fresh-read 的读取核对与写入不是原子事务，不能代替多进程文件锁或独立工作树。

渠道关闭先封口、取消并等待调度与投递，最后释放持久日志锁。调用者等待超时后仍保留清理所有者；无法强杀不响应上下文的任意 Go 协程。

## 持久化契约

```text
.pico/
  sessions/<session_id>.json
  sessions/<session_id>.events.jsonl
  runs/<run_id>/task_state.json
  runs/<run_id>/trace.jsonl
  runs/<run_id>/report.json
  runs/<run_id>/checkpoint.json
  runs/<run_id>/artifacts/*
  memory/working*.json
  memory/facts.json
  memory/topics/project.md
  memory/topics/dream.md
  memory/dream-state.json
  telemetry/<session_id>.calls.jsonl
  gateway/journal.json
  media/*
  maintenance/<fix_id>/*
  todos.json
  plans/active.md
```

运行文件使用受限权限；trace、session event 和 session snapshot 通过配置密钥和模式脱敏。归档、附件与 Journal 可能含用户原文，不能认为所有文件都已脱敏，必须保护整个运行目录。

事实和收发日志使用带进程锁、重读、临时文件、同步与替换的文件事务；普通会话快照、检查点和报告不是一个跨文件事务。Journal 重启只自动重排明确未启动的工作，运行中/发送中状态转为需对账/未知，不盲目重放副作用。

## Go 设计选择

- `context.Context` 统一管理取消和 Shell/模型超时。
- 使用接口隔离 provider 与工具宿主，便于无网络测试。
- 使用 goroutine 和 channel 实现隔离的后台 worker。
- 使用具体 Go struct 承载数据契约，在 registry 和边界方法执行校验。
- 并发和业务状态主要使用标准库；MCP 与飞书使用各自官方 Go SDK，参数约束使用 `jsonschema-go`。

上下文是字节预算，不是精确 token 计量；工作区恢复指纹当前为路径、大小和修改时间；词法召回不是向量检索；调用账本不等于自动节费。平台入口默认只开放可信白名单用户的读取能力，不是公开匿名 Agent 服务。

完整请求链路和扩展规则见[请求链路文档](REQUEST_FLOW.md)、[飞书部署](FEISHU.md)及[能力状态](GO_REWRITE_STATUS.md)。
