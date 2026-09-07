# Pico v3 to GopherHarness parity map

This is a behavioral rewrite, not a line-by-line translation.

| Python v3 area | Go implementation | Status |
| --- | --- | --- |
| `cli.py`, `commands/` | `internal/app`, `cmd/gopherharness` | Implemented |
| Textual TUI | `cmd/gopherharness-tui`, ANSI event-oriented UI | Functional; visual design is simpler |
| `core/runtime.py`, `core/engine.py` | `internal/runtime` | Implemented |
| `core/model_output.py` | `internal/protocol` | Implemented, including `arguments` alias |
| `providers/` | `internal/provider` | OpenAI/Anthropic-compatible implemented |
| `tools/` | `internal/tools` | All 16 public tool names implemented |
| Permissions/tool policy | `internal/permission`, tool/runtime gates | Implemented |
| OS sandbox | `internal/sandbox` | Linux Bubblewrap and macOS sandbox-exec, with fail-closed modes |
| Context manager/compact | `internal/contextbuilder` | Section budget and tail compaction implemented |
| Working/durable memory | `internal/memory` | Implemented with lightweight rule retrieval |
| Session/run evidence | `internal/store` | Implemented with Go schema identifiers |
| Checkpoint/resume | runtime checkpoint + session resume | Final checkpoints plus schema/workspace validation implemented |
| Todo/plan mode | `internal/state` | Implemented |
| Worker manager | `internal/worker` | Goroutine workers, cancellation, and consumed inbox messages implemented |
| Skills | `internal/skills` | Project/user discovery and prompt selection implemented |
| Benchmark evaluator | `internal/evaluation`, `cmd/gopherharness-eval` | Implemented |
| Image inspection | `inspect_image`, provider vision interface | Semantic vision routing and separate provider overrides implemented |
| Auto-dream | `internal/memory`, `internal/runtime/dream.go` | Session/time gate, lock, background task, trace, wait, and `/dream` implemented |
| Native streaming TUI | provider SSE clients + protocol-aware renderer | Final-answer token streaming implemented without exposing tool protocol |
| Cross-language protocol tests | `contracts/`, `cmd/gopherharness-contract`, `scripts/` | Python v3 and Go golden comparison implemented |
| Final readiness | `internal/governance` | off/warn/soft/strict modes with verification evidence implemented |
| Evidence redaction | `internal/evidence`, `internal/redact` | Trace, events, and session snapshots recursively redacted |
| Lifecycle | `Agent.Close`, `worker.Manager.Close` | Workers cancel/join and background memory maintenance drains |

GopherHarness now covers the previously identified functional gaps. It is
still not a byte-for-byte clone: the TUI is intentionally simpler than Textual,
the Go evaluator is smaller than Python's full research evaluation suite, and
the dream quality pipeline uses a lighter report format. Describe this as
behavioral v3 parity for the coding-agent loop, not total implementation parity.

## Compatibility policy

The Go project preserves directory names and human-readable JSON/JSONL shapes,
but writes explicit Go schema identifiers. Do not point Python and Go at the
same active session concurrently. Workspace files are shared safely; mutable
session files should be owned by one runtime at a time.
