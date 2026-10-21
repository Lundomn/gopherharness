package contextbuilder

import (
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
	MaxTokens, OutputReserve       int
}
type Metadata struct {
	PromptChars        int            `json:"prompt_chars"`
	RawChars           int            `json:"raw_chars"`
	Reduced            bool           `json:"reduced"`
	PrefixHash         string         `json:"prefix_hash"`
	SectionChars       map[string]int `json:"section_chars"`
	Path               string         `json:"selection_path"`
	SelectedGroups     []int          `json:"selected_groups"`
	BudgetExceeded     bool           `json:"budget_exceeded"`
	ConservativeTokens int            `json:"conservative_tokens"`
}

func buildPrefix(workspace string, specs []tools.Spec) string {
	var b strings.Builder
	b.WriteString("You are GopherHarness, a local coding agent. Work from evidence and use tools instead of guessing.\n\n")
	b.WriteString("Response protocol:\n- Tool: <tool>{\"name\":\"read_file\",\"args\":{\"path\":\"README.md\"}}</tool>\n- Final: <final>answer</final>\n- Never mix an earlier final answer before a tool call.\n\nTools:\n")
	b.WriteString("Before returning final after changing files, run the relevant tests, build, lint, or verifier and report the result.\n\n")
	for _, s := range specs {
		risk := "not approval-gated"
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
	var b strings.Builder
	b.WriteString("Transcript:\n")
	for _, m := range history {
		fmt.Fprintf(&b, "%s[message=%s reply_to=%s]: %s\n", m.Role, m.ID, m.ToolCallID, m.Content)
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
