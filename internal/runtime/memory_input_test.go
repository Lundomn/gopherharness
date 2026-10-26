package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lundomn/gopherharness/internal/memory"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/skills"
)

type failedMemory struct{}

func (failedMemory) Recall(context.Context, memory.Scope, string, int) ([]memory.Fact, error) {
	return nil, errors.New("recall unavailable")
}
func (failedMemory) Store(context.Context, memory.Scope, string, []memory.Evidence) error { return nil }
func (failedMemory) Close() error                                                         { return nil }

func TestRuntimeHostProfileAndRecallAreSeparateFromWorkingState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, memory.ProfileRelativePath("local"))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("## Preferences\nGo conventions\n## Notes\nPROFILE_MARKER"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{Root: root, Provider: &provider.Fake{Responses: []string{"<final>done</final>"}}, DisableAutoDream: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Memory.SetGoal("WORKING_GOAL_MARKER")
	s, err := a.memoryInput(context.Background(), "Go")
	if err != nil || !strings.Contains(s.Text, "PROFILE_MARKER") || strings.Contains(s.Text, "WORKING_GOAL_MARKER") {
		t.Fatal(s, err)
	}
	if !strings.Contains(a.Memory.Render("Go"), "WORKING_GOAL_MARKER") {
		t.Fatal("working state lost")
	}
	if _, err = a.Ask(context.Background(), "Go conventions"); err != nil {
		t.Fatal(err)
	}
}

func TestRecallFailureNeverPretendsNoHits(t *testing.T) {
	fake := &provider.Fake{Responses: []string{"<final>done</final>"}}
	a, err := New(Options{Root: t.TempDir(), Provider: fake, FactBackend: failedMemory{}, DisableAutoDream: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, err = a.Ask(context.Background(), "question")
	if err == nil || !strings.Contains(err.Error(), "recall unavailable") || fake.Index != 0 {
		t.Fatal(err, fake.Index)
	}
}

func TestBM25ReferencesCanBeReadWithoutExecutingSkill(t *testing.T) {
	a, err := New(Options{Root: t.TempDir(), Provider: &provider.Fake{Responses: []string{"<final>done</final>"}}, DisableAutoDream: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Skills.Replace([]skills.Skill{{Name: "reader", Description: "extract pdf", Path: "/registered/reader/SKILL.md", Body: "BODY_SENTINEL"}})
	s := a.Skills.Resolve("pdf")
	if len(s.Metadata.ReferencedIDs) != 1 {
		t.Fatal(s.Metadata)
	}
	result := a.Tools.Run(context.Background(), a, model.ToolCall{Name: "skill_read", Args: map[string]any{"name": "reader"}})
	if result.Status != "ok" || !strings.Contains(result.Output, "BODY_SENTINEL") || len(result.AffectedPaths) != 0 {
		t.Fatal(result)
	}
	bad := a.Tools.Run(context.Background(), a, model.ToolCall{Name: "skill_read", Args: map[string]any{"name": "../../secret"}})
	if bad.Status != "error" {
		t.Fatal("arbitrary path accepted")
	}
}
