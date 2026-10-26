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
	for _, command := range []string{"go test ./...", "/usr/local/go/bin/go test ./...", "go vet ./...", "make check", "pytest -q", "GOPROXY=off go test ./...", "go test -run '^TestFoo$' ./...", "python3 -m pytest -q", "npm run test", "golangci-lint run"} {
		if !IsVerificationCommand(command) {
			t.Fatalf("not classified as verification: %q", command)
		}
	}
	for _, command := range []string{
		"git status --short", "echo 'go test ./...'", "printf 'pytest'", "go test ./... || true",
		"false; go test ./...", "go test ./...; true", "go test ./... | cat", "go test ./... &",
		"# go test ./...", "go test ./... # hidden", "go test ./...\ntrue", "go test $(echo ./...)",
		"go test `echo ./...`", "go test > results.log", "go test \"$PACKAGES\"", "go test 'unclosed",
		"env sh -c 'go test ./... || true'", "go version", "make test-ignored", "golangci-lint version",
	} {
		if IsVerificationCommand(command) {
			t.Fatalf("not a standalone verifier: %q", command)
		}
	}
}
