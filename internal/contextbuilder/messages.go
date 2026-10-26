package contextbuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tools"
)

// RefreshLegacyTools replaces only the exact originally assembled prefix.
// History, memory and in-flight tool results are not reselected or rewritten.
func RefreshLegacyTools(prompt, workspace string, oldSpecs, newSpecs []tools.Spec) (string, error) {
	oldPrefix := buildPrefix(workspace, oldSpecs)
	if !strings.HasPrefix(prompt, oldPrefix) {
		return "", fmt.Errorf("legacy prompt does not match original tool prefix")
	}
	return buildPrefix(workspace, newSpecs) + strings.TrimPrefix(prompt, oldPrefix), nil
}

// BuildMessagesChecked selects history with the same gates as BuildChecked,
// but preserves provider roles and tool-call IDs instead of flattening them.
// Tool contracts are counted once as a separate wire field, never inserted in
// the system message. The caller must send the same in.Tools snapshot.
func BuildMessagesChecked(ctx context.Context, in Input, selector Selector) ([]model.Message, Metadata, error) {
	var messages []model.Message
	if err := ValidateClosure(in.History); err != nil {
		return nil, Metadata{}, err
	}
	// Legacy text sessions have tool output without native call IDs. Preserve
	// its provenance as quoted historical data, never invent an API call/result.
	in.History = append([]model.Message(nil), in.History...)
	for i := range in.History {
		m := &in.History[i]
		if m.Role == "tool" && m.ToolCallID == "" {
			m.Role = "user"
			m.Content = "[Historical tool result; not a current tool call]\n" + m.Content
		}
	}
	in.native, in.resultMessages = true, &messages
	_, meta, err := BuildChecked(ctx, in, selector)
	return messages, meta, err
}

// MeasureMessages is the per-iteration gate. It does not repeat selection or
// memory recall. Usage remains an estimate; the provider can still reject it.
func MeasureMessages(in Input, messages []model.Message) (Metadata, error) {
	meta := Metadata{ContextWindow: in.MaxTokens, OutputReserve: in.OutputReserve, CountIsEstimate: true}
	if meta.ContextWindow == 0 {
		meta.ContextWindow = DefaultContextWindow
		if meta.OutputReserve == 0 {
			meta.OutputReserve = DefaultOutputReserve
		}
	}
	meta.InputBudget = meta.ContextWindow - meta.OutputReserve
	if meta.ContextWindow < 1 || meta.OutputReserve < 0 || meta.InputBudget <= 0 || in.MaxChars < 0 {
		return meta, fmt.Errorf("invalid context/output/byte budget")
	}
	if err := ValidateClosure(messages); err != nil {
		return meta, err
	}
	counter := in.Counter
	if counter == nil {
		var err error
		counter, err = NewCounter("")
		if err != nil {
			return meta, err
		}
	}
	serialized, err := serializeMessages(messages, in.Tools)
	if err != nil {
		return meta, err
	}
	n, err := counter.Count(serialized)
	if err != nil {
		return meta, err
	}
	if n <= 0 {
		return meta, fmt.Errorf("invalid token count %d", n)
	}
	meta.CountSource = counter.Name()
	meta.FramingReserve = 8
	meta.PromptChars, meta.PromptTokens = len(serialized), n+meta.FramingReserve
	meta.ConservativeTokens = meta.PromptTokens
	if meta.PromptTokens > meta.InputBudget || (in.MaxChars > 0 && meta.PromptChars > in.MaxChars) {
		meta.BudgetExceeded = true
		return meta, ErrBudget
	}
	return meta, nil
}

// Wire-shaped representation deliberately omits local ledger IDs/timestamps.
// Function arguments are JSON strings, as in native chat completion requests.
func serializeMessages(messages []model.Message, specs []tools.Spec) (string, error) {
	ms := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		item := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.NativeContent) > 0 {
			item["native_content"] = m.NativeContent
		}
		if m.ToolCallID != "" {
			item["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				args, err := json.Marshal(c.Args)
				if err != nil {
					return "", err
				}
				calls = append(calls, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": string(args)}})
			}
			item["tool_calls"] = calls
		}
		ms = append(ms, item)
	}
	payload := map[string]any{"messages": ms}
	if len(specs) > 0 {
		ts := make([]map[string]any, 0, len(specs))
		for _, s := range specs {
			raw := s.Schema
			if raw == "" {
				raw = "{}"
			}
			parameters, err := tools.JSONSchema(raw)
			if err != nil {
				return "", fmt.Errorf("tool %s schema: %w", s.Name, err)
			}
			ts = append(ts, map[string]any{"type": "function", "function": map[string]any{"name": s.Name, "description": s.Description, "parameters": parameters}})
		}
		payload["tools"] = ts
	}
	b, err := json.Marshal(payload)
	return string(b), err
}
