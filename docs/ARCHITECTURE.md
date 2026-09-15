# GopherHarness 系统架构

GopherHarness 按职责划分为三个明确的平面：

```text
控制平面
  cmd/gopherharness、cmd/gopherharness-tui
    -> internal/app
    -> internal/runtime.Agent
    -> internal/runtime.run 回合状态机
    -> provider / protocol / tools / governance

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
5. `contextbuilder.Build` 组装稳定指令、工具、skills、workspace、memory、历史记录和当前请求。
6. provider 返回 JSON 或 SSE 文本；`protocol.Parse` 将完整响应分类为工具调用、最终回答或重试。TUI 只流式展示最终回答文本。
7. 每个工具执行前都要经过 allowlist、plan mode、approval、注册 handler、workspace 和 sandbox 边界。
8. 工具输出加入 transcript；过大的输出转为 run artifact。runtime 同步更新 memory、checkpoint、trace 和 session event。
9. final readiness 检查变更路径、验证证据和活动 worker，决定是否接受回答。
10. 接受最终回答后，写入最新 checkpoint、晋升持久事实、关闭任务状态和报告，并可在门控通过后提交后台 dream consolidation。

## 并发模型

主 session 由 mutex 保护，避免两个调用者同时修改 transcript。Worker 在独立 session 中运行于 goroutine，拥有步数上限、取消 context 和已消费的 inbox channel。Dream 在受限后台 goroutine 中运行，由文件锁和 wait group 保护。`Agent.Close` 会取消并等待 worker，再等待 memory maintenance。共享 memory 和 workspace read tracking 各自拥有锁。

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
  memory/working.json
  memory/topics/project.md
  memory/topics/dream.md
  memory/dream-state.json
  todos.json
  plans/active.md
```

可能包含 provider 或任务数据的文件会使用受限权限写入。trace、session event 和 session snapshot 在落盘前都会经过递归的 key、pattern 和配置 secret 脱敏。

## Go 设计选择

- `context.Context` 统一管理取消和 Shell/模型超时。
- 使用接口隔离 provider 与工具宿主，便于无网络测试。
- 使用 goroutine 和 channel 实现隔离的后台 worker。
- 使用具体 Go struct 承载数据契约，在 registry 和边界方法执行校验。
- 全部使用标准库，运行时不依赖第三方 Go 包。

完整请求链路和扩展规则见[请求链路文档](REQUEST_FLOW.md)。
