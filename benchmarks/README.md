# Benchmark 体系

本目录包含两类不同目的的评测，名称和结论必须区分：

## 1. GopherHarness Runtime Regression

`benchmark.json` 是仓库内置的 20 个确定性任务。它们使用固定响应和本地 fixture，检查工具协议、沙箱、恢复、记忆、证据和治理行为。

这套基线回答的是：

> GopherHarness 的运行时契约有没有被新的代码改坏？

它不是模型能力分数，也不是 SWE-bench 分数。每次提交都应运行：

```bash
make benchmark
```

预期结果为 `20/20`，其中任务 20 是一个有意触发 `step_limit_reached` 的负向控制。

## 2. Public Agent Benchmark

公开 benchmark 必须使用外部发布、版本固定的任务集和官方 verifier，而不是把固定答案写进任务响应。每次运行至少记录：

- 数据集名称、版本、任务 ID 和任务集 hash；
- 模型、provider、解码参数、最大 token 和重复次数；
- 隔离环境镜像及其 digest；
- agent 生成的 patch，以及 patch 是否能应用；
- 官方测试的原始结果、超时、退出原因和运行时长；
- 每个任务的成功/失败、超预算和基础设施错误；
- 可复现的完整 JSONL 结果，而不是只报告一个百分比。

GopherHarness 的第一优先级是 **SWE-bench Verified**：它与编码 agent 的工作流最匹配，任务来自真实 GitHub issue，结果由仓库测试验证。第二阶段再接入 Terminal-Bench，用于评估更广泛的终端环境操作能力。

运行公开 benchmark 时，必须保留原始任务集和官方评测脚本的版本信息，并把公开 benchmark 的结果与本目录的 runtime regression 分开发布。

参考：

- [SWE-bench 官方仓库](https://github.com/SWE-bench/SWE-bench)
- [SWE-bench Verified 说明](https://openai.com/index/introducing-swe-bench-verified/)
- [Terminal-Bench 官方仓库](https://github.com/harbor-framework/terminal-bench)

## 当前状态

当前仓库已经具备可重复的 runtime regression 基线、任务级 verifier、负向控制、预算统计、fixture 隔离和 evidence 脱敏。公开 benchmark 接入不能复用 `benchmark.json` 的 fake provider；下一步应新增独立的 SWE-bench adapter，消费用户本地下载并固定版本的实例 JSONL，调用真实 provider，并委托官方 harness 执行测试。

