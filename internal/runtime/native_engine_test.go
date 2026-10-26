package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Lundomn/gopherharness/internal/memory"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/tools"
)

type scriptedChat struct {
	call func(context.Context, provider.ChatRequest) (provider.Completion, error)
}

func (c *scriptedChat) Name() string      { return "test-native" }
func (c *scriptedChat) Model() string     { return "test-native-model" }
func (c *scriptedChat) NativeTools() bool { return true }
func (c *scriptedChat) Complete(context.Context, string, int) (provider.Completion, error) {
	return provider.Completion{}, errors.New("unexpected legacy completion")
}
func (c *scriptedChat) Chat(ctx context.Context, r provider.ChatRequest) (provider.Completion, error) {
	return c.call(ctx, r)
}

type recallCounter struct{ recalls atomic.Int32 }

func (b *recallCounter) Recall(context.Context, memory.Scope, string, int) ([]memory.Fact, error) {
	b.recalls.Add(1)
	return nil, nil
}
func (b *recallCounter) Store(context.Context, memory.Scope, string, []memory.Evidence) error {
	return nil
}
func (b *recallCounter) Close() error { return nil }
func TestNativeIterationPreservesBatchThenInjectionAndRecallsOnce(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0600)
	facts := &recallCounter{}
	calls := 0
	var inject bool
	c := &scriptedChat{call: func(ctx context.Context, r provider.ChatRequest) (provider.Completion, error) {
		calls++
		if err := provider.ValidateConversation(r.Messages); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			if r.Messages[0].Role != "system" || strings.Contains(r.Messages[0].Content, "<tool>") {
				t.Fatal("bad native prefix")
			}
			inject = true
			return provider.Completion{ToolCalls: []model.ToolCall{{ID: "read-1", Name: "read_file", Args: map[string]any{"path": "a.txt"}}, {ID: "list-2", Name: "list_files", Args: map[string]any{"path": "."}}}}, nil
		}
		if len(r.Messages) != 6 {
			t.Fatalf("messages=%+v", r.Messages)
		}
		for i, role := range []string{"system", "user", "assistant", "tool", "tool", "user"} {
			if r.Messages[i].Role != role {
				t.Fatalf("order=%+v", r.Messages)
			}
		}
		if r.Messages[3].ToolCallID != "read-1" || r.Messages[4].ToolCallID != "list-2" || !strings.Contains(r.Messages[5].Content, "also explain") {
			t.Fatalf("lost correlation: %+v", r.Messages)
		}
		return provider.Completion{Text: "done"}, nil
	}}
	a, e := New(Options{Root: root, Provider: c, FactBackend: facts, HistorySelection: "rule", DisableAutoDream: true})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	a.DrainInjected = func() []string {
		if inject {
			inject = false
			return []string{"also explain"}
		}
		return nil
	}
	answer, e := a.Ask(context.Background(), "read and list")
	if e != nil || answer != "done" || calls != 2 || facts.recalls.Load() != 1 {
		t.Fatalf("%s %v calls=%d recall=%d", answer, e, calls, facts.recalls.Load())
	}
}
func TestNativeOverflowShrinksOldOutputsWithoutRerunningTools(t *testing.T) {
	calls, toolsRun := 0, 0
	c := &scriptedChat{call: func(ctx context.Context, r provider.ChatRequest) (provider.Completion, error) {
		calls++
		if calls == 1 {
			var cs []model.ToolCall
			for _, id := range []string{"c1", "c2", "c3", "c4"} {
				cs = append(cs, model.ToolCall{ID: id, Name: "probe", Args: map[string]any{}})
			}
			return provider.Completion{ToolCalls: cs}, nil
		}
		if calls == 2 {
			return provider.Completion{}, &provider.HTTPError{Status: 400, Detail: "context_length_exceeded"}
		}
		old, latest := 0, 0
		for _, m := range r.Messages {
			if m.Role == "tool" {
				if strings.Contains(m.Content, "omitted") {
					old++
				} else {
					latest++
				}
			}
		}
		if old != 1 || latest != 3 {
			t.Fatalf("wrong shrink: %+v", r.Messages)
		}
		return provider.Completion{Text: "done"}, nil
	}}
	a, e := New(Options{Root: t.TempDir(), Provider: c, HistorySelection: "rule", DisableAutoDream: true})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	e = a.Tools.Register(tools.Definition{Spec: tools.Spec{Name: "probe", Schema: `{}`}, Handler: func(context.Context, tools.Host, map[string]any) (string, []string, error) {
		toolsRun++
		return strings.Repeat("log ", 200), nil, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	answer, e := a.Ask(context.Background(), "probe")
	if e != nil || answer != "done" || calls != 3 || toolsRun != 4 {
		t.Fatalf("%s %v calls=%d tools=%d", answer, e, calls, toolsRun)
	}
	preserved := 0
	for _, m := range a.Session.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "log ") {
			preserved++
		}
	}
	if preserved != 4 {
		t.Fatal("request compaction mutated source ledger")
	}
}

type mutatingApprover struct {
	root  string
	calls int
}

func (p *mutatingApprover) Approve(_ string, args map[string]any) bool {
	p.calls++
	args["content"] = "hijack"
	return true
}
func TestApprovalFreezesValidatedActionAndPersistsReceipt(t *testing.T) {
	root := t.TempDir()
	approver := &mutatingApprover{root: root}
	fake := &provider.Fake{Responses: []string{`<tool>{"name":"write_file","args":{"path":"x","content":"intended"}}</tool>`, `<final>done</final>`}}
	a, e := New(Options{Root: root, Provider: fake, Approver: approver, Approval: permission.Ask, DisableAutoDream: true, HistorySelection: "rule"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if _, e = a.Ask(context.Background(), "write"); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(root, "x"))
	if string(b) != "intended" {
		t.Fatalf("approved action mutated: %s", b)
	}
	raw, e := os.ReadFile(filepath.Join(root, ".pico", "approvals", "requests.json"))
	if e != nil {
		t.Fatal(e)
	}
	// File is an envelope; receipt is still visible as an escaped payload.
	if !strings.Contains(string(raw), "completed") {
		t.Fatalf("missing durable outcome: %s", raw)
	}
}
func TestRecoveryClosesMissingReceiptsWithoutExecuting(t *testing.T) {
	source := []model.Message{{Role: "user", Content: "change"}, {Role: "assistant", ToolCalls: []model.ToolCall{{ID: "a", Name: "write_file"}, {ID: "b", Name: "run_shell"}}}, {Role: "tool", ToolCallID: "a", Content: "done"}}
	repaired, n, e := closeInterruptedExchanges(source)
	if e != nil || n != 1 || len(repaired) != 4 || !strings.Contains(repaired[3].Content, "unknown") {
		t.Fatalf("%+v %d %v", repaired, n, e)
	}
	if e = provider.ValidateConversation(repaired); e != nil {
		t.Fatal(e)
	}
	again, n, e := closeInterruptedExchanges(repaired)
	if e != nil || n != 0 {
		t.Fatal(n, e)
	}
	x, _ := json.Marshal(repaired)
	y, _ := json.Marshal(again)
	if string(x) != string(y) {
		t.Fatal("recovery not idempotent")
	}
}

func TestSignedNativeBlockWithSecretIsNotPersistedOrExecuted(t *testing.T) {
	root := t.TempDir()
	c := &scriptedChat{call: func(context.Context, provider.ChatRequest) (provider.Completion, error) {
		return provider.Completion{NativeContent: []model.NativeItem{{Protocol: "anthropic", Data: json.RawMessage(`{"type":"thinking","thinking":"example-sensitive-key","signature":"unchanged"}`)}}, ToolCalls: []model.ToolCall{{ID: "c", Name: "write_file", Args: map[string]any{"path": "x", "content": "never"}}}}, nil
	}}
	a, e := New(Options{Root: root, Provider: c, Secrets: []string{"example-sensitive-key"}, Approval: permission.Auto, DisableAutoDream: true, HistorySelection: "rule"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if _, e = a.Ask(context.Background(), "write"); e == nil {
		t.Fatal("sensitive signature block accepted")
	}
	if _, e = os.Stat(filepath.Join(root, "x")); !os.IsNotExist(e) {
		t.Fatal("tool executed before safe persistence")
	}
	filepath.WalkDir(filepath.Join(root, ".pico"), func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "example-sensitive-key") {
				t.Errorf("secret leaked: %s", p)
			}
		}
		return nil
	})
}
