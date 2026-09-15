# 请求链路

这是 GopherHarness 最短的端到端调用路径。每个箭头都是明确的 package 边界；持久化和策略判断不会隐式发生在 provider 或工具内部。

```text
cmd/gopherharness 或 cmd/gopherharness-tui
        |
        v
internal/app.Run
  flags -> config -> provider capabilities -> runtime.Options
        |
        v
runtime.New
  workspace + session + memory + tools + workers + sandbox + redactor
        |
        v
Agent.Ask(ctx, request)
  串行化主 session，并创建 RunStore + evidence.Recorder
        |
        v
contextbuilder.Build -> provider.Complete/Stream -> protocol.Parse
        |                                      |
        | 工具调用                                 | 最终回答/重试
        v                                      v
allowlist -> plan -> approval -> handler    governance.Evaluate
        |                                      |
        v                                      v
workspace/sandbox -> ToolResult            提醒/阻止/允许
        |                                      |
        +----------> session + checkpoint <---+
                         |
                         v
             脱敏后的 trace/report/session
                         |
                         v
                    可选 auto-dream
```

## 一次工具调用

1. `protocol.Parse` 创建 `model.ToolCall`。
2. `runtime.executeTool` 检查当前 allowlist。
3. plan mode 限制写操作，随后由 `permission.Check` 应用用户策略。
4. `tools.Registry` 查找注册的 `Spec + Handler` 对。
5. handler 只调用窄接口 `tools.Host`，不能直接访问 runtime 内部。
6. 路径经过 `workspace.Resolve`；Shell 命令经过 `sandbox.Run`。
7. `model.ToolResult` 返回 transcript，同时 `evidence.Recorder` 写入递归脱敏后的 event。
8. 工作区发生变更时重置验证状态；识别出的 test/build/lint 命令会更新验证证据。

## 一次最终回答

1. `protocol.Parse` 提议最终文本。
2. `governance.Evaluate` 检查变更路径、验证证据和活动 worker。
3. `off`、`warn`、`soft`、`strict` 决定允许、提醒或阻止提议。
4. 接受后写入带当前 workspace fingerprint 的 checkpoint，退出 plan mode，晋升持久 memory，并写入 run report。
5. session 和 interval 门控通过后，可能提交 auto-dream。

## 恢复链路

`runtime.New --resume` 加载 session 和最近一次完成的 run checkpoint，比较 checkpoint schema 与 workspace fingerprint，并记录以下状态之一：

```text
no-checkpoint
full-valid
schema-mismatch
workspace-mismatch
```

只有 `full-valid` checkpoint 会将 goal 和 next step 恢复到 working memory；状态会写入下一份 `task_state.json`。

## 扩展规则

- 新增模型协议：实现 `provider.Client`；只有 provider 确实支持时才增加 `StreamClient` 或 `VisionClient`。
- 新增工具：注册一个包含公共 `Spec` 和 `Handler` 的 `tools.Definition`；操作系统和工作区访问必须经过 `tools.Host`。
- 新增证据：通过 `evidence.Recorder` 发出，不要在业务功能中直接写 trace JSONL。
- 新增终态策略：在 `internal/governance` 增加证据和决策，不要放在 provider 或 TUI。
- 新增后台任务：必须拥有取消路径，并纳入 `Agent.Close` 的等待范围。
