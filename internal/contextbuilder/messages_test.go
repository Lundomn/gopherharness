package contextbuilder

import (
	"context"
	"strings"
	"testing"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tools"
)

func TestRefreshLegacyToolsOnlyReplacesExactPrefix(t *testing.T) {
	old := []tools.Spec{{Name: "old_tool", Description: "old description", Schema: "{}"}}
	next := []tools.Spec{{Name: "new_tool", Description: "new description", Schema: "{}"}}
	in := Input{Workspace: "workspace", Tools: old, Memory: "preserve memory", History: []model.Message{{Role: "user", Content: "previous old_tool"}}, UserMessage: "current request"}
	prompt, _, err := BuildChecked(context.Background(), in, nil)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := RefreshLegacyTools(prompt, in.Workspace, old, next)
	if err != nil || !strings.HasPrefix(refreshed, buildPrefix(in.Workspace, next)) {
		t.Fatal(refreshed, err)
	}
	if strings.TrimPrefix(refreshed, buildPrefix(in.Workspace, next)) != strings.TrimPrefix(prompt, buildPrefix(in.Workspace, old)) {
		t.Fatal("history or memory changed")
	}
	for _, invalid := range []string{"untrusted " + prompt, "history contains " + buildPrefix(in.Workspace, old)} {
		if _, err := RefreshLegacyTools(invalid, in.Workspace, old, next); err == nil {
			t.Fatal("matched prefix from inside untrusted body")
		}
	}
}
