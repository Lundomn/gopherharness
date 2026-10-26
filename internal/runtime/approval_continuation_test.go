package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/governance"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/tools"
)

func approvalTestAgent(t *testing.T) (*Agent, *atomic.Int32) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	count := &atomic.Int32{}
	p := &scriptedChat{call: func(ctx context.Context, r provider.ChatRequest) (provider.Completion, error) {
		if err := provider.ValidateConversation(r.Messages); err != nil {
			return provider.Completion{}, err
		}
		if count.Add(1) == 1 {
			return provider.Completion{ToolCalls: []model.ToolCall{
				{ID: "read-before", Name: "read_file", Args: map[string]any{"path": "a.txt"}},
				{ID: "write-frozen", Name: "write_file", Args: map[string]any{"path": "a.txt", "content": "new"}},
				{ID: "read-after", Name: "read_file", Args: map[string]any{"path": "a.txt"}},
			}}, nil
		}
		var ids []string
		for _, m := range r.Messages {
			if m.Role == "tool" {
				ids = append(ids, m.ToolCallID)
			}
		}
		if strings.Join(ids, ",") != "read-before,write-frozen,read-after" {
			return provider.Completion{}, errors.New("original tool correlation lost: " + strings.Join(ids, ","))
		}
		return provider.Completion{Text: "done"}, nil
	}}
	a, err := New(Options{Root: root, Provider: p, Approval: permission.Ask, AsyncApproval: true, HistorySelection: "rule", DisableAutoDream: true, FinalReadiness: governance.Off})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, count
}
func pauseApproval(t *testing.T, a *Agent) *PendingApprovalError {
	t.Helper()
	_, err := a.Ask(context.Background(), "read, update, check")
	var pending *PendingApprovalError
	if !errors.As(err, &pending) || pending.ApprovalID == "" {
		t.Fatalf("expected pending approval, got %v", err)
	}
	return pending
}
func TestAsyncApprovalResumesOriginalBatchAndRun(t *testing.T) {
	a, count := approvalTestAgent(t)
	pending := pauseApproval(t, a)
	run := a.LastRunID()
	if count.Load() != 1 {
		t.Fatal("model called while tool exchange incomplete")
	}
	if data, _ := os.ReadFile(filepath.Join(a.Workspace.Root, "a.txt")); string(data) != "old" {
		t.Fatal("pending write executed")
	}
	if _, err := a.Ask(context.Background(), "a different request"); err == nil {
		t.Fatal("ordinary request passed pending batch")
	}
	if _, err := a.ResumeApproval(context.Background(), pending.ApprovalID, "intruder", true); !errors.Is(err, permission.ErrApprovalIdentity) {
		t.Fatal(err)
	}
	answer, err := a.ResumeApproval(context.Background(), pending.ApprovalID, "local", true)
	if err != nil || answer != "done" {
		t.Fatalf("resume=%q %v", answer, err)
	}
	if a.LastRunID() != run || count.Load() != 2 {
		t.Fatal("resumption created another run or model request")
	}
	if data, _ := os.ReadFile(filepath.Join(a.Workspace.Root, "a.txt")); string(data) != "new" {
		t.Fatal("write not executed")
	}
	if _, err := a.ResumeApproval(context.Background(), pending.ApprovalID, "local", true); err == nil {
		t.Fatal("duplicate resume accepted")
	}
}
func TestAsyncApprovalRestartAndDenial(t *testing.T) {
	a, count := approvalTestAgent(t)
	pending := pauseApproval(t, a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := New(Options{Root: a.Workspace.Root, Resume: a.SessionID(), Provider: a.Provider, Approval: permission.Ask, AsyncApproval: true, HistorySelection: "rule", DisableAutoDream: true, FinalReadiness: governance.Off})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err = b.ResumeApproval(context.Background(), pending.ApprovalID, "local", false); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatal("restart reran initial model")
	}
	if data, _ := os.ReadFile(filepath.Join(a.Workspace.Root, "a.txt")); string(data) != "old" {
		t.Fatal("denied write executed")
	}
	for _, m := range b.Session.Messages {
		if m.ToolCallID == "write-frozen" && !strings.Contains(m.Content, "approval_denied") {
			t.Fatal("missing explicit denial result")
		}
	}
}
func TestAsyncApprovalChangedFileRejectsAndCanDeny(t *testing.T) {
	a, _ := approvalTestAgent(t)
	pending := pauseApproval(t, a)
	if err := os.WriteFile(filepath.Join(a.Workspace.Root, "a.txt"), []byte("external edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ResumeApproval(context.Background(), pending.ApprovalID, "local", true); err == nil {
		t.Fatal("changed precondition accepted")
	}
	if _, err := a.ResumeApproval(context.Background(), pending.ApprovalID, "local", false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(a.Workspace.Root, "a.txt")); string(data) != "external edit" {
		t.Fatal("external edit overwritten")
	}
}
func TestAsyncApprovalClaimWithoutReceiptNeverReplays(t *testing.T) {
	a, _ := approvalTestAgent(t)
	pending := pauseApproval(t, a)
	s := permission.OpenApprovals(a.Workspace.Root)
	r, err := s.Decide(context.Background(), pending.ApprovalID, "local", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(context.Background(), r.ID, "local", r.Action); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ResumeApproval(context.Background(), r.ID, "local", true); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(a.Workspace.Root, "a.txt")); string(data) != "old" {
		t.Fatal("ambiguous claim was replayed")
	}
}
func TestAsyncApprovalCompletedReceiptRecoversWithoutReexecution(t *testing.T) {
	a, _ := approvalTestAgent(t)
	pending := pauseApproval(t, a)
	ctx := context.Background()
	s := permission.OpenApprovals(a.Workspace.Root)
	r, err := s.Decide(ctx, pending.ApprovalID, "local", true)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(ctx, r.ID, "local", r.Action)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate effect+receipt durable but process stopped before session update.
	if err = os.WriteFile(filepath.Join(a.Workspace.Root, "a.txt"), []byte("effect already happened"), 0600); err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(model.ToolResult{Name: "write_file", Status: "ok", Output: "saved", AffectedPaths: []string{"a.txt"}})
	if err = s.Finish(ctx, claim, "completed", string(receipt)); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ResumeApproval(ctx, r.ID, "local", true); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(a.Workspace.Root, "a.txt")); string(data) != "effect already happened" {
		t.Fatal("completed write executed twice")
	}
}

