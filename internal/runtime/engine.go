package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/evidence"
	"github.com/Lundomn/gopherharness/internal/governance"
	"github.com/Lundomn/gopherharness/internal/memory"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/protocol"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/store"
	"github.com/Lundomn/gopherharness/internal/tools"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

func (a *Agent) run(ctx context.Context, request string, persistUser bool) (answer string, runErr error) {
	return a.runWithContinuation(ctx, request, persistUser, nil)
}
func (a *Agent) runWithContinuation(ctx context.Context, request string, persistUser bool, continuation *approvalContinuation) (answer string, runErr error) {
	request = strings.TrimSpace(request)
	if request == "" {
		return "", fmt.Errorf("request must not be empty")
	}
	runStore := store.NewRunStore(a.Workspace.Root)
	if id, ok := ctx.Value(workerSummaryRunKey{}).(string); ok && id != "" {
		runStore = &store.RunStore{Root: a.Workspace.Root, RunID: id, Dir: filepath.Join(a.Workspace.Root, ".pico", "runs", id)}
	}
	if continuation != nil {
		runStore = &store.RunStore{Root: a.Workspace.Root, RunID: continuation.Task.RunID, Dir: filepath.Join(a.Workspace.Root, ".pico", "runs", continuation.Task.RunID)}
	}
	recorder := evidence.New(runStore, a.Sessions, a.Session.ID, a.Redactor)
	tracer := tracing.New(func(record tracing.Record) error {
		payload, err := json.Marshal(record)
		if err != nil {
			return err
		}
		var fields map[string]any
		if err = json.Unmarshal(payload, &fields); err != nil {
			return err
		}
		return recorder.Emit("span_completed", fields)
	})
	ctx, endTurn := tracer.Start(ctx, "session.turn", tracing.Current(ctx).TraceID == "")
	defer func() { endTurn(runErr); a.traceLost.Add(tracer.Lost()) }()
	a.lastRun = runStore.RunID
	now := time.Now().UTC()
	task := &model.TaskState{SchemaVersion: model.ArtifactSchema, RunID: runStore.RunID, TaskID: store.NewID("task"), UserRequest: request, Status: model.StatusRunning, ResumeStatus: a.ResumeStatus, StartedAt: now, UpdatedAt: now}
	if continuation != nil {
		restored := continuation.Task
		task = &restored
		task.Status = model.StatusRunning
		now = task.StartedAt
	}
	if persistUser {
		a.Session.Messages = append(a.Session.Messages, model.Message{Role: "user", Content: request, CreatedAt: now})
	}
	a.Memory.SetGoal(request)
	if err := a.saveSession(); err != nil {
		return "", err
	}
	if err := a.writeTask(runStore, task); err != nil {
		return "", err
	}
	_ = recorder.Emit("run_started", map[string]any{"task_id": task.TaskID, "request": request})
	started := now
	retries := 0
	var promptMeta map[string]any
	guard := &loopGuard{}
	if continuation != nil {
		*guard = continuation.Guard
	}
	defer func() {
		if runErr != nil && task.Status == model.StatusRunning {
			task.Status = model.StatusFailed
			task.StopReason = model.StopPersistence
			if ctx.Err() != nil {
				task.StopReason = "cancelled"
				if ctx.Err() == context.DeadlineExceeded {
					task.StopReason = "timed_out"
				}
			}
			task.FinalAnswer = runErr.Error()
			task.UpdatedAt = time.Now().UTC()
			_ = a.writeTask(runStore, task)
			_ = a.writeReport(runStore, task, started, promptMeta)
		}
	}()
	if continuation != nil {
		if err := a.executeToolBatch(ctx, recorder, tracer, task, request, continuation.Calls, continuation.Blocked, guard, continuation); err != nil {
			return "", err
		}
	}
	if _, err := a.ingestWorkerResults(ctx); err != nil {
		return "", err
	}
	turnPrompt, buildErr := a.prepareTurnContext(ctx, request)
	if buildErr != nil {
		return a.finishError(runStore, task, started, promptMeta, "context_build_failed", buildErr)
	}
	promptMeta = turnPrompt.meta
	overflowRetries := 0

turnLoop:
	for task.Attempts < a.MaxAttempts {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if task.ToolSteps >= a.MaxSteps {
			guard.FinalOnly = true
			guard.Reason = "tool step budget exhausted"
		}
		if guard.FinalOnly && guard.FinalCalls >= 2 {
			break turnLoop
		}
		turnPrompt.finalOnly = guard.FinalOnly
		if guard.FinalOnly {
			guard.FinalCalls++
		}
		task.Attempts++
		task.UpdatedAt = time.Now().UTC()
		if err := a.writeTask(runStore, task); err != nil {
			return "", err
		}
		if received := a.drainInbox(); received > 0 {
			_ = recorder.Emit("worker_messages_received", map[string]any{"count": received})
			if err := a.saveSession(); err != nil {
				return "", err
			}
		}
		if _, err := a.ingestWorkerResults(ctx); err != nil {
			return "", err
		}
		if err := turnPrompt.advance(a); err != nil {
			return a.finishError(runStore, task, started, promptMeta, "context_budget_exceeded", err)
		}
		iterationCtx := tools.WithCatalog(ctx, turnPrompt.catalog)
		modelCtx, endModel := tracer.Start(iterationCtx, "llm.call", false)
		_ = recorder.EmitContext(modelCtx, "model_started", map[string]any{"attempt": task.Attempts, "prompt_tokens": promptMeta["prompt_tokens"], "input_budget_tokens": a.ContextWindowTokens - a.MaxNewTokens})
		var completion provider.Completion
		var err error
		for {
			completion, err = turnPrompt.complete(modelCtx, a)
			a.modelCalls++
			a.usageInput += completion.Usage.InputTokens
			a.usageOutput += completion.Usage.OutputTokens
			if !provider.ContextOverflow(err) || overflowRetries >= 2 || ctx.Err() != nil || completion.Text != "" || !turnPrompt.shrink() {
				break
			}
			overflowRetries++
			_ = recorder.EmitContext(modelCtx, "context_overflow_recovery", map[string]any{"retry": overflowRetries, "tools_reexecuted": false})
			if e := turnPrompt.measure(); e != nil {
				err = e
				break
			}
		}
		endModel(err)
		if err != nil {
			// Bounded wrap-up is optional after a hard stop. A provider failure
			// here cannot replace the original tool-limit/no-progress outcome.
			if guard.FinalOnly && ctx.Err() == nil {
				break turnLoop
			}
			return a.finishError(runStore, task, started, promptMeta, model.StopModelError, fmt.Errorf("model: %w", err))
		}
		_ = recorder.EmitContext(modelCtx, "model_completed", map[string]any{"attempt": task.Attempts, "output_chars": len(completion.Text), "input_tokens": completion.Usage.InputTokens, "output_tokens": completion.Usage.OutputTokens})
		parsed := protocol.Parse(completion.Text)
		if turnPrompt.native {
			kind, text := nativeParsed(completion)
			parsed = protocol.Parsed{Kind: protocol.Kind(kind), Text: text, Tools: completion.ToolCalls}
			if kind == "final" && strings.HasPrefix(text, "<final>") {
				parsed = protocol.Parse(text)
			}
		}
		switch parsed.Kind {
		case protocol.KindRetry:
			retries++
			a.Session.Messages = append(a.Session.Messages, model.Message{Role: "assistant", Content: completion.Text, CreatedAt: time.Now().UTC()}, model.Message{Role: "system", Content: parsed.Text, CreatedAt: time.Now().UTC()})
			if err := a.saveSession(); err != nil {
				return "", err
			}
			_ = recorder.Emit("model_retry", map[string]any{"notice": parsed.Text})
			if retries >= 3 {
				return a.finishError(runStore, task, started, promptMeta, model.StopRetryLimit, fmt.Errorf("model output retry limit reached"))
			}
		case protocol.KindFinal:
			decision := governance.Evaluate(a.FinalReadiness, governance.Evidence{ChangedPaths: task.ChangedPaths, VerificationAttempted: task.VerificationAttempted, VerificationSucceeded: task.VerificationSucceeded, RunningWorkers: a.Workers.RunningIDs(), ReminderAlreadySent: task.ReadinessReminded})
			_ = recorder.Emit("final_readiness_decision", readinessPayload(decision))
			if decision.Action == "runtime_notice" || decision.Action == "block" {
				task.ReadinessReminded = true
				notice := governance.Notice(decision)
				a.Session.Messages = append(a.Session.Messages, model.Message{Role: "system", Content: notice, CreatedAt: time.Now().UTC()})
				_ = recorder.Emit("final_readiness_notice", map[string]any{"action": decision.Action, "notice": notice, "reason_signature": decision.Signature})
				if err := a.writeTask(runStore, task); err != nil {
					return "", err
				}
				if err := a.saveSession(); err != nil {
					return "", err
				}
				continue
			}
			answer = parsed.Text
			task.Status = model.StatusCompleted
			task.StopReason = model.StopFinalAnswer
			task.FinalAnswer = answer
			task.UpdatedAt = time.Now().UTC()
			a.Session.Messages = append(a.Session.Messages, model.Message{Role: "assistant", Content: answer, CreatedAt: time.Now().UTC()})
			if active, _ := a.Plan.Active(); active {
				a.Plan.Exit()
			}
			a.Memory.SetNextStep("")
			_ = a.Memory.Promote(answer)
			if a.FactWriter != nil {
				if factErr := a.FactWriter.Prepare(ctx, runStore.RunID, a.Session.ID, []memory.Evidence{{ID: runStore.RunID + "-request", Kind: "conversation", Text: a.Redactor.Text(request), ObservedAt: now}, {ID: runStore.RunID + "-answer", Kind: "model", Text: a.Redactor.Text(answer), ObservedAt: time.Now().UTC()}}); factErr != nil {
					return a.finishError(runStore, task, started, promptMeta, model.StopPersistence, factErr)
				}
			}
			if err := a.writeCheckpoint(runStore, recorder, task, request, ""); err != nil {
				return a.finishError(runStore, task, started, promptMeta, model.StopPersistence, err)
			}
			if e := a.saveSession(); e != nil {
				return a.finishError(runStore, task, started, promptMeta, model.StopPersistence, e)
			}
			if e := a.writeTask(runStore, task); e != nil {
				return a.finishError(runStore, task, started, promptMeta, model.StopPersistence, e)
			}
			if e := a.writeReport(runStore, task, started, promptMeta); e != nil {
				return "", e
			}
			if a.FactWriter != nil {
				a.FactWriter.Notify()
			}
			_ = recorder.Emit("run_completed", map[string]any{"stop_reason": task.StopReason})
			a.scheduleDream(ctx, recorder)
			return answer, nil
		case protocol.KindTools:
			// The tool budget does not consume the final model response. Allow
			// summarization after the last tool, but never execute another tool.
			calls := make([]model.ToolCall, 0, len(parsed.Tools))
			blocked := []bool{}
			seenIDs := map[string]bool{}
			for _, m := range a.Session.Messages {
				for _, c := range m.ToolCalls {
					seenIDs[c.ID] = true
				}
			}
			for _, call := range parsed.Tools {
				if call.ID == "" {
					call.ID = store.NewID("call")
				}
				if seenIDs[call.ID] {
					return a.finishError(runStore, task, started, promptMeta, "invalid_tool_call", fmt.Errorf("duplicate tool call ID"))
				}
				seenIDs[call.ID] = true
				calls = append(calls, call)
				blocked = append(blocked, guard.FinalOnly || task.ToolSteps+len(calls) > a.MaxSteps)
			}
			if a.AsyncApproval {
				if err := a.freezeableCalls(calls); err != nil {
					return "", err
				}
			}
			a.Session.Messages = append(a.Session.Messages, model.Message{ID: store.NewID("message"), Role: "assistant", Content: completion.Text, ToolCalls: calls, NativeContent: completion.NativeContent, CreatedAt: time.Now().UTC()})
			if err := a.saveSession(); err != nil {
				return a.finishError(runStore, task, started, promptMeta, model.StopPersistence, err)
			}
			if err := a.executeToolBatch(iterationCtx, recorder, tracer, task, request, calls, blocked, guard, nil); err != nil {
				return "", err
			}
		}
	}
	task.Status = model.StatusFailed
	task.StopReason = model.StopStepLimit
	task.FinalAnswer = "Stopped after reaching the configured step limit."
	if task.ToolSteps < a.MaxSteps {
		task.StopReason = model.StopAttemptLimit
		task.FinalAnswer = "Stopped after reaching the configured model-attempt limit."
	}
	if guard.FinalOnly && guard.Reason != "tool step budget exhausted" {
		task.StopReason = "repeated_tool_failure"
		task.FinalAnswer = "Stopped after repeated identical tool failures and bounded final-answer attempts."
	}
	task.UpdatedAt = time.Now().UTC()
	if err := a.writeTask(runStore, task); err != nil {
		return "", err
	}
	_ = recorder.Emit("run_completed", map[string]any{"stop_reason": task.StopReason})
	return task.FinalAnswer, a.writeReport(runStore, task, started, promptMeta)
}

