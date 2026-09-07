package contextbuilder

import (
	"strings"
	"testing"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tools"
)

func TestBuildKeepsCurrentRequestUnderPressure(t *testing.T) {
	history := []model.Message{{Role: "tool", Content: strings.Repeat("old", 5000)}}
	prompt, meta := Build(Input{Workspace: "workspace", Memory: strings.Repeat("memory", 1000), History: history, UserMessage: "MUST_KEEP", Tools: tools.NewRegistry().Specs(), MaxChars: 6000})
	if !strings.Contains(prompt, "MUST_KEEP") {
		t.Fatal("current request was removed")
	}
	if !meta.Reduced {
		t.Fatal("expected reduction")
	}
	if len(prompt) > 6500 {
		t.Fatalf("prompt too large: %d", len(prompt))
	}
}
