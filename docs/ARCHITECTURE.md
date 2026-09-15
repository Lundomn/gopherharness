# GopherHarness architecture

GopherHarness is organized into three explicit planes:

```text
Control plane
  cmd/gopherharness, cmd/gopherharness-tui
    -> internal/app
    -> internal/runtime.Agent
    -> internal/runtime.run turn state machine
    -> provider / protocol / tools / governance

State plane
  session -> working memory -> checkpoint -> todos / plan / workers

Evidence plane
  runtime event -> evidence.Recorder -> redactor
    -> task_state.json / trace.jsonl / session events / report.json
```

## Request lifecycle

1. `app.Run` resolves flags, `.pico.toml`, environment overrides, and provider.
2. `runtime.New` validates resume checkpoints and builds the workspace,
   session, memory, tool registry, plan, todo ledger, workers, skills, sandbox,
   and redactor.
3. `Agent.Ask` serializes turns for the main session.
4. `run` creates the run directory and writes the first task state before the
   model call.
5. `contextbuilder.Build` assembles stable instructions, tools, skills,
   workspace, memory, history, and the current request.
6. The provider returns JSON or SSE text; `protocol.Parse` classifies the full
   response as tools, final, or retry. The TUI streams only final-answer text.
7. Every tool crosses allowlist, plan-mode, approval, registered handler,
   workspace, and sandbox boundaries before execution.
8. Tool output is appended to the transcript; large output is moved to a run
   artifact. The runtime updates memory, checkpoint, trace, and session events.
9. Final readiness evaluates changed paths, verification evidence, and live
   workers before accepting an answer.
10. Accepted answers write a current checkpoint, promote durable facts, close
    task state and report, and may submit gated background dream consolidation.

## Concurrency model

The main session is protected by a mutex, so two callers cannot mutate one
transcript concurrently. Workers run in goroutines with independent sessions,
bounded steps, cancellation contexts, and consumed inbox channels. Dream runs
in a bounded background goroutine protected by a filesystem lock and a wait
group. `Agent.Close` cancels and joins workers, then waits for memory
maintenance. Shared memory and workspace read tracking have their own locks.

## Persistence contract

```text
.pico/
  sessions/<session_id>.json
  sessions/<session_id>.events.jsonl
  runs/<run_id>/task_state.json
  runs/<run_id>/trace.jsonl
  runs/<run_id>/report.json
  runs/<run_id>/checkpoint.json
  runs/<run_id>/artifacts/*
  memory/working.json
  memory/topics/project.md
  memory/topics/dream.md
  memory/dream-state.json
  todos.json
  plans/active.md
```

Files are written with restricted permissions where they can contain provider
or task data. Trace, session event, and session snapshot persistence passes
through recursive key-, pattern-, and configured-secret redaction.

## Design choices specific to Go

- `context.Context` owns cancellation and shell/model timeouts.
- Interfaces keep providers and the tool host testable without network calls.
- Goroutines and channels provide isolated background workers.
- Concrete Go structs carry the data contracts; validation happens at registry
  and boundary methods.
- The standard library is used throughout, so the runtime builds without
  third-party Go dependencies.

See [request flow](REQUEST_FLOW.md) for the end-to-end call chain and extension
rules.
