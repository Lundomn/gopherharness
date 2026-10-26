package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	gostdruntime "runtime"
	"strings"
	"testing"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/governance"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/store"
)

func TestAgentRunsToolLoopAndWritesEvidence(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &provider.Fake{Responses: []string{`<tool>{"name":"read_file","args":{"path":"README.md","start":1,"end":20}}</tool>`, `<final>README says hello.</final>`}}
	agent, err := New(Options{Root: root, Config: config.Defaults(), Provider: fake, Approval: permission.Auto, MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	answer, err := agent.Ask(context.Background(), "What does README say?")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "README says hello." {
		t.Fatalf("answer=%q", answer)
	}
	trace, err := os.ReadFile(filepath.Join(root, ".pico", "runs", agent.LastRunID(), "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tool_started", "tool_completed", "run_completed"} {
		if !strings.Contains(string(trace), want) {
			t.Fatalf("trace missing %s", want)
		}
	}
}

func TestToolBudgetAllowsFinalAnswerButNeverAnotherTool(t *testing.T) {
	for _, final := range []bool{true, false} {
		t.Run(fmt.Sprint(final), func(t *testing.T) {
			root := t.TempDir()
			last := `<tool>{"name":"write_file","args":{"path":"over-budget.txt","content":"no"}}</tool>`
			if final {
				last = `<final>done</final>`
			}
			responses := []string{`<tool>{"name":"list_files","args":{"path":"."}}</tool>`, last}
			if !final {
				responses = append(responses, last)
			}
			fake := &provider.Fake{Responses: responses}
			a, err := New(Options{Root: root, Config: config.Defaults(), Provider: fake, Approval: permission.Auto, MaxSteps: 1, DisableAutoDream: true})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			answer, err := a.Ask(context.Background(), "list and finish")
			if err != nil {
				t.Fatal(err)
			}
			var report model.RunReport
			raw, err := os.ReadFile(filepath.Join(root, ".pico", "runs", a.LastRunID(), "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &report); err != nil {
				t.Fatal(err)
			}
			want := model.StopStepLimit
			if final {
				want = model.StopFinalAnswer
			}
			attempts := 2
			if !final {
				attempts = 3
			}
			if report.StopReason != want || report.ToolSteps != 1 || report.Attempts != attempts {
				t.Fatalf("report=%+v answer=%s", report, answer)
			}
			if _, err := os.Stat(filepath.Join(root, "over-budget.txt")); !os.IsNotExist(err) {
				t.Fatal("tool ran after budget exhausted")
			}
		})
	}
}

func TestModelAttemptBudgetHasSeparateStopReason(t *testing.T) {
	fake := &provider.Fake{Responses: []string{"not a valid protocol"}}
	a, err := New(Options{Root: t.TempDir(), Config: config.Defaults(), Provider: fake, MaxSteps: 5, MaxAttempts: 1, DisableAutoDream: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err = a.Ask(context.Background(), "finish"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(a.Workspace.Root, ".pico", "runs", a.LastRunID(), "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report model.RunReport
	if err = json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.StopReason != model.StopAttemptLimit || report.ToolSteps != 0 {
		t.Fatalf("report=%+v", report)
	}
}

func TestPatchRequiresFreshRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	_ = os.WriteFile(path, []byte("old"), 0o600)
	fake := &provider.Fake{Responses: []string{`<tool>{"name":"patch_file","args":{"path":"a.txt","old_text":"old","new_text":"new"}}</tool>`, `<final>done</final>`}}
	agent, err := New(Options{Root: root, Config: config.Defaults(), Provider: fake, Approval: permission.Auto, MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	_, _ = agent.Ask(context.Background(), "patch it")
	b, _ := os.ReadFile(path)
	if string(b) != "old" {
		t.Fatalf("file changed without prior read: %s", b)
	}
}

func TestToolAllowlistBlocksUnavailableTool(t *testing.T) {
	root := t.TempDir()
	fake := &provider.Fake{Responses: []string{`<tool>{"name":"write_file","args":{"path":"blocked.txt","content":"no"}}</tool>`, `<final>done</final>`}}
	agent, err := New(Options{Root: root, Config: config.Defaults(), Provider: fake, Approval: permission.Auto, MaxSteps: 2, AllowedTools: []string{"read_file"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	_, _ = agent.Ask(context.Background(), "write it")
	if _, err = os.Stat(filepath.Join(root, "blocked.txt")); !os.IsNotExist(err) {
		t.Fatal("disallowed tool wrote a file")
	}
}

func TestAutoDreamRunsAfterSessionGate(t *testing.T) {
	root := t.TempDir()
	sessions := store.NewSessionStore(root)
	for range 2 {
		session := sessions.New(root)
		session.MemoryUser = "local"
		if err := sessions.Save(session); err != nil {
			t.Fatal(err)
		}
	}
	fake := &provider.Fake{Responses: []string{`<final>done</final>`, `<final>- Decision: keep contract tests.</final>`}}
	agent, err := New(Options{Root: root, Config: config.Defaults(), Provider: fake, Approval: permission.Auto, DreamMinSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	if _, err = agent.Ask(context.Background(), "finish"); err != nil {
		t.Fatal(err)
	}
	if err = agent.WaitForMemoryMaintenance(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".pico", "memory", "topics", "dream.md"))
	if err != nil || !strings.Contains(string(b), "keep contract tests") {
		t.Fatalf("dream output missing: %s %v", b, err)
	}
}

func TestStrictReadinessRequiresVerificationAfterChange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module readiness-test\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package readiness\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	verifyCommand := filepath.Join(gostdruntime.GOROOT(), "bin", "go") + " test ./..."
	fake := &provider.Fake{Responses: []string{
		`<tool>{"name":"write_file","args":{"path":"notes.txt","content":"changed"}}</tool>`,
		`<final>premature</final>`,
		`<tool>{"name":"run_shell","args":{"command":"` + verifyCommand + `"}}</tool>`,
		`<final>verified</final>`,
	}}
	agent, err := New(Options{Root: root, Config: config.Defaults(), Provider: fake, Approval: permission.Auto, FinalReadiness: governance.Strict, DisableAutoDream: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	answer, err := agent.Ask(context.Background(), "change and verify")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "verified" || fake.Index != 4 {
		t.Fatalf("answer=%q calls=%d", answer, fake.Index)
	}
	trace, err := os.ReadFile(filepath.Join(root, ".pico", "runs", agent.LastRunID(), "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"final_readiness_notice", "verification_completed", `"decision":"allow"`} {
		if !strings.Contains(string(trace), marker) {
			t.Fatalf("trace missing %q: %s", marker, trace)
		}
	}
}

func TestSessionAndTracePersistenceRedactSecrets(t *testing.T) {
	root := t.TempDir()
	secret := "sk-abcdefghijklmnop"
	fake := &provider.Fake{Responses: []string{
		`<tool>{"name":"todo_add","args":{"text":"sk-abcdefghijklmnop"}}</tool>`,
		`<final>done sk-abcdefghijklmnop</final>`,
	}}
	agent, err := New(Options{Root: root, Config: config.Defaults(), Provider: fake, Approval: permission.Auto, DisableAutoDream: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	if _, err = agent.Ask(context.Background(), "record a todo"); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(root, ".pico", "sessions", agent.SessionID()+".json"),
		filepath.Join(root, ".pico", "sessions", agent.SessionID()+".events.jsonl"),
		filepath.Join(root, ".pico", "runs", agent.LastRunID(), "trace.jsonl"),
		filepath.Join(root, ".pico", "runs", agent.LastRunID(), "task_state.json"),
		filepath.Join(root, ".pico", "runs", agent.LastRunID(), "report.json"),
	}
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("secret persisted in %s: %s", path, data)
		}
	}
}

func TestResumeValidatesLatestCheckpoint(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("stable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := New(Options{Root: root, Config: config.Defaults(), Provider: &provider.Fake{Responses: []string{`<final>first</final>`}}, Approval: permission.Auto, DisableAutoDream: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err = first.Ask(context.Background(), "first turn"); err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := New(Options{Root: root, Config: config.Defaults(), Provider: &provider.Fake{Responses: []string{`<final>resumed</final>`}}, Resume: first.SessionID(), Approval: permission.Auto, DisableAutoDream: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	if resumed.ResumeStatus != model.ResumeFull {
		t.Fatalf("resume status=%q", resumed.ResumeStatus)
	}
	if err = resumed.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "README.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mismatched, err := New(Options{Root: root, Config: config.Defaults(), Provider: &provider.Fake{Responses: []string{`<final>mismatch</final>`}}, Resume: first.SessionID(), Approval: permission.Auto, DisableAutoDream: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mismatched.Close() })
	if mismatched.ResumeStatus != model.ResumeWorkspace {
		t.Fatalf("mismatch status=%q", mismatched.ResumeStatus)
	}
}
