package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Lundomn/gopherharness/internal/evidence"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/tools"
)

func (a *Agent) approvalAction(ctx context.Context, call model.ToolCall, spec tools.Spec, run string) (permission.Action, error) {
	definition, err := json.Marshal(spec)
	if err != nil {
		return permission.Action{}, err
	}
	sum := sha256.Sum256(definition)
	action := permission.Action{Principal: a.FactScope.User, Session: a.Session.ID, Turn: run, CallID: call.ID, Tool: call.Name, Args: call.Args, DefinitionDigest: hex.EncodeToString(sum[:]), Preconditions: map[string]string{}}
	if action.Principal == "" {
		action.Principal = "local"
	}
	switch call.Name {
	case "write_file", "patch_file":
		p, e := a.Resolve(fmt.Sprint(call.Args["path"]))
		if e != nil {
			return action, e
		}
		real, e := filepath.EvalSymlinks(p)
		if e == nil {
			action.Preconditions["target"] = real
		} else if os.IsNotExist(e) {
			action.Preconditions["target"] = p
		} else {
			return action, e
		}
		data, e := a.Workspace.ReadFile(ctx, fmt.Sprint(call.Args["path"]))
		if os.IsNotExist(e) {
			action.Preconditions["content"] = "absent"
		} else if e != nil {
			return action, e
		} else {
			h := sha256.Sum256(data)
			action.Preconditions["content"] = hex.EncodeToString(h[:])
		}
	}
	return action, nil
}

type replayedToolResult struct{ Result model.ToolResult }

func (*replayedToolResult) Error() string { return "durable tool receipt already exists" }

// Approvals bind one frozen action and claim it durably immediately before
// dispatch. A claim without a receipt is never eligible for automatic replay.
func (a *Agent) authorizeTool(ctx context.Context, recorder *evidence.Recorder, call model.ToolCall, spec tools.Spec) (func(model.ToolResult) model.ToolResult, error) {
	if !spec.Risky {
		return func(r model.ToolResult) model.ToolResult { return r }, nil
	}
	store := permission.OpenApprovals(a.Workspace.Root)
	identity := permission.Action{Principal: a.FactScope.User, Session: a.Session.ID, Turn: recorder.Run.RunID, CallID: call.ID}
	prior, found, err := store.Find(ctx, identity)
	if err != nil {
		return nil, err
	}
	if found && (prior.Status == "claimed" || prior.Status == "unknown") {
		return nil, &PendingApprovalError{ApprovalID: prior.ID, SessionID: a.Session.ID, Reason: "tool outcome unknown; reconcile before retry"}
	}
	if found && prior.Status == "completed" {
		// Receipt recovery does not execute the action; compare the immutable
		// call/schema, not the post-write file contents.
		definition, _ := json.Marshal(spec)
		sum := sha256.Sum256(definition)
		args, _ := json.Marshal(call.Args)
		approvedArgs, _ := json.Marshal(prior.Action.Args)
		if string(args) != string(approvedArgs) || prior.Action.DefinitionDigest != hex.EncodeToString(sum[:]) {
			return nil, permission.ErrApprovalChanged
		}
		var result model.ToolResult
		if err := json.Unmarshal([]byte(prior.Receipt), &result); err != nil {
			return nil, err
		}
		return nil, &replayedToolResult{Result: result}
	}
	if found && (prior.Status == "denied" || prior.Status == "expired") {
		return nil, fmt.Errorf("approval %s", prior.Status)
	}
	if err := a.preflightPath(call); err != nil {
		return nil, err
	}
	if err := a.freezeableCalls([]model.ToolCall{call}); err != nil {
		return nil, err
	}
	action, err := a.approvalAction(ctx, call, spec, recorder.Run.RunID)
	if err != nil {
		return nil, err
	}
	record, err := store.Create(ctx, action, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	if a.AsyncApproval && a.Approval == permission.Ask {
		if record.Status == "pending" {
			return nil, &PendingApprovalError{ApprovalID: record.ID, SessionID: a.Session.ID, Reason: "explicit approve or deny required"}
		}
		if record.Status != "approved" {
			return nil, fmt.Errorf("approval %s", record.Status)
		}
	} else {
		display, _ := a.Tools.Validate(call)
		decision := permission.Check(a.Approval, true, call.Name, display.Args, a.Approver)
		_, err = store.Decide(ctx, record.ID, action.Principal, decision.Allowed)
		if err != nil {
			return nil, err
		}
		if !decision.Allowed {
			return nil, fmt.Errorf("%s", decision.Reason)
		}
	}
	if !a.toolAllowed(call.Name) {
		return nil, fmt.Errorf("tool authorization changed")
	}
	if err = a.planAllows(call); err != nil {
		return nil, err
	}
	if err = a.preflightPath(call); err != nil {
		return nil, err
	}
	current, err := a.approvalAction(ctx, call, spec, recorder.Run.RunID)
	if err != nil {
		return nil, err
	}
	claim, err := store.Claim(ctx, record.ID, action.Principal, current)
	if err != nil {
		if errors.Is(err, permission.ErrApprovalChanged) {
			return nil, &PendingApprovalError{ApprovalID: record.ID, SessionID: a.Session.ID, Reason: err.Error()}
		}
		return nil, err
	}
	return func(result model.ToolResult) model.ToolResult {
		outcome := "completed"
		if result.Status != "ok" && result.ErrorCode != "definition_changed" && result.ErrorCode != "already_invoked" {
			outcome = "unknown"
		}
		receipt, _ := json.Marshal(result)
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if e := store.Finish(finishCtx, claim, outcome, a.Redactor.Text(string(receipt))); e != nil {
			result.Status = "error"
			result.Error = "tool finished but approval receipt could not be persisted; reconcile before retry: " + e.Error()
			result.ErrorCode = "receipt_unknown"
		} else if outcome == "unknown" && a.AsyncApproval {
			result.ErrorCode = "effect_unknown"
		}
		return result
	}, nil
}
