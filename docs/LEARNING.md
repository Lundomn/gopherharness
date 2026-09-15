# Go 学习路径

这份项目以 Go 版本为唯一主线，建议按下面的顺序学习。每一步都对应一个清晰的 Go 包边界和可运行测试。

1. 阅读 `internal/model/types.go`，了解落盘数据结构和 schema contract。
2. 阅读 `internal/runtime/engine.go`，跟踪完整的 turn 状态机。
3. 从 `protocol.Parse` 开始跟踪一次工具调用，直到 `tools.Registry` 的 handler。
4. 阅读 `workspace.Resolve` 和 fresh-read 测试，理解工作区安全边界。
5. 阅读 `contextbuilder.Build`，理解 section budget、上下文压缩和当前请求保护。
6. 跟踪 `evidence.Recorder` 到 trace、session 和 run JSONL 的递归脱敏。
7. 分别阅读 memory、plan、todo、worker、skills 和 sandbox 模块。
8. 阅读 `worker.Manager` 和 auto-dream，理解 goroutine、channel、context、锁和 wait group。
9. 跟踪 provider 的 `Complete`、`Stream` 和 `InspectImage`，学习小接口与能力检查。
10. 阅读 `governance.Evaluate`，理解 off、warn、soft、strict 四种终态策略。
11. 跟踪 checkpoint 创建和 resume 时的 workspace fingerprint 校验。
12. 运行 race detector，分析跨 worker 共享的数据和锁。
13. 运行 `make contract`，检查 Go 协议 golden contract。
14. 使用 `gopherharness-eval` 运行 fixture 和 verifier，理解可复现评测。

## 推荐练习

- 保持 runtime 接口不变，将 ANSI TUI 替换为 Bubble Tea。
- 将 golden suite 扩展到 trace event 的归一化比较。
- 为 provider 增加有界 work queue，并测试 backpressure。
- 使用 `go test -fuzz` 测试协议解析和工作区路径解析。
- 为公开 benchmark adapter 增加任务集 hash、环境 digest 和官方 verifier 记录。
