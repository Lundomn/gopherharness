package contextbuilder

import (
	"fmt"
	"github.com/Lundomn/gopherharness/internal/model"
)

// Legacy text-only transcripts remain readable. Structured exchanges are
// rejected if a result is orphaned, duplicated, or split across user turns.
func ValidateClosure(messages []model.Message) error {
	pending := map[string]bool{}
	seen := map[string]bool{}
	for _, m := range messages {
		if m.Role != "tool" && len(pending) > 0 {
			return fmt.Errorf("incomplete tool exchange before next message")
		}
		if len(m.ToolCalls) > 0 && m.Role != "assistant" {
			return fmt.Errorf("tool calls require assistant role")
		}
		if m.Role == "tool" && m.ToolCallID == "" && len(pending) > 0 {
			return fmt.Errorf("missing tool result ID in native exchange")
		}
		for _, c := range m.ToolCalls {
			if c.ID == "" || seen[c.ID] {
				return fmt.Errorf("invalid or duplicate tool call ID")
			}
			seen[c.ID] = true
			pending[c.ID] = true
		}
		if m.ToolCallID != "" {
			if m.Role != "tool" || !pending[m.ToolCallID] {
				return fmt.Errorf("orphan or duplicate tool result %q", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("incomplete tool exchange: %d results missing", len(pending))
	}
	return nil
}
