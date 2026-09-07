# Learning path

Use the Python v3 project as the behavioral reference and GopherHarness as the main
codebase. The goal is to learn Go engineering, not to imitate Python syntax.

1. Read `internal/model/types.go` to learn the contracts written to disk.
2. Read `internal/runtime/engine.go` for the entire turn state machine.
3. Follow one tool through `protocol.Parse`, runtime governance,
   `tools.Registry`, and its registered handler.
4. Read `workspace.Resolve` and the fresh-read tests to understand the security
   boundary.
5. Compare `contextbuilder.Build` with Python's context manager and explain why
   the current request is never trimmed.
6. Follow `evidence.Recorder` through redaction into session and run JSONL.
7. Read memory, plan, todo, worker, skills, and sandbox as independent state or
   policy modules.
8. Trace `worker.Manager` and auto-dream to compare cancellation, channels,
   wait groups, and lock files as different concurrency tools.
9. Follow provider `Complete`, `Stream`, and `InspectImage` to learn small Go
   interfaces and capability checks.
10. Read `governance.Evaluate` and compare warn, soft, and strict terminal flow.
11. Trace checkpoint creation and workspace-fingerprint validation on resume.
12. Run the race detector and explain which objects are shared across workers.
13. Run `make contract` and inspect how Python v3 acts as a behavioral oracle.
14. Use `gopherharness-eval` to exercise repository fixtures and verifier commands.

Suggested exercises:

- Replace the ANSI TUI with Bubble Tea while preserving the runtime interfaces.
- Extend the golden suite from model-output parsing to normalized trace events.
- Add a bounded provider work queue and benchmark its backpressure behavior.
- Use `go test -fuzz` against protocol parsing and workspace path resolution.
