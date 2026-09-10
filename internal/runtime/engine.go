package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/contextbuilder"
	"github.com/Lundomn/gopherharness/internal/evidence"
	"github.com/Lundomn/gopherharness/internal/governance"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/protocol"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/store"
)

func (a *Agent) run(ctx context.Context, request string, persistUser bool) (answer string, runErr error) {
	request = strings.TrimSpace(request)
	if request == "" {
		return "", fmt.Errorf("request must not be empty")
	}
	runStore := store.NewRunStore(a.Workspace.Root)
	recorder := evidence.New(runStore, a.Sessions, a.Session.ID, a.Redactor)
	a.lastRun = runStore.RunID
	now := time.Now().UTC()
	task := &model.TaskState{SchemaVersion: model.ArtifactSchema, RunID: runStore.RunID, TaskID: store.NewID("task"), UserRequest: request, Status: model.StatusRunning, ResumeStatus: a.ResumeStatus, StartedAt: now, UpdatedAt: now}
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
	repetitions := map[string]int{}
	var promptMeta map[string]any
	defer func() {
		if runErr != nil && task.Status == model.StatusRunning {
			task.Status = model.StatusFailed
			task.StopReason = model.StopPersistence
			task.FinalAnswer = runErr.Error()
			task.UpdatedAt = time.Now().UTC()
			_ = a.writeTask(runStore, task)
		}
		_ = a.saveSession()
	}()

	for task.ToolSteps < a.MaxSteps && task.Attempts < a.MaxAttempts {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		task.Attempts++
		task.UpdatedAt = time.Now().UTC()
		_ = a.writeTask(runStore, task)
		if received := a.drainInbox(); received > 0 {
			_ = recorder.Emit("worker_messages_received", map[string]any{"count": received})
			_ = a.saveSession()
		}
		prompt, meta := contextbuilder.Build(contextbuilder.Input{Workspace: a.Workspace.Summary(), Skills: a.Skills.Render(request), Memory: a.Memory.Render(request), History: a.Session.Messages, UserMessage: request, Tools: a.activeToolSpecs(), MaxChars: a.PromptBudget})
		rawMeta, _ := json.Marshal(meta)
		_ = json.Unmarshal(rawMeta, &promptMeta)
		_ = recorder.Emit("model_started", map[string]any{"attempt": task.Attempts, "prompt_chars": len(prompt)})
		var completion provider.Completion
		var err error
		if stream, ok := a.Provider.(provider.StreamClient); ok && a.OnDelta != nil {
			if a.OnModelStart != nil {
				a.OnModelStart()
			}
			completion, err = stream.Stream(ctx, prompt, a.MaxNewTokens, a.OnDelta)
		} else {
			completion, err = a.Provider.Complete(ctx, prompt, a.MaxNewTokens)
		}
		a.modelCalls++
		a.usageInput += completion.Usage.InputTokens
		a.usageOutput += completion.Usage.OutputTokens
		if err != nil {
			return a.finishError(runStore, task, started, promptMeta, model.StopModelError, fmt.Errorf("model: %w", err))
		}
		_ = recorder.Emit("model_completed", map[string]any{"attempt": task.Attempts, "output_chars": len(completion.Text), "input_tokens": completion.Usage.InputTokens, "output_tokens": completion.Usage.OutputTokens})
		parsed := protocol.Parse(completion.Text)
		switch parsed.Kind {
		case protocol.KindRetry:
			retries++
			a.Session.Messages = append(a.Session.Messages, model.Message{Role: "assistant", Content: completion.Text, CreatedAt: time.Now().UTC()}, model.Message{Role: "system", Content: parsed.Text, CreatedAt: time.Now().UTC()})
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
				_ = a.writeTask(runStore, task)
				_ = a.saveSession()
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
			a.writeCheckpoint(runStore, recorder, task, request, "")
			_ = a.writeTask(runStore, task)
			_ = recorder.Emit("run_completed", map[string]any{"stop_reason": task.StopReason})
			a.scheduleDream(recorder)
			return answer, a.writeReport(runStore, task, started, promptMeta)
		case protocol.KindTools:
			a.Session.Messages = append(a.Session.Messages, model.Message{Role: "assistant", Content: completion.Text, CreatedAt: time.Now().UTC()})
			for _, call := range parsed.Tools {
				if task.ToolSteps >= a.MaxSteps {
					break
				}
				sigBytes, _ := json.Marshal(call)
				sig := string(sigBytes)
				repetitions[sig]++
				if repetitions[sig] > 2 {
					notice := "repeated identical tool call blocked"
					a.Session.Messages = append(a.Session.Messages, model.Message{Role: "tool", Content: notice, CreatedAt: time.Now().UTC()})
					_ = recorder.Emit("tool_blocked", map[string]any{"tool_name": call.Name, "reason": notice})
					continue
				}
				result := a.executeTool(ctx, recorder, call)
				task.ToolSteps++
				task.LastTool = call.Name
				task.UpdatedAt = time.Now().UTC()
				_ = a.writeTask(runStore, task)
				content := renderToolResult(result)
				a.Session.Messages = append(a.Session.Messages, model.Message{Role: "tool", Content: content, CreatedAt: time.Now().UTC()})
				if call.Name == "read_file" && result.Status == "ok" {
					a.Memory.ObserveFile(fmt.Sprint(call.Args["path"]), result.Output)
				}
				if len(result.AffectedPaths) > 0 {
					task.ChangedPaths = appendUnique(task.ChangedPaths, result.AffectedPaths...)
					task.VerificationAttempted = false
					task.VerificationSucceeded = false
					a.Memory.AddNote("Changed: "+strings.Join(result.AffectedPaths, ", "), "workspace_change", call.Name)
				}
				if call.Name == "run_shell" && governance.IsVerificationCommand(fmt.Sprint(call.Args["command"])) {
					task.VerificationAttempted = true
					task.VerificationSucceeded = result.Status == "ok" && strings.Contains(result.Output, "exit_code: 0")
					_ = recorder.Emit("verification_completed", map[string]any{"command": call.Args["command"], "succeeded": task.VerificationSucceeded})
				}
			}
			a.writeCheckpoint(runStore, recorder, task, request, "continue from the latest tool results")
			_ = a.saveSession()
		}
	}
	task.Status = model.StatusFailed
	task.StopReason = model.StopStepLimit
	task.FinalAnswer = "Stopped after reaching the configured step limit."
	task.UpdatedAt = time.Now().UTC()
	_ = a.writeTask(runStore, task)
	_ = recorder.Emit("run_completed", map[string]any{"stop_reason": task.StopReason})
	return task.FinalAnswer, a.writeReport(runStore, task, started, promptMeta)
}

