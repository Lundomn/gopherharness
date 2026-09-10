# GopherHarness

GopherHarness is a Go-native local coding-agent harness, evolved from the Pico
v3 behavioral reference. A model is only one component inside a governed runtime that
also owns tools, context, memory, permissions, persistence, recovery evidence,
workers, and evaluation.

This repository is intentionally separate from `/Users/ljy/Documents/pico`.
The Python v3 checkout remains a behavioral reference; GopherHarness is an
independent Go implementation.

## What works

- OpenAI-compatible Responses and Anthropic-compatible Messages providers.
- The Pico text protocol: `<tool>...</tool>` and `<final>...</final>`.
- A turn state machine with retry, step, repetition, and cancellation bounds.
- Sixteen tools: files, search, shell, semantic image inspection, todos, plan mode,
  user questions, and worker lifecycle operations.
- Workspace and symlink escape protection, fresh-read-before-write policy,
  approval modes, destructive-shell checks, and optional Bubblewrap/Linux or
  sandbox-exec/macOS isolation.
- Prompt sections and character budgets that preserve the current request.
- Working memory, file summaries, relevant-note recall, durable fact promotion,
  and gated background dream consolidation under `.pico/memory/`.
- Checkpoints, sessions, event JSONL, run traces, reports, and long-output
  artifacts under `.pico/`.
- Isolated goroutine workers with cancellation and live message queues.
- Project and user Skills discovery from `SKILL.md` files.
- Plain REPL, protocol-aware streaming ANSI terminal UI, and benchmark runner.
- A Python-v3/Go golden contract suite for the model-output protocol.
- Final-answer readiness governance, recursive evidence redaction, validated
  checkpoint resume, and deterministic worker shutdown.

## Requirements

- Go 1.27 or later.
- A DeepSeek, OpenAI-compatible, or Anthropic-compatible provider key for live
  model calls.
- Bubblewrap (`bwrap`) on Linux when sandbox mode is required. macOS uses the
  built-in `sandbox-exec` backend when available.

## Build

```bash
cd /Users/ljy/Documents/pico-go
make build
```

The resulting executables are:

```text
bin/gopherharness
bin/gopherharness-tui
bin/gopherharness-eval
bin/gopherharness-contract
```

## Configure

Copy `.pico.toml.example` to `.pico.toml` and fill only the provider you use.
The local `.pico.toml` is ignored by Git.

```toml
provider = "deepseek"

[providers.deepseek]
protocol = "anthropic"
api_key = "your-key"
base_url = "https://api.deepseek.com/anthropic"
model = "deepseek-v4-pro"
```

Configuration priority is:

```text
CLI flags > environment variables > project .pico.toml > defaults
```

## Run

```bash
# Interactive terminal UI
./bin/gopherharness-tui --cwd /path/to/repository

# Plain REPL
./bin/gopherharness --repl --cwd /path/to/repository

# One-shot task
./bin/gopherharness --cwd /path/to/repository \
  "inspect the failing tests and propose a fix"

# Resume the latest session
./bin/gopherharness --cwd /path/to/repository --resume latest
```

Useful commands in the REPL/TUI:

```text
/help /session /memory /dream /skills /todo /workers /usage /reset /exit
```

## Safety

Risky tools use `--approval ask|auto|never`. Existing files must be read before
they can be patched or overwritten. All tool paths are resolved against the
workspace, including symlink targets. The shell policy blocks known destructive
patterns before execution.

Sandbox modes are:

```bash
--sandbox off
--sandbox best_effort
--sandbox required --sandbox-backend auto
```

`required` fails closed when the requested sandbox backend is unavailable.
`auto` selects Bubblewrap on Linux and sandbox-exec on macOS. Use
`workspace_write = false` for a read-only workspace sandbox.

Image inspection uses the main provider by default. A separate vision-capable
profile can be selected without changing the main model:

```bash
./bin/gopherharness-tui --vision-provider openai --vision-model gpt-5.4
```

Automatic dream consolidation starts after five sessions and no more than once
per 24 hours by default. Adjust it with `--dream-min-sessions` and
`--dream-interval`, disable it with `--no-auto-dream`, or run `/dream` manually.

Use `--final-readiness off|warn|soft|strict` to control whether unverified
workspace changes are recorded, reminded once, or blocked before final answer.

## Evaluation

The benchmark schema follows the Python project's task shape: fixture repo,
prompt, allowed tools, step budget, expected artifact, and verifier command.

```bash
./bin/gopherharness-eval \
  --benchmark benchmarks/benchmark.json \
  --fixtures benchmarks \
  --artifact artifacts/go-benchmark.json
```

The repository includes 20 deterministic offline tasks covering workspace I/O,
Shell, state, plan governance, protocol recovery, tool allowlists, path safety,
fresh-read enforcement, timeout handling, large-output artifacts, memory and
checkpoint persistence, redaction, and an intentional step-limit negative
control. Run the baseline without a provider key:

```bash
make benchmark
```

See [benchmark baseline](benchmarks/baseline.md) for the expected 20/20 result.

## Verification

```bash
go vet ./...
go test -race ./...
make contract
make build
```

See [request flow](docs/REQUEST_FLOW.md), [architecture](docs/ARCHITECTURE.md),
[parity map](docs/PARITY.md), and the [learning path](docs/LEARNING.md).