func (a *Agent) executeTool(ctx context.Context, recorder *evidence.Recorder, call model.ToolCall) (result model.ToolResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = model.ToolResult{Name: call.Name, Status: "error", Error: fmt.Sprintf("tool panicked; outcome requires reconciliation: %v", recovered), ErrorCode: "effect_unknown"}
		}
		event := "tool_completed"
		if result.Status == "pending" {
			event = "tool_paused"
		}
		_ = recorder.EmitContext(ctx, event, map[string]any{"tool_name": call.Name, "call_id": call.ID, "status": result.Status, "error_code": result.ErrorCode, "affected_paths": result.AffectedPaths, "metadata": result.Metadata})
	}()
	limit := 300 * time.Second
	if call.Name == "run_shell" {
		limit = 660 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return model.ToolResult{Name: call.Name, Status: "error", Error: err.Error(), ErrorCode: "cancelled"}
	}
	_ = recorder.EmitContext(ctx, "tool_started", map[string]any{"tool_name": call.Name, "args": call.Args})
	if !a.toolAllowed(call.Name) {
		return a.denied(recorder, call, "tool is not allowed by the active profile", "tool_not_allowed")
	}
	invocation, prepareErr := a.Tools.PrepareContext(ctx, call)
	if prepareErr != nil {
		return a.denied(recorder, call, prepareErr.Error(), "invalid_or_changed_tool")
	}
	call, spec := invocation.Call, invocation.Spec
	if err := a.planAllows(call); err != nil {
		return a.denied(recorder, call, err.Error(), "plan_mode")
	}
	var err error
	finishApproval, approvalErr := a.authorizeTool(ctx, recorder, call, spec)
	if approvalErr != nil {
		var replay *replayedToolResult
		if errors.As(approvalErr, &replay) {
			return a.boundToolResult(recorder, replay.Result)
		}
		var pending *PendingApprovalError
		if errors.As(approvalErr, &pending) {
			return model.ToolResult{Name: call.Name, Status: "pending", Error: pending.Reason, ErrorCode: "approval_pending", Metadata: map[string]any{"approval_id": pending.ApprovalID}}
		}
		return a.denied(recorder, call, approvalErr.Error(), "approval_denied")
	}
	if err := ctx.Err(); err != nil {
		return a.denied(recorder, call, err.Error(), "cancelled")
	}
	if err = a.preflightPath(call); err != nil {
		return a.denied(recorder, call, err.Error(), "path_precondition")
	}
	result = finishApproval(invocation.Run(ctx, a))
	return a.boundToolResult(recorder, result)
}
func (a *Agent) boundToolResult(recorder *evidence.Recorder, result model.ToolResult) model.ToolResult {
	runes := []rune(result.Output)
	if len(runes) > 16000 {
		path, err := recorder.Run.WriteArtifact(store.NewID(result.Name)+"-full-output.txt", []byte(a.Redactor.Text(result.Output)))
		if err == nil {
			if result.Metadata == nil {
				result.Metadata = map[string]any{}
			}
			result.Metadata["full_output_artifact"] = path
			result.Output = string(runes[:16000]) + "\n...[full output written to artifact]"
		}
		if err != nil {
			result.Output = string(runes[:16000]) + "\n...[output truncated; full artifact persistence failed]"
		}
	}
	return result
}
func (a *Agent) denied(recorder *evidence.Recorder, call model.ToolCall, reason, code string) model.ToolResult {
	r := model.ToolResult{Name: call.Name, Status: "error", Error: reason, ErrorCode: code}
	_ = recorder.Emit("permission_decision", map[string]any{"tool_name": call.Name, "decision": "deny", "reason": reason, "security_event_type": code})
	return r
}
func (a *Agent) writeReport(runStore *store.RunStore, task *model.TaskState, started time.Time, promptMeta map[string]any) error {
	finalAnswer := task.FinalAnswer
	if a.Redactor != nil {
		finalAnswer = a.Redactor.Text(finalAnswer)
	}
	return runStore.WriteReport(&model.RunReport{SchemaVersion: model.ArtifactSchema, RunID: runStore.RunID, SessionID: a.Session.ID, Status: task.Status, StopReason: task.StopReason, FinalAnswer: finalAnswer, Attempts: task.Attempts, ToolSteps: task.ToolSteps, Provider: a.Provider.Name(), Model: a.Provider.Model(), PromptMeta: promptMeta, StartedAt: started, FinishedAt: time.Now().UTC()})
}

