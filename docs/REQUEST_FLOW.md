# Request flow

This document is the shortest path through GopherHarness. Each arrow is an explicit
package boundary; persistence and policy do not happen implicitly inside a
provider or tool.

```text
cmd/gopherharness or cmd/gopherharness-tui
        |
        v
internal/app.Run
  flags -> config -> provider capabilities -> runtime.Options
        |
        v
runtime.New
  workspace + session + memory + tools + workers + sandbox + redactor
        |
        v
Agent.Ask(ctx, request)
  serialize the main session and create RunStore + evidence.Recorder
        |
        v
contextbuilder.Build -> provider.Complete/Stream -> protocol.Parse
        |                                      |
        | tools                                | final/retry
        v                                      v
allowlist -> plan -> approval -> handler    governance.Evaluate
        |                                      |
        v                                      v
workspace/sandbox -> ToolResult            remind/block/allow
        |                                      |
        +----------> session + checkpoint <---+
                         |
                         v
             redacted trace/report/session
                         |
                         v
                  optional auto-dream
```

## One tool call

1. `protocol.Parse` creates `model.ToolCall`.
2. `runtime.executeTool` checks the active allowlist.
3. Plan mode limits writes, then `permission.Check` applies user policy.
4. `tools.Registry` looks up the registered `Spec + Handler` pair.
5. The handler calls the narrow `tools.Host` interface; it cannot reach runtime
   internals directly.
6. Paths pass through `workspace.Resolve`; shell commands pass through
   `sandbox.Run`.
7. `model.ToolResult` returns to the transcript, while `evidence.Recorder`
   writes a recursively redacted event.
8. Changed paths reset verification state. Recognized test/build/lint commands
   update verification evidence.

## One final answer

1. `protocol.Parse` proposes final text.
2. `governance.Evaluate` checks changed paths, verification, and live workers.
3. `off`, `warn`, `soft`, and `strict` decide whether to allow, remind, or
   block the proposal.
4. An accepted final writes a fresh checkpoint with the current workspace
   fingerprint, closes plan mode, promotes durable memory, and writes the run
   report.
5. Auto-dream may be submitted after its session and interval gates pass.

## Resume flow

`runtime.New --resume` loads the session and its latest completed run
checkpoint. It compares the checkpoint schema and workspace fingerprint and
records one of:

```text
no-checkpoint
full-valid
schema-mismatch
workspace-mismatch
```

Only a `full-valid` checkpoint restores its goal and next step into working
memory. The status is persisted in the next `task_state.json`.

## Extension rules

- New model protocol: implement `provider.Client`; add `StreamClient` or
  `VisionClient` only when supported.
- New tool: register one `tools.Definition` containing its public `Spec` and
  `Handler`; keep OS and workspace access behind `tools.Host`.
- New evidence: emit through `evidence.Recorder`, never write trace JSONL from
  feature code.
- New terminal policy: add evidence and decisions in `internal/governance`, not
  in the provider or TUI.
- New background work: own a cancellation path and include it in `Agent.Close`.
