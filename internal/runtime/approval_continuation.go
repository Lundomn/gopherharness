package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/store"
	"github.com/Lundomn/gopherharness/internal/tools"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

// PendingApprovalError ends the active turn without consuming scheduler
// capacity. The durable batch remains the only runnable work for this session.
type PendingApprovalError struct {
	ApprovalID string
	SessionID  string
	Reason     string
}

func (e *PendingApprovalError) Error() string {
	return fmt.Sprintf("session %s paused for approval %s: %s", e.SessionID, e.ApprovalID, e.Reason)
}
func (*PendingApprovalError) PauseConversation() bool { return true }

// Only execution data needed to finish the original batch is persisted. We do
// not serialize a live context, prompt, closure, connection or native signature.
type approvalContinuation struct {
	Active                        bool
	SessionID, Principal, Request string
	Task                          model.TaskState
	Calls                         []model.ToolCall
	Blocked                       []bool
	Next                          int
	Executing                     bool
	PendingID                     string
	Epoch, Revision               uint64
	MessagesDigest                string
	Trace                         tracing.Identity
	MaxSteps, MaxAttempts         int
	Guard                         loopGuard
	Definitions                   map[string]string
	LedgerLength                  int
	Buffered                      []model.Message
	BufferedNext                  int
	BufferedTask                  *model.TaskState
	BufferedGuard                 *loopGuard
}

