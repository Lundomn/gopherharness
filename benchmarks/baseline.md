# GopherHarness benchmark baseline

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
| Tasks | 12 |
| Passed | 12 |
| Failed | 0 |
| Categories | workspace-read, workspace-search, workspace-write, shell, state, governance, reliability, security, protocol |
| Maximum task steps | 5 |

The generated JSON artifact contains per-task attempts, tool steps, verifier
status, and duration. Runtime IDs and timestamps are intentionally generated at
run time and are not committed as source fixtures.
