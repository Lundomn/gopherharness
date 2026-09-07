package contextbuilder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tools"
)

type Input struct {
	Workspace, Memory, UserMessage string
	Skills                         string
	History                        []model.Message
	Tools                          []tools.Spec
	MaxChars                       int
}
type Metadata struct {
	PromptChars  int            `json:"prompt_chars"`
	RawChars     int            `json:"raw_chars"`
	Reduced      bool           `json:"reduced"`
	PrefixHash   string         `json:"prefix_hash"`
	SectionChars map[string]int `json:"section_chars"`
}

func Build(in Input) (string, Metadata) {
	if in.MaxChars <= 0 {
		in.MaxChars = 60000
	}
	prefix := buildPrefix(in.Workspace, in.Tools)
	memory := in.Memory
	history := renderHistory(in.History)
	request := "Current request:\n" + in.UserMessage
	raw := strings.Join([]string{prefix, in.Skills, memory, history, request}, "\n\n")
	meta := Metadata{RawChars: len(raw), SectionChars: map[string]int{"prefix": len(prefix), "memory": len(memory), "history": len(history), "current_request": len(request)}}
	if len(raw) > in.MaxChars {
		fixed := len(prefix) + len(request) + 4
		budget := in.MaxChars - fixed
		if budget < 0 {
			budget = 0
		}
		memoryBudget := budget / 3
		historyBudget := budget - memoryBudget
		memory = tail(memory, memoryBudget)
		history = tail(history, historyBudget)
		meta.Reduced = true
	}
	prompt := strings.Join([]string{prefix, in.Skills, memory, history, request}, "\n\n")
	meta.PromptChars = len(prompt)
	sum := sha256.Sum256([]byte(prefix))
	meta.PrefixHash = hex.EncodeToString(sum[:])
	return prompt, meta
}

func buildPrefix(workspace string, specs []tools.Spec) string {
	var b strings.Builder
	b.WriteString("You are Pico, a local coding agent. Work from evidence and use tools instead of guessing.\n\n")
	b.WriteString("Response protocol:\n- Tool: <tool>{\"name\":\"read_file\",\"args\":{\"path\":\"README.md\"}}</tool>\n- Final: <final>answer</final>\n- Never mix an earlier final answer before a tool call.\n\nTools:\n")
	b.WriteString("Before returning final after changing files, run the relevant tests, build, lint, or verifier and report the result.\n\n")
	for _, s := range specs {
		risk := "read-only"
		if s.Risky {
			risk = "risky"
		}
		fmt.Fprintf(&b, "- %s %s [%s]: %s\n", s.Name, s.Schema, risk, s.Description)
	}
	b.WriteString("\n")
	b.WriteString(workspace)
	return strings.TrimSpace(b.String())
}
func renderHistory(history []model.Message) string {
	if len(history) == 0 {
		return "Transcript:\n- none"
	}
	start := 0
	if len(history) > 40 {
		start = len(history) - 40
	}
	var b strings.Builder
	b.WriteString("Transcript:\n")
	for _, m := range history[start:] {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}
	return strings.TrimSpace(b.String())
}
func tail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return "...[earlier content compacted]\n" + s[len(s)-n:]
}