func (a *Agent) writeTask(runStore *store.RunStore, task *model.TaskState) error {
	snapshot := *task
	if a.Redactor != nil {
		snapshot.UserRequest = a.Redactor.Text(snapshot.UserRequest)
		snapshot.FinalAnswer = a.Redactor.Text(snapshot.FinalAnswer)
		if len(snapshot.ChangedPaths) > 0 {
			paths := make([]string, len(snapshot.ChangedPaths))
			for index, path := range snapshot.ChangedPaths {
				paths[index] = a.Redactor.Text(path)
			}
			snapshot.ChangedPaths = paths
		}
	}
	return runStore.WriteTask(&snapshot)
}

func (a *Agent) finishError(runStore *store.RunStore, task *model.TaskState, started time.Time, promptMeta map[string]any, reason string, err error) (string, error) {
	task.Status = model.StatusFailed
	task.StopReason = reason
	task.FinalAnswer = err.Error()
	task.UpdatedAt = time.Now().UTC()
	_ = a.writeTask(runStore, task)
	recorder := evidence.New(runStore, a.Sessions, a.Session.ID, a.Redactor)
	_ = recorder.Emit("run_failed", map[string]any{"stop_reason": reason, "error": err.Error()})
	_ = a.writeReport(runStore, task, started, promptMeta)
	return "", err
}
func renderToolResult(r model.ToolResult) string {
	if r.Status == "ok" {
		return fmt.Sprintf("<tool_result name=%q status=%q>\n%s\n</tool_result>", r.Name, r.Status, r.Output)
	}
	return fmt.Sprintf("<tool_result name=%q status=%q error_code=%q>\n%s\n</tool_result>", r.Name, r.Status, r.ErrorCode, r.Error)
}

