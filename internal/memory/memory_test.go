package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryPersistsAndPromotesDurableFacts(t *testing.T) {
	root := t.TempDir()
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetGoal("fix tests")
	m.ObserveFile("README.md", "project docs")
	m.AddNote("pytest is required", "dependency", "test")
	if !strings.Contains(m.Render("pytest"), "pytest is required") {
		t.Fatal("relevant note missing")
	}
	if err = m.Promote("Decision: Keep trace JSONL compatible.\nOther text"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".pico", "memory", "topics", "project.md"))
	if err != nil || !strings.Contains(string(b), "Keep trace JSONL compatible") {
		t.Fatalf("durable memory missing: %s %v", b, err)
	}
}

func TestDreamGateLockAndSecretRedaction(t *testing.T) {
	root := t.TempDir()
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if due, _ := m.DreamDue(1, 2, time.Hour); due {
		t.Fatal("dream should wait for the session gate")
	}
	release, err := m.AcquireDream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.AcquireDream(); err == nil {
		t.Fatal("dream lock was acquired twice")
	}
	release()
	lock := filepath.Join(root, ".pico", "memory", ".consolidate-lock")
	if err = os.WriteFile(lock, []byte("released\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := m.AcquireDream()
	if err != nil {
		t.Fatalf("stale lock was not recovered: %v", err)
	}
	recovered()
	path, err := m.FinishDream("- Decision: keep tests\n- api_key=sk-1234567890abcdefghijkl", 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk-123") || !strings.Contains(string(b), "[REDACTED]") {
		t.Fatalf("secret was not redacted: %s", b)
	}
}

func TestWorkingAndDurableMemoryRejectSecrets(t *testing.T) {
	root := t.TempDir()
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	secret := "sk-abcdefghijklmnop"
	m.SetGoal("use " + secret)
	m.AddNote("password=hunter2", "project", "test")
	m.ObserveFile(".env", "API_KEY="+secret)
	if err = m.Promote("Decision: keep " + secret); err != nil {
		t.Fatal(err)
	}
	working, err := os.ReadFile(filepath.Join(root, ".pico", "memory", "working.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(working), secret) || strings.Contains(string(working), "hunter2") {
		t.Fatalf("secret persisted in working memory: %s", working)
	}
	project := filepath.Join(root, ".pico", "memory", "topics", "project.md")
	if data, readErr := os.ReadFile(project); readErr == nil && strings.Contains(string(data), secret) {
		t.Fatalf("secret persisted in durable memory: %s", data)
	}
}
