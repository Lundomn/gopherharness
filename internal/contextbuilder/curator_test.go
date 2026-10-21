package contextbuilder

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Lundomn/gopherharness/internal/model"
)

func TestProtectedBudgetFailsClosedIncludingSkills(t *testing.T) {
	for _, input := range []Input{
		{UserMessage: strings.Repeat("x", 2000), MaxChars: 1000},
		{UserMessage: "q", Skills: strings.Repeat("x", 2000), MaxChars: 1000},
		{UserMessage: "q", History: []model.Message{{Role: "user", Content: strings.Repeat("x", 2000)}}, MaxChars: 1000},
	} {
		prompt, meta, err := BuildChecked(context.Background(), input, nil)
		if !errors.Is(err, ErrBudget) || prompt != "" || !meta.BudgetExceeded {
			t.Fatalf("prompt=%d meta=%+v err=%v", len(prompt), meta, err)
		}
	}
}

func TestInvalidModelSelectionFallsBackAndCannotRemoveProtected(t *testing.T) {
	input := Input{UserMessage: "target", MaxChars: 1500, History: []model.Message{
		{Role: "user", Content: "initial"}, {Role: "user", Content: strings.Repeat("irrelevant", 300)},
		{Role: "assistant", Content: "OLD_TOOL_CALL_MARKER"}, {Role: "tool", Content: "OLD_TOOL_RESULT_MARKER"}, {Role: "user", Content: "recent"}, {Role: "user", Content: "current"},
	}}
	for _, selection := range [][]Selection{{{ID: 999, Score: 1}}, {{ID: 1, Score: 2}}, {{ID: 1, Score: 0.2}, {ID: 1, Score: 0.5}}} {
		prompt, meta, err := BuildChecked(context.Background(), input, func(context.Context, string, []Group) ([]Selection, error) { return selection, nil })
		if err != nil || len(prompt) > input.MaxChars || meta.Path != "rule_fallback" {
			t.Fatalf("%+v %v", meta, err)
		}
		if !strings.Contains(prompt, "initial") || !strings.Contains(prompt, "current") || strings.Contains(prompt, "OLD_TOOL_CALL_MARKER") || strings.Contains(prompt, "OLD_TOOL_RESULT_MARKER") {
			t.Fatal("group/protection broken")
		}
	}
}

func TestModelSelectorProposalUsesValidatedIDs(t *testing.T) {
	s := ModelSelector(func(_ context.Context, prompt string, _ int) (string, error) {
		if !strings.Contains(prompt, "untrusted") {
			t.Fatal("missing trust boundary")
		}
		return `[{"id":1,"score":0.9}]`, nil
	})
	got, err := s(context.Background(), "q", []Group{{ID: 1, Preview: "history"}})
	if err != nil || len(got) != 1 || got[0].Score != 0.9 {
		t.Fatal(got, err)
	}
}