func (a *Agent) executeTool(ctx context.Context, recorder *evidence.Recorder, call model.ToolCall) model.ToolResult {
	_ = recorder.Emit("tool_started", map[string]any{"tool_name": call.Name, "args": call.Args})
	if !a.toolAllowed(call.Name) {
		return a.denied(recorder, call, "tool is not allowed by the active profile", "tool_not_allowed")
	}
	spec, ok := a.Tools.Spec(call.Name)
	if !ok {
		return model.ToolResult{Name: call.Name, Status: "error", Error: "unknown tool", ErrorCode: "unknown_tool"}
	}
	if err := a.planAllows(call); err != nil {
		return a.denied(recorder, call, err.Error(), "plan_mode")
	}
	decision := permission.Check(a.Approval, spec.Risky, call.Name, call.Args, a.Approver)
	if !decision.Allowed {
		return a.denied(recorder, call, decision.Reason, decision.SecurityEvent)
	}
	result := a.Tools.Run(ctx, a, call)
	if len(result.Output) > 12000 {
		path, err := recorder.Run.WriteArtifact(call.Name+"-full-output.txt", []byte(result.Output))
		if err == nil {
			result.Metadata["full_output_artifact"] = path
			result.Output = result.Output[:12000] + "\n...[full output written to artifact]"
		}
	}
	_ = recorder.Emit("tool_completed", map[string]any{"tool_name": call.Name, "status": result.Status, "error_code": result.ErrorCode, "affected_paths": result.AffectedPaths, "metadata": result.Metadata})
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

func (a *Agent) writeCheckpoint(runStore *store.RunStore, recorder *evidence.Recorder, task *model.TaskState, goal, nextStep string) {
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchema, ID: store.NewID("ckpt"), RunID: runStore.RunID, Goal: goal, NextStep: nextStep, WorkspaceHash: a.Workspace.Fingerprint(), CreatedAt: time.Now().UTC()}
	task.CheckpointID = checkpoint.ID
	_ = runStore.WriteCheckpoint(checkpoint)
	_ = recorder.Emit("checkpoint_created", map[string]any{"checkpoint_id": checkpoint.ID, "workspace_fingerprint": checkpoint.WorkspaceHash})
}