func TestAsyncApprovalRecoversJournalledSessionSuffix(t *testing.T) {
	a, _ := approvalTestAgent(t)
	pending := pauseApproval(t, a)
	ctx := context.Background()
	s := permission.OpenApprovals(a.Workspace.Root)
	r, err := s.Decide(ctx, pending.ApprovalID, "local", true)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(ctx, r.ID, "local", r.Action)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(a.Workspace.Root, "a.txt"), []byte("already written"), 0600); err != nil {
		t.Fatal(err)
	}
	result := model.ToolResult{Name: "write_file", Status: "ok", Output: "saved"}
	raw, _ := json.Marshal(result)
	if err = s.Finish(ctx, claim, "completed", string(raw)); err != nil {
		t.Fatal(err)
	}
	c, err := continuationFile(a.Workspace.Root, a.SessionID()).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	message := model.Message{ID: "journal-result", ToolCallID: "write-frozen", Role: "tool", Content: renderToolResult(result), CreatedAt: time.Now().UTC()}
	task := c.Task
	task.ToolSteps++
	task.Status = model.StatusRunning
	guard := c.Guard
	c.Buffered = []model.Message{message}
	c.BufferedNext = c.Next + 1
	c.BufferedTask = &task
	c.BufferedGuard = &guard
	if err = a.saveContinuation(ctx, &c); err != nil {
		t.Fatal(err)
	}
	a.Session.Messages = append(a.Session.Messages, message)
	if err = a.saveSession(); err != nil {
		t.Fatal(err)
	} // crash before advancing continuation
	if _, err = a.ResumeApproval(ctx, r.ID, "local", true); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(a.Workspace.Root, "a.txt")); string(data) != "already written" {
		t.Fatal("journalled write replayed")
	}
}

