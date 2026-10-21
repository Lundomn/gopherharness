# GopherHarness 运行时回归基线

> 这是一套运行时回归基线，不是 SWE-bench、Terminal-Bench 或其他公开模型能力排名。

公开 benchmark 的分层和接入约束见 [benchmarks/README.md](README.md)。

这是仓库提交的确定性 benchmark 基线契约。它使用任务内置的 fake provider 响应，因此不需要 API key、网络或在线模型。每个任务都会获得一份全新的 `benchmarks/fixtures/sample` 副本。

## 运行

```bash
make benchmark
```

等价命令：

```bash
go run ./cmd/gopherharness-eval \
  --offline \
  --benchmark benchmarks/benchmark.json \
  --fixtures benchmarks \
  --workspaces artifacts/go-workspaces \
  --artifact artifacts/go-benchmark.json
```

## 预期基线

| Metric | Expected |
| --- | ---: |
| Tasks | 20 |
| Passed | 20 |
| Failed | 0 |
| 类别 | workspace-read、workspace-search、workspace-write、shell、state、governance、reliability、security、protocol、evidence |
| 最大任务步数 | 5 |

生成的 JSON artifact 包含每个任务的 attempts、tool steps、verifier 状态、run status、stop reason、预期失败处理、类别统计和耗时。普通任务要求 `completed + final_answer_returned`，不能把写对产物但异常终止的运行判通过；缺失、损坏或身份不符的运行报告失败关闭。任务 20 是负向控制：要求 `failed + step_limit_reached`，且仍须通过预算、产物和独立校验。Runtime ID 和时间戳动态生成，不作为 fixture 提交。