func appendUnique(existing []string, values ...string) []string {
	seen := make(map[string]bool, len(existing)+len(values))
	for _, value := range existing {
		seen[value] = true
	}
	for _, value := range values {
		if value != "" && !seen[value] {
			existing = append(existing, value)
			seen[value] = true
		}
	}
	return existing
}

func readinessPayload(decision governance.Decision) map[string]any {
	reasons := make([]any, 0, len(decision.Reasons))
	for _, reason := range decision.Reasons {
		reasons = append(reasons, map[string]any{"code": reason.Code, "severity": reason.Severity, "message": reason.Message})
	}
	return map[string]any{"mode": decision.Mode, "decision": decision.Decision, "action": decision.Action, "reasons": reasons, "reason_signature": decision.Signature}
}

func (a *Agent) writeCheckpoint(runStore *store.RunStore, recorder *evidence.Recorder, task *model.TaskState, goal, nextStep string) error {
	fingerprint, err := a.Workspace.FingerprintChecked()
	if err != nil {
		return err
	}
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchema, ID: store.NewID("ckpt"), RunID: runStore.RunID, Goal: a.Redactor.Text(goal), NextStep: a.Redactor.Text(nextStep), WorkspaceHash: fingerprint, CreatedAt: time.Now().UTC()}
	checkpoint.SessionID = a.Session.ID
	if err := runStore.WriteCheckpoint(checkpoint); err != nil {
		return err
	}
	task.CheckpointID = checkpoint.ID
	_ = recorder.Emit("checkpoint_created", map[string]any{"checkpoint_id": checkpoint.ID, "workspace_fingerprint": checkpoint.WorkspaceHash})
	return nil
}