func TestRepeatedFailureStopsToolsButSuccessfulRepeatedReadsDoNot(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(map[bool]string{true: "failure", false: "success"}[fail], func(t *testing.T) {
			root := t.TempDir()
			_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("safe"), 0600)
			var modelCalls, effects atomic.Int32
			p := &scriptedChat{call: func(ctx context.Context, r provider.ChatRequest) (provider.Completion, error) {
				n := modelCalls.Add(1)
				if n == 4 {
					return provider.Completion{Text: "finished with available evidence"}, nil
				}
				if fail && n >= 3 && len(r.Tools) != 0 {
					return provider.Completion{}, errors.New("final-only request still advertised tools")
				}
				name, args := "read_file", map[string]any{"path": "a.txt"}
				if fail {
					name, args = "fail_tool", map[string]any{}
				}
				return provider.Completion{ToolCalls: []model.ToolCall{{ID: fmt.Sprintf("call-%d", n), Name: name, Args: args}}}, nil
			}}
			a, err := New(Options{Root: root, Provider: p, HistorySelection: "rule", DisableAutoDream: true, FinalReadiness: governance.Off})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if fail {
				if err = a.Tools.Register(tools.Definition{Spec: tools.Spec{Name: "fail_tool", Schema: `{}`}, Handler: func(context.Context, tools.Host, map[string]any) (string, []string, error) {
					effects.Add(1)
					return "", nil, errors.New("same failure")
				}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = a.Ask(context.Background(), "try the operation"); err != nil {
				t.Fatal(err)
			}
			if fail && effects.Load() != 2 {
				t.Fatalf("failed operation executed %d times", effects.Load())
			}
			if modelCalls.Load() != 4 {
				t.Fatalf("model calls=%d", modelCalls.Load())
			}
		})
	}
}
func TestServicePendingApprovalReleasesCapacityAndPreservesLane(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count atomic.Int32
	p := &scriptedChat{call: func(ctx context.Context, r provider.ChatRequest) (provider.Completion, error) {
		count.Add(1)
		var last string
		for _, m := range r.Messages {
			if m.Role == "user" {
				last = m.Content
			}
		}
		if last == "write" {
			for _, m := range r.Messages {
				if m.ToolCallID == "write" {
					return provider.Completion{Text: "resumed"}, nil
				}
			}
			return provider.Completion{ToolCalls: []model.ToolCall{{ID: "write", Name: "write_file", Args: map[string]any{"path": "new.txt", "content": "new"}}}}, nil
		}
		return provider.Completion{Text: last}, nil
	}}
	s := NewService(Options{Root: t.TempDir(), Provider: p, Approval: permission.Ask, AsyncApproval: true, HistorySelection: "rule", DisableAutoDream: true, FinalReadiness: governance.Off}, 1, 1)
	defer s.Close(context.Background())
	source := scheduler.Source{Channel: "test", ChatID: "group", SenderID: "alice"}
	h, err := s.Scheduler.Submit(ctx, scheduler.Request{Conversation: "a", Text: "write", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Wait(ctx)
	var pending *PendingApprovalError
	if !errors.As(err, &pending) {
		t.Fatal(err)
	}
	queued, err := s.Scheduler.Submit(ctx, scheduler.Request{Conversation: "a", Text: "after", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Scheduler.Submit(ctx, scheduler.Request{Conversation: "b", Text: "other", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := other.Wait(ctx); err != nil || text != "other" {
		t.Fatalf("other conversation blocked: %q %v", text, err)
	}
	short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if _, err = queued.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued turn passed pending batch: %v", err)
	}
	decision, err := s.DecideApproval(ctx, "a", source, pending.ApprovalID, true)
	if err != nil {
		t.Fatal(err)
	}
	if text, err := decision.Wait(ctx); err != nil || text != "resumed" {
		t.Fatalf("resume=%q %v", text, err)
	}
	if text, err := queued.Wait(ctx); err != nil || text != "after" {
		t.Fatalf("queue=%q %v", text, err)
	}
}
