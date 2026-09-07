package permission

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

type Policy string

const (
	Ask   Policy = "ask"
	Auto  Policy = "auto"
	Never Policy = "never"
)

type Decision struct {
	Allowed       bool
	Reason        string
	SecurityEvent string
}

type Approver interface {
	Approve(tool string, args map[string]any) bool
}

func Check(policy Policy, risky bool, tool string, args map[string]any, approver Approver) Decision {
	if !risky {
		return Decision{Allowed: true, Reason: "read-only tool"}
	}
	switch policy {
	case Auto:
		return Decision{Allowed: true, Reason: "approval policy auto"}
	case Never:
		return Decision{Allowed: false, Reason: "approval policy never", SecurityEvent: "approval_denied"}
	case Ask:
		if approver != nil && approver.Approve(tool, args) {
			return Decision{Allowed: true, Reason: "user approved"}
		}
		return Decision{Allowed: false, Reason: "user denied or approval unavailable", SecurityEvent: "approval_denied"}
	default:
		return Decision{Allowed: false, Reason: "invalid approval policy", SecurityEvent: "approval_denied"}
	}
}

type TerminalApprover struct {
	In  io.Reader
	Out io.Writer
}

func (a TerminalApprover) Approve(tool string, args map[string]any) bool {
	fmt.Fprintf(a.Out, "Allow %s %v? [y/N] ", tool, args)
	line, _ := bufio.NewReader(a.In).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
