package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

type Mode string

const (
	Off    Mode = "off"
	Warn   Mode = "warn"
	Soft   Mode = "soft"
	Strict Mode = "strict"
)

type Evidence struct {
	ChangedPaths          []string
	VerificationAttempted bool
	VerificationSucceeded bool
	RunningWorkers        []string
	ReminderAlreadySent   bool
}

type Reason struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type Decision struct {
	Mode      Mode     `json:"mode"`
	Decision  string   `json:"decision"`
	Action    string   `json:"action"`
	Reasons   []Reason `json:"reasons,omitempty"`
	Signature string   `json:"reason_signature,omitempty"`
}

func ParseMode(value string) (Mode, bool) {
	mode := Mode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case Off, Warn, Soft, Strict:
		return mode, true
	default:
		return Warn, false
	}
}

func Evaluate(mode Mode, evidence Evidence) Decision {
	decision := Decision{Mode: mode, Decision: "allow", Action: "none"}
	if mode == Off {
		return decision
	}
	if len(evidence.ChangedPaths) > 0 && !evidence.VerificationSucceeded {
		message := "Files changed, but no successful verification was recorded."
		if evidence.VerificationAttempted {
			message = "Files changed, and the latest verification did not succeed."
		}
		decision.Reasons = append(decision.Reasons, Reason{Code: "changed_paths_without_verification", Severity: "hard", Message: message})
	}
	if len(evidence.RunningWorkers) > 0 {
		decision.Reasons = append(decision.Reasons, Reason{Code: "workers_still_running", Severity: "soft", Message: "Background workers are still running: " + strings.Join(evidence.RunningWorkers, ", ") + "."})
	}
	if len(decision.Reasons) == 0 {
		return decision
	}
	decision.Signature = reasonSignature(decision.Reasons)
	switch mode {
	case Warn:
		decision.Decision = "warn"
	case Soft:
		if evidence.ReminderAlreadySent {
			decision.Decision = "warn"
		} else {
			decision.Decision = "remind"
			decision.Action = "runtime_notice"
		}
	case Strict:
		decision.Decision = "warn"
		for _, reason := range decision.Reasons {
			if reason.Severity == "hard" {
				decision.Decision = "block"
				decision.Action = "block"
				break
			}
		}
	}
	return decision
}

func Notice(decision Decision) string {
	prefix := "Before final answer, address these readiness issues:"
	if decision.Action == "block" {
		prefix = "Final answer blocked by the runtime readiness gate:"
	}
	var lines []string
	for _, reason := range decision.Reasons {
		lines = append(lines, "- "+reason.Message)
	}
	return prefix + "\n" + strings.Join(lines, "\n") + "\nRun the relevant checks, wait for workers, or explain the limitation before returning final again."
}

func IsVerificationCommand(command string) bool {
	command = strings.ToLower(strings.TrimSpace(command))
	markers := []string{
		"go test", "go vet", "go build", "golangci-lint", "staticcheck",
		"pytest", "python -m pytest", "npm test", "npm run test", "pnpm test", "yarn test",
		"cargo test", "mvn test", "gradle test", "make test", "make check", "make build",
	}
	for _, marker := range markers {
		if strings.Contains(command, marker) {
			return true
		}
	}
	return false
}

func reasonSignature(reasons []Reason) string {
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, reason.Code)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:8])
}
