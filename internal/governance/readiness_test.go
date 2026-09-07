package governance

import (
	"strings"
	"testing"
)

func TestReadinessModes(t *testing.T) {
	evidence := Evidence{ChangedPaths: []string{"main.go"}}
	if decision := Evaluate(Warn, evidence); decision.Decision != "warn" || decision.Action != "none" {
		t.Fatalf("warn decision: %#v", decision)
	}
	if decision := Evaluate(Soft, evidence); decision.Action != "runtime_notice" {
		t.Fatalf("soft decision: %#v", decision)
	}
	evidence.ReminderAlreadySent = true
	if decision := Evaluate(Soft, evidence); decision.Action != "none" {
		t.Fatalf("second soft decision: %#v", decision)
	}
	if decision := Evaluate(Strict, evidence); decision.Action != "block" || !strings.Contains(Notice(decision), "blocked") {
		t.Fatalf("strict decision: %#v", decision)
	}
	evidence.VerificationSucceeded = true
	if decision := Evaluate(Strict, evidence); decision.Decision != "allow" {
		t.Fatalf("verified decision: %#v", decision)
	}
}

func TestVerificationCommandClassification(t *testing.T) {
	for _, command := range []string{"go test ./...", "go vet ./...", "make check", "pytest -q"} {
		if !IsVerificationCommand(command) {
			t.Fatalf("not classified as verification: %q", command)
		}
	}
	if IsVerificationCommand("git status --short") {
		t.Fatal("read-only inspection classified as verification")
	}
}
