package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTodoAndPlanState(t *testing.T) {
	root := t.TempDir()
	todos := OpenTodo(root)
	id := todos.Add("port runtime")
	if err := todos.Update(id, "done"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(todos.List(), "[done]") {
		t.Fatal(todos.List())
	}
	plan := NewPlan(root)
	out, err := plan.Enter(".pico/plans/test.md")
	if err != nil || !strings.Contains(out, "active") {
		t.Fatalf("out=%s err=%v", out, err)
	}
	active, path := plan.Active()
	if !active || path != ".pico/plans/test.md" {
		t.Fatalf("active=%v path=%s", active, path)
	}
	plan.Exit()
	active, _ = plan.Active()
	if active {
		t.Fatal("plan remained active")
	}
}

func TestPlanRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := NewPlan(root).Enter("linked/plan.md"); err == nil {
		t.Fatal("plan path through an outside symlink was accepted")
	}
}

func TestPlanRejectsAbsolutePath(t *testing.T) {
	root := t.TempDir()
	if _, err := NewPlan(root).Enter(filepath.Join(root, "plan.md")); err == nil {
		t.Fatal("absolute plan path was accepted")
	}
}
