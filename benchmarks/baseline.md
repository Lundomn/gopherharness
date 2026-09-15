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

生成的 JSON artifact 包含每个任务的 attempts、tool steps、verifier 状态、stop reason、预期失败处理、类别统计和耗时。任务 20 是有意设置的负向控制：只有 runtime 以 `step_limit_reached` 停止时才算通过。Runtime ID 和时间戳会在运行时生成，不会作为 source fixture 提交。