func (c *approvalContinuation) UnmarshalJSON(data []byte) error {
	type plain approvalContinuation
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return d.Decode((*plain)(c))
}
func continuationFile(root, session string) durable.File[approvalContinuation] {
	// Hash even trusted IDs so a malformed persisted identifier cannot form a path.
	h := sha256.Sum256([]byte(session))
	return durable.File[approvalContinuation]{Path: filepath.Join(root, ".pico", "approvals", "continuations", hex.EncodeToString(h[:])+".json")}
}
func activeApprovalContinuation(root, session string) (bool, error) {
	c, err := continuationFile(root, session).Read(context.Background())
	return c.Active, err
}
func (a *Agent) pendingError(c *approvalContinuation, reason string) error {
	return &PendingApprovalError{ApprovalID: c.PendingID, SessionID: a.Session.ID, Reason: reason}
}
func (a *Agent) ensureNoPendingApproval(ctx context.Context) error {
	c, err := continuationFile(a.Workspace.Root, a.Session.ID).Read(ctx)
	if err != nil {
		return err
	}
	if c.Active {
		return a.pendingError(&c, "resolve the frozen tool batch before starting another turn")
	}
	return nil
}
func messageDigest(messages []model.Message) string {
	data, _ := json.Marshal(messages)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
func definitionDigest(spec tools.Spec) string {
	data, _ := json.Marshal(spec)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
func (a *Agent) saveContinuation(ctx context.Context, c *approvalContinuation) error {
	disk, err := a.Sessions.Load(a.Session.ID)
	if err != nil {
		return err
	}
	if disk.Epoch != a.Session.Epoch || disk.Revision != a.Session.Revision || disk.Fence != a.Session.Fence {
		return store.ErrSessionConflict
	}
	c.Epoch, c.Revision, c.MessagesDigest = disk.Epoch, disk.Revision, messageDigest(disk.Messages)
	c.LedgerLength = len(disk.Messages)
	c.Task.UserRequest = a.Redactor.Text(c.Task.UserRequest)
	c.Task.FinalAnswer = a.Redactor.Text(c.Task.FinalAnswer)
	if c.BufferedTask != nil {
		c.BufferedTask.UserRequest = a.Redactor.Text(c.BufferedTask.UserRequest)
		c.BufferedTask.FinalAnswer = a.Redactor.Text(c.BufferedTask.FinalAnswer)
	}
	c.Request = a.Redactor.Text(c.Request)
	return continuationFile(a.Workspace.Root, a.Session.ID).Update(ctx, func(v *approvalContinuation) error { *v = *c; return nil })
}
func (a *Agent) clearContinuation(ctx context.Context) error {
	return continuationFile(a.Workspace.Root, a.Session.ID).Update(ctx, func(v *approvalContinuation) error { *v = approvalContinuation{}; return nil })
}
func (a *Agent) freezeableCalls(calls []model.ToolCall) error {
	for _, call := range calls {
		original, err := json.Marshal(call.Args)
		if err != nil {
			return err
		}
		safe, err := json.Marshal(a.Redactor.Map(call.Args))
		if err != nil {
			return err
		}
		if !bytes.Equal(original, safe) || a.Redactor.Text(string(original)) != string(original) {
			return fmt.Errorf("tool %s contains secret material; durable approval requires a secret reference instead of raw credentials", call.Name)
		}
	}
	return nil
}

// PendingApprovals exposes frozen arguments to the authenticated host; callers
// must not publish them into a shared chat or an untrusted model prompt.
func (a *Agent) PendingApprovals(ctx context.Context) ([]permission.ApprovalRecord, error) {
	if err := a.mu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer a.mu.Unlock()
	c, err := continuationFile(a.Workspace.Root, a.Session.ID).Read(ctx)
	if err != nil || !c.Active {
		return nil, err
	}
	if c.PendingID != "" {
		r, err := permission.OpenApprovals(a.Workspace.Root).Get(ctx, c.PendingID, a.FactScope.User)
		if err != nil {
			return nil, err
		}
		return []permission.ApprovalRecord{r}, nil
	}
	return permission.OpenApprovals(a.Workspace.Root).Pending(ctx, a.FactScope.User, a.Session.ID)
}

func (a *Agent) ResumeApproval(ctx context.Context, id, principal string, allow bool) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if a.lifecycleCtx != nil {
		stop := context.AfterFunc(a.lifecycleCtx, cancel)
		defer stop()
	}
	if err := a.mu.LockContext(ctx); err != nil {
		return "", err
	}
	defer a.mu.Unlock()
	if a.closed.Load() {
		return "", errors.New("agent is closed")
	}
	if principal == "" {
		principal = "local"
	}
	if principal != a.FactScope.User {
		return "", permission.ErrApprovalIdentity
	}
	release, err := a.Sessions.TurnLease(ctx, a.Session.ID)
	if err != nil {
		return "", err
	}
	defer release()
	c, err := continuationFile(a.Workspace.Root, a.Session.ID).Read(ctx)
	if err != nil {
		return "", err
	}
	if !c.Active || c.Next < 0 || c.Next > len(c.Calls) || len(c.Blocked) != len(c.Calls) {
		return "", errors.New("no resumable approval batch")
	}
	if c.Principal != principal || c.SessionID != a.Session.ID {
		return "", permission.ErrApprovalIdentity
	}
	disk, err := a.Sessions.Load(a.Session.ID)
	if err != nil {
		return "", err
	}
	if disk.Epoch != a.Session.Epoch || disk.Revision != a.Session.Revision || disk.Fence != a.Session.Fence || disk.Epoch < c.Epoch || disk.Revision < c.Revision {
		return "", a.pendingError(&c, "session owner or messages changed; frozen action was not executed")
	}
	if err := a.recoverBufferedResults(ctx, &c, disk); err != nil {
		return "", err
	}
	approvals := permission.OpenApprovals(a.Workspace.Root)
	r, err := approvals.Get(ctx, id, principal)
	if err != nil {
		return "", err
	}
	callIndex := -1
	for i, call := range c.Calls {
		if call.ID == r.Action.CallID {
			callIndex = i
			break
		}
	}
	if r.Action.Session != c.SessionID || r.Action.Turn != c.Task.RunID || callIndex < 0 || callIndex > c.Next || (c.PendingID != "" && c.PendingID != id) || (callIndex < c.Next && r.Status != "completed" && r.Status != "denied" && r.Status != "expired") {
		return "", permission.ErrApprovalIdentity
	}
	c.PendingID = id
	if r.Status == "claimed" || r.Status == "unknown" {
		return "", a.pendingError(&c, "tool outcome is unknown; reconcile externally, never replay automatically")
	}
	if r.Status == "pending" || r.Status == "approved" {
		r, err = approvals.Decide(ctx, id, principal, allow)
		if err != nil && !errors.Is(err, permission.ErrApprovalExpired) {
			return "", err
		}
	}
	catalog := a.Tools.Snapshot()
	for _, call := range c.Calls[c.Next:] {
		expected, known := c.Definitions[call.Name]
		if !known {
			continue
		}
		spec, found := a.Tools.Spec(call.Name)
		if !found || definitionDigest(spec) != expected {
			return "", a.pendingError(&c, "a frozen batch tool definition changed")
		}
	}
	// A restarted workspace has no fresh-read permission. Restore it only for
	// the exact approved file version; any file change fails before dispatch.
	if allow && (r.Action.Tool == "write_file" || r.Action.Tool == "patch_file") && r.Status == "approved" && c.Next < len(c.Calls) {
		spec, ok := a.Tools.Spec(r.Action.Tool)
		if !ok {
			return "", a.pendingError(&c, "tool no longer exists")
		}
		current, e := a.approvalAction(ctx, c.Calls[c.Next], spec, c.Task.RunID)
		if e != nil {
			return "", a.pendingError(&c, e.Error())
		}
		old, _ := json.Marshal(r.Action)
		now, _ := json.Marshal(current)
		if !bytes.Equal(old, now) {
			return "", a.pendingError(&c, "approved file or tool definition changed")
		}
		if current.Preconditions["content"] != "absent" {
			if _, e = a.Workspace.ReadSnapshot(ctx, fmt.Sprint(current.Args["path"])); e != nil {
				return "", e
			}
		}
	}
	if err = a.saveContinuation(ctx, &c); err != nil {
		return "", err
	}
	oldSteps, oldAttempts := a.MaxSteps, a.MaxAttempts
	wasAsync := a.AsyncApproval
	a.AsyncApproval = true
	defer func() { a.AsyncApproval = wasAsync }()
	a.MaxSteps, a.MaxAttempts = min(a.MaxSteps, c.MaxSteps), min(a.MaxAttempts, c.MaxAttempts)
	defer func() { a.MaxSteps, a.MaxAttempts = oldSteps, oldAttempts }()
	return a.runWithContinuation(tools.WithCatalog(tracing.Attach(ctx, c.Trace), catalog), c.Request, false, &c)
}

// Results are journalled before the session ledger. Recovery accepts the exact
// old ledger or the exact journalled suffix, never arbitrary new messages.
func (a *Agent) recoverBufferedResults(ctx context.Context, c *approvalContinuation, disk *model.Session) error {
	if len(c.Buffered) == 0 {
		if messageDigest(disk.Messages) != c.MessagesDigest {
			return a.pendingError(c, "session messages changed")
		}
		return nil
	}
	if c.BufferedTask == nil || c.BufferedGuard == nil || c.BufferedNext != c.Next+len(c.Buffered) || c.BufferedNext > len(c.Calls) || c.LedgerLength > len(disk.Messages) {
		return a.pendingError(c, "invalid result journal")
	}
	for i, m := range c.Buffered {
		if m.Role != "tool" || m.ToolCallID != c.Calls[c.Next+i].ID {
			return a.pendingError(c, "result journal call identity mismatch")
		}
	}
	old := messageDigest(disk.Messages) == c.MessagesDigest
	if !old {
		if len(disk.Messages) != c.LedgerLength+len(c.Buffered) || messageDigest(disk.Messages[:c.LedgerLength]) != c.MessagesDigest || messageDigest(disk.Messages[c.LedgerLength:]) != messageDigest(c.Buffered) {
			return a.pendingError(c, "session differs from journalled tool results")
		}
	}
	a.Session.Messages = append([]model.Message(nil), disk.Messages...)
	if old {
		a.Session.Messages = append(a.Session.Messages, c.Buffered...)
		if err := a.saveSession(); err != nil {
			return err
		}
	}
	c.Next, c.Task, c.Guard = c.BufferedNext, *c.BufferedTask, *c.BufferedGuard
	c.Buffered, c.BufferedTask, c.BufferedGuard = nil, nil, nil
	c.Executing = false
	return a.saveContinuation(ctx, c)
}

// Journal writes after an effect use a short independent deadline so caller
// cancellation cannot skip the durable receipt/continuation boundary.
func persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
