package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/evidence"
	"github.com/Lundomn/gopherharness/internal/governance"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/store"
	"github.com/Lundomn/gopherharness/internal/tools"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

// A batch consists of bounded read groups separated by write barriers. Each
// group is joined and persisted before dispatching the next barrier. A pending
// approval never becomes a fake tool result and never reaches the model.
func (a *Agent) executeToolBatch(ctx context.Context, recorder *evidence.Recorder, tracer *tracing.Tracer, task *model.TaskState, request string, calls []model.ToolCall, blocked []bool, guard *loopGuard, c *approvalContinuation) error {
	durableBatch := a.AsyncApproval || c != nil
	if c == nil {
		c = &approvalContinuation{Active: true, SessionID: a.Session.ID, Principal: a.FactScope.User, Request: request, Task: *task, Calls: calls, Blocked: blocked, Trace: tracing.Current(ctx), MaxSteps: a.MaxSteps, MaxAttempts: a.MaxAttempts}
		c.Guard = *guard
		catalog := tools.CatalogFromContext(ctx)
		if catalog == nil {
			catalog = a.Tools.Snapshot()
		}
		c.Definitions = map[string]string{}
		for _, spec := range catalog.Specs() {
			c.Definitions[spec.Name] = definitionDigest(spec)
		}
		if durableBatch {
			if err := a.saveContinuation(ctx, c); err != nil {
				return err
			}
		}
	}
	for c.Next < len(calls) {
		if err := ctx.Err(); err != nil {
			return err
		}
		start, end := c.Next, c.Next+1
		capability := a.Tools.Capability(calls[start].Name)
		safeRead := capability.Effect == tools.Read && capability.ConcurrencySafe
		if c.Executing && !safeRead {
			spec, _ := a.Tools.Spec(calls[start].Name)
			if !spec.Risky {
				return a.pendingError(c, "interrupted non-read tool has no durable receipt; reconcile before retry")
			}
		}
		if safeRead {
			for end < len(calls) && end-start < 4 {
				cap := a.Tools.Capability(calls[end].Name)
				if cap.Effect != tools.Read || !cap.ConcurrencySafe {
					break
				}
				end++
			}
		}
		c.Executing = true
		if durableBatch {
			if err := a.saveContinuation(ctx, c); err != nil {
				return err
			}
		}
		results := a.Tools.RunIndexedBatch(ctx, calls[start:end], 4, func(toolCtx context.Context, index int, call model.ToolCall) model.ToolResult {
			if blocked[start+index] || guard.FinalOnly {
				return model.ToolResult{Name: call.Name, Status: "error", Error: "tool step budget exhausted; do not retry", ErrorCode: "step_limit"}
			}
			toolCtx, endTool := tracer.Start(toolCtx, "tool."+call.Name, false)
			result := a.executeTool(toolCtx, recorder, call)
			var toolErr error
			if result.Status != "ok" {
				toolErr = fmt.Errorf("%s", result.Error)
			}
			if toolCtx.Err() != nil && result.Status != "pending" {
				toolErr = toolCtx.Err()
				result.Status = "error"
				result.Error = toolErr.Error()
				result.ErrorCode = "cancelled"
			}
			endTool(toolErr)
			return result
		})
		ledgerStart := len(a.Session.Messages)
		for i, result := range results {
			if result.Status == "pending" {
				c.PendingID, _ = result.Metadata["approval_id"].(string)
				c.Executing = false
				task.Status = "waiting_approval"
				task.UpdatedAt = time.Now().UTC()
				c.Task = *task
				c.Guard = *guard
				persist, cancel := persistenceContext(ctx)
				err := a.saveContinuation(persist, c)
				cancel()
				if err != nil {
					return err
				}
				if err := a.writeTask(recorder.Run, task); err != nil {
					return err
				}
				return a.pendingError(c, result.Error)
			}
			if result.ErrorCode == "receipt_unknown" || result.ErrorCode == "effect_unknown" {
				task.Status = "waiting_approval"
				return a.pendingError(c, "tool receipt persistence failed; reconcile before retry")
			}
			call := calls[start+i]
			guard.observe(call, result)
			a.acceptToolResult(recorder, task, call, result, blocked[start+i] || result.ErrorCode == "step_limit")
		}
		if durableBatch {
			c.Buffered = append([]model.Message(nil), a.Session.Messages[ledgerStart:]...)
			for i := range c.Buffered {
				c.Buffered[i].Content = a.Redactor.Text(c.Buffered[i].Content)
			}
			taskSnapshot := *task
			guardSnapshot := *guard
			c.BufferedTask, c.BufferedGuard, c.BufferedNext = &taskSnapshot, &guardSnapshot, end
			persist, cancel := persistenceContext(ctx)
			err := a.saveContinuation(persist, c)
			cancel()
			if err != nil {
				return err
			}
		}
		// Save all returned IDs together. A failure stops the batch before any
		// subsequent write/model; a claimed tool's receipt prevents blind replay.
		if err := a.saveSession(); err != nil {
			return err
		}
		if err := a.writeCheckpoint(recorder.Run, recorder, task, request, "continue from the latest tool results"); err != nil {
			return err
		}
		if err := a.writeTask(recorder.Run, task); err != nil {
			return err
		}
		c.Next = end
		c.Executing = false
		c.Buffered, c.BufferedTask, c.BufferedGuard = nil, nil, nil
		c.Task = *task
		c.Guard = *guard
		if durableBatch {
			persist, cancel := persistenceContext(ctx)
			err := a.saveContinuation(persist, c)
			cancel()
			if err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if durableBatch {
		return a.clearContinuation(ctx)
	}
	return nil
}

func (a *Agent) acceptToolResult(recorder *evidence.Recorder, task *model.TaskState, call model.ToolCall, result model.ToolResult, blocked bool) {
	if !blocked {
		task.ToolSteps++
	}
	task.LastTool = call.Name
	task.UpdatedAt = time.Now().UTC()
	a.Session.Messages = append(a.Session.Messages, model.Message{ID: store.NewID("message"), ToolCallID: call.ID, Role: "tool", Content: renderToolResult(result), CreatedAt: time.Now().UTC()})
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
		task.VerificationSucceeded = result.Status == "ok" && tools.SuccessfulShellExit(result.Metadata)
		_ = recorder.Emit("verification_completed", map[string]any{"command": call.Args["command"], "succeeded": task.VerificationSucceeded})
	}
}
