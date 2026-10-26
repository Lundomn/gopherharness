package runtime

import (
	"fmt"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
)

// Reopening a session resumes its ledger, not an interrupted process. Missing
// receipts become explicit unknown results; the model must inspect reality
// before proposing another action. No old tool call is automatically re-run.
func closeInterruptedExchanges(messages []model.Message) ([]model.Message, int, error) {
	out := make([]model.Message, 0, len(messages))
	pending := []model.ToolCall{}
	got, seen := map[string]bool{}, map[string]bool{}
	repaired := 0
	flush := func() {
		for _, c := range pending {
			if !got[c.ID] {
				out = append(out, model.Message{Role: "tool", ToolCallID: c.ID, Content: fmt.Sprintf("Tool %s has no durable receipt after interruption. Outcome is unknown; do not repeat a write. Inspect the workspace or reconcile the external operation first.", c.Name), CreatedAt: time.Now().UTC()})
				repaired++
			}
		}
		pending = nil
		got = map[string]bool{}
	}
	for _, m := range messages {
		if m.Role != "tool" {
			flush()
		}
		if len(m.ToolCalls) > 0 {
			for _, c := range m.ToolCalls {
				if c.ID == "" {
					continue
				}
				if seen[c.ID] {
					return nil, 0, fmt.Errorf("corrupt session: repeated tool call ID")
				}
				seen[c.ID] = true
				pending = append(pending, c)
			}
		}
		if m.Role == "tool" && m.ToolCallID != "" {
			found := false
			for _, c := range pending {
				found = found || c.ID == m.ToolCallID
			}
			if !found || got[m.ToolCallID] {
				return nil, 0, fmt.Errorf("corrupt session: orphan tool receipt")
			}
			got[m.ToolCallID] = true
		}
		out = append(out, m)
	}
	flush()
	return out, repaired, nil
}
