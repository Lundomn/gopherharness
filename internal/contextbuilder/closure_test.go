package contextbuilder

import (
	"github.com/Lundomn/gopherharness/internal/model"
	"testing"
)

func TestStructuredClosure(t *testing.T) {
	valid := []model.Message{{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "a", Name: "read_file"}}}, {Role: "tool", ToolCallID: "a"}}
	if err := ValidateClosure(valid); err != nil {
		t.Fatal(err)
	}
	for _, m := range [][]model.Message{valid[:1], append(append([]model.Message{}, valid...), valid[1]), {{Role: "tool", ToolCallID: "unknown"}}} {
		if ValidateClosure(m) == nil {
			t.Fatal("broken exchange accepted")
		}
	}
}
