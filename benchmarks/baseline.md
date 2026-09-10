# GopherHarness Runtime Regression Baseline

> 这是一套运行时回归基线，不是 SWE-bench、Terminal-Bench 或其他公开模型能力排名。

公开 benchmark 的分层和接入约束见 [benchmarks/README.md](README.md)。

This is the checked-in baseline contract for the deterministic benchmark suite.
It uses the task-local fake provider responses, so it does not require an API key,
network access, or a live model. Each task receives a fresh copy of
`benchmarks/fixtures/sample`.

## Run

```bash
make benchmark
```

Equivalent command:

```bash
go run ./cmd/gopherharness-eval \
  --offline \
  --benchmark benchmarks/benchmark.json \
  --fixtures benchmarks \
  --workspaces artifacts/go-workspaces \
  --artifact artifacts/go-benchmark.json
```

## Expected baseline

| Metric | Expected |
| --- | ---: |
| Tasks | 20 |
| Passed | 20 |
| Failed | 0 |
| Categories | workspace-read, workspace-search, workspace-write, shell, state, governance, reliability, security, protocol, evidence |
| Maximum task steps | 5 |

The generated JSON artifact contains per-task attempts, tool steps, verifier
status, stop reason, expected-failure handling, category counts, and duration.
Task 20 is an intentional negative control: it passes only when the runtime
stops with `step_limit_reached`. Runtime IDs and timestamps are intentionally
generated at run time and are not committed as source fixtures.
