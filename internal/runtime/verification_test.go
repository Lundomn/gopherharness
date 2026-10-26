package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Lundomn/gopherharness/internal/evidence"
	"github.com/Lundomn/gopherharness/internal/governance"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/store"
	"github.com/Lundomn/gopherharness/internal/tools"
)

func TestReadinessUsesRealProcessStatusNotPrintedExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	a, err := New(Options{Root: t.TempDir(), Provider: &provider.Fake{}, DisableAutoDream: true, HistorySelection: "rule"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// This fake test command deliberately prints the previous success marker,
	// but actually exits seven. The process metadata must win.
	result := a.Tools.Run(context.Background(), a, model.ToolCall{Name: "run_shell", Args: map[string]any{"command": "printf 'exit_code: 0\\n'; exit 7"}})
	if result.Metadata["shell_exit_code"] != 7 || tools.SuccessfulShellExit(result.Metadata) {
		t.Fatalf("printed output replaced the process status: %+v", result)
	}
	recorder := evidence.New(store.NewRunStore(a.Workspace.Root), a.Sessions, a.Session.ID, a.Redactor)
	task := &model.TaskState{ChangedPaths: []string{"source.go"}}
	call := model.ToolCall{Name: "run_shell", Args: map[string]any{"command": "go test ./..."}}
	a.acceptToolResult(recorder, task, call, result, false)
	if !task.VerificationAttempted || task.VerificationSucceeded || governance.Evaluate(governance.Strict, governance.Evidence{ChangedPaths: task.ChangedPaths, VerificationSucceeded: task.VerificationSucceeded}).Decision != "block" {
		t.Fatalf("failed process passed strict readiness: %+v", task)
	}
}

func TestReadinessRejectsEchoedAndFailureSwallowingChecks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixtures")
	}
	a, err := New(Options{Root: t.TempDir(), Provider: &provider.Fake{}, DisableAutoDream: true, HistorySelection: "rule"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := os.WriteFile(filepath.Join(a.Workspace.Root, "go.mod"), []byte("module verificationfixture\n\ngo 1.20\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Workspace.Root, "main.go"), []byte("package verificationfixture\nfunc Valid() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"echo 'go test ./...'", "false || true", "go test ./... || true", "go test ./..."} {
		call := model.ToolCall{Name: "run_shell", Args: map[string]any{"command": command, "timeout": 30}}
		result := a.Tools.Run(context.Background(), a, call)
		if result.Status != "ok" || !tools.SuccessfulShellExit(result.Metadata) {
			t.Fatalf("fixture command did not finish successfully: %s %+v", command, result)
		}
		// Exercise persisted receipt restoration as well as the live result.
		raw, _ := json.Marshal(result)
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		recorder := evidence.New(store.NewRunStore(a.Workspace.Root), a.Sessions, a.Session.ID, a.Redactor)
		task := &model.TaskState{ChangedPaths: []string{"main.go"}}
		a.acceptToolResult(recorder, task, call, result, false)
		if want := command == "go test ./..."; task.VerificationSucceeded != want {
			t.Fatalf("command=%q verified=%v want=%v", command, task.VerificationSucceeded, want)
		}
	}
}
