package contextbuilder

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tools"
)

type charCounter struct{}

func (charCounter) Count(s string) (int, error) { return len([]rune(s)), nil }
func (charCounter) Name() string                { return "test-runes" }

func slowInput() Input {
	in := Input{UserMessage: "find OLD_TARGET and continue", Counter: charCounter{}, MaxTokens: 18000, OutputReserve: 2048}
	for i := 0; i < 24; i++ {
		text := strings.Repeat("x", 950)
		if i == 8 {
			text = "OLD_TARGET " + text
		}
		in.History = append(in.History, model.Message{Role: "user", Content: text})
	}
	return in
}

func TestCuratorMultiStepSearchReadArchiveRetrieveStateAndPlan(t *testing.T) {
	in := slowInput()
	step := 0
	dir := t.TempDir()
	in.Curator = &CuratorConfig{ArchiveDir: dir, SessionKey: "session/one", Complete: func(ctx context.Context, prompt string, maxOutput int) (string, error) {
		if maxOutput != 2048 {
			t.Fatalf("output limit %d", maxOutput)
		}
		step++
		switch step {
		case 1:
			return `{"tool":"search","query":"OLD_TARGET"}`, nil
		case 2:
			if !strings.Contains(prompt, `"id":8`) {
				t.Fatal("search result missing")
			}
			return `{"tool":"read","ids":[8]}`, nil
		case 3:
			return `{"tool":"archive","ids":[8]}`, nil
		case 4:
			ref := regexp.MustCompile(`[a-f0-9]{64}\.jsonl`).FindString(prompt)
			if ref == "" {
				t.Fatal("archive not durable/referenced")
			}
			return `{"tool":"retrieve","ref":"` + ref + `"}`, nil
		case 5:
			return `{"tool":"score","scores":[{"id":8,"score":0.9}]}`, nil
		case 6:
			return `{"tool":"state","state":{"goals":["finish task"],"open_threads":["check test"],"decisions":["keep existing API"]}}`, nil
		case 7:
			return `{"tool":"budget","ids":[8,23]}`, nil
		case 8:
			return `{"tool":"finish","ids":[8,23]}`, nil
		default:
			t.Fatal("unexpected extra step")
			return "", nil
		}
	}}
	p, meta, err := BuildChecked(context.Background(), in, nil)
	if err != nil || meta.Path != "curator" || meta.CuratorSteps != 8 || len(meta.ArchiveRefs) != 1 || !strings.Contains(p, "finish task") || !strings.Contains(p, "OLD_TARGET") {
		t.Fatalf("%+v %v", meta, err)
	}
	if len(meta.SelectedGroups) != 8 || !meta.Reduced {
		t.Fatal(meta)
	}
	store, err := openCuratorStore(dir, "session/one")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.readState()
	if err != nil || len(state.Goals) != 1 {
		t.Fatal(state, err)
	}
	archived, err := store.retrieve(meta.ArchiveRefs[0])
	if err != nil || len(archived) != 1 || archived[0].Messages[0].Content != in.History[8].Content {
		t.Fatal(archived, err)
	}
}

func TestCuratorRejectsInvalidActionAndProtectedArchiveThenRecovers(t *testing.T) {
	in := slowInput()
	step := 0
	in.Curator = &CuratorConfig{ArchiveDir: t.TempDir(), SessionKey: "s", Complete: func(_ context.Context, p string, _ int) (string, error) {
		step++
		switch step {
		case 1:
			return `{"tool":"archive","ids":[0]}`, nil
		case 2:
			if !strings.Contains(p, "protected groups cannot") {
				t.Fatal(p)
			}
			return `{"tool":"score","scores":[{"id":999,"score":0.9}]}`, nil
		case 3:
			if !strings.Contains(p, "invalid score proposal") {
				t.Fatal(p)
			}
			return `{"tool":"finish","ids":[23]}`, nil
		default:
			t.Fatal("unexpected")
			return "", nil
		}
	}}
	_, meta, err := BuildChecked(context.Background(), in, nil)
	if err != nil || meta.Path != "curator" || meta.CuratorSteps != 3 {
		t.Fatal(meta, err)
	}
	if len(meta.SelectedGroups) != 7 {
		t.Fatal("head protection removed", meta.SelectedGroups)
	}
}

func TestCuratorBoundedExhaustionFallbackAndParentCancel(t *testing.T) {
	in := slowInput()
	calls := 0
	in.Curator = &CuratorConfig{MaxSteps: 3, Complete: func(context.Context, string, int) (string, error) {
		calls++
		return `{"tool":"shell","query":"rm"}`, nil
	}}
	p, meta, err := BuildChecked(context.Background(), in, nil)
	if err != nil || p == "" || calls != 3 || meta.Path != "rule_fallback" || meta.SelectionError == "" || meta.PromptTokens > meta.InputBudget {
		t.Fatal(meta, err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	in.Curator.Complete = func(context.Context, string, int) (string, error) {
		cancel()
		return `{"tool":"finish","ids":[23]}`, nil
	}
	if _, _, err := BuildChecked(ctx, in, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	in.SelectorTimeout = time.Millisecond
	in.Curator.Complete = func(ctx context.Context, _ string, _ int) (string, error) {
		<-ctx.Done()
		return `{"tool":"finish","ids":[23]}`, nil
	}
	_, meta, err = BuildChecked(context.Background(), in, nil)
	if err != nil || meta.Path != "rule_fallback" || !strings.Contains(meta.SelectionError, "deadline") {
		t.Fatal(meta, err)
	}
}

func TestCuratorStorageFailureDoesNotClaimArchiveOrDeleteHistory(t *testing.T) {
	in := slowInput()
	file := filepath.Join(t.TempDir(), "not-dir")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	in.Curator = &CuratorConfig{ArchiveDir: file, SessionKey: "s", Complete: func(context.Context, string, int) (string, error) {
		t.Fatal("model called after storage failure")
		return "", nil
	}}
	_, meta, err := BuildChecked(context.Background(), in, nil)
	if err != nil || meta.Path != "rule_fallback" || len(meta.ArchiveRefs) != 0 || len(in.History) != 24 {
		t.Fatal(meta, err)
	}
}

func TestCuratorArchiveScopeIntegrityAndRedaction(t *testing.T) {
	dir := t.TempDir()
	store, err := openCuratorStore(dir, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.redact = func(s string) string { return strings.ReplaceAll(s, "private-value", "[REDACTED]") }
	ref, err := store.archive([]Group{{ID: 1, Messages: []model.Message{{Role: "user", Content: "private-value sk-abcdefghijklmnop"}}}})
	if err != nil {
		t.Fatal(err)
	}
	gs, err := store.retrieve(ref)
	if err != nil || strings.Contains(gs[0].Messages[0].Content, "private-value") || strings.Contains(gs[0].Messages[0].Content, "sk-") {
		t.Fatal(gs, err)
	}
	other, err := openCuratorStore(dir, "two")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.retrieve(ref); err == nil {
		t.Fatal("cross-session archive visible")
	}
	for _, bad := range []string{"../working-state.json", "/etc/passwd", "working-state.json"} {
		if _, err := store.retrieve(bad); err == nil {
			t.Fatal("unsafe ref", bad)
		}
	}
	if err := store.write(ref, []byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.retrieve(ref); err == nil {
		t.Fatal("checksum ignored")
	}
}

func TestCuratorWorkingStateAndReadsAreBounded(t *testing.T) {
	s := WorkingState{Goals: make([]string, 20), OpenThreads: make([]string, 20), Decisions: make([]string, 30)}
	for i := range s.Goals {
		s.Goals[i] = strings.Repeat("中", 1000)
	}
	for i := range s.OpenThreads {
		s.OpenThreads[i] = "open"
	}
	for i := range s.Decisions {
		s.Decisions[i] = "decision"
	}
	s = boundedState(s)
	if len(s.Goals) != 8 || len([]rune(s.Goals[0])) != 420 || len(s.OpenThreads) != 12 || len(s.Decisions) != 20 {
		t.Fatal(s)
	}
	out, err := boundedGroups(makeGroups(slowInput().History, ""), charCounter{}, 2000)
	if err != nil || out.(map[string]any)["content_tokens"].(int) > 2000 {
		t.Fatal(out, err)
	}
}

func TestCuratorStateRemainsFixedMaterialOnNextFastTurn(t *testing.T) {
	dir := t.TempDir()
	store, err := openCuratorStore(dir, "same-session")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.writeState(WorkingState{Decisions: []string{"preserve request idempotency"}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	in := Input{UserMessage: "continue", Counter: charCounter{}, MaxTokens: 18000, OutputReserve: 2048,
		Curator: &CuratorConfig{ArchiveDir: dir, SessionKey: "same-session", Complete: func(context.Context, string, int) (string, error) {
			t.Fatal("fast path must not call curator")
			return "", nil
		}}}
	text, meta, err := BuildChecked(context.Background(), in, nil)
	if err != nil || meta.Path != "fast" || !strings.Contains(text, "preserve request idempotency") || meta.SectionTokens["working_state"] == 0 {
		t.Fatal(meta, err, text)
	}
	messages, meta, err := BuildMessagesChecked(context.Background(), in, nil)
	if err != nil || meta.Path != "fast" || len(messages) != 2 || !strings.Contains(messages[0].Content, "preserve request idempotency") {
		t.Fatal(meta, err, messages)
	}
}

func TestNativeAssemblyPreservesRolesCountsSchemasAndOpaqueContent(t *testing.T) {
	in := Input{UserMessage: "continue", History: []model.Message{
		{Role: "user", Content: "inspect"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "a", Name: "read_file", Args: map[string]any{"path": "README.md"}}}, NativeContent: []model.NativeItem{{Protocol: "responses", Data: json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque"}`)}}},
		{Role: "tool", ToolCallID: "a", Content: "actual result"},
		{Role: "user", Content: "continue"},
	}, Tools: []tools.Spec{{Name: "read_file", Description: "read", Schema: `{"path":"string"}`}}}
	msgs, meta, err := BuildMessagesChecked(context.Background(), in, nil)
	if err != nil || len(msgs) != 5 || msgs[0].Role != "system" || msgs[4].Content != "continue" {
		t.Fatal(msgs, meta, err)
	}
	if strings.Contains(msgs[0].Content, "<tool>") || strings.Contains(msgs[0].Content, "read_file") || msgs[2].ToolCalls[0].ID != "a" || len(msgs[2].NativeContent) != 1 {
		t.Fatal(msgs)
	}
	measured, err := MeasureMessages(in, msgs)
	if err != nil || measured.PromptTokens != meta.PromptTokens {
		t.Fatal(measured, meta, err)
	}
	wire, err := serializeMessages(msgs, in.Tools)
	if err != nil || !strings.Contains(wire, `"required":["path"]`) || !strings.Contains(wire, "opaque") {
		t.Fatal(wire, err)
	}
}

func TestNativeBudgetAndLegacyToolCompatibility(t *testing.T) {
	in := Input{UserMessage: "new", History: []model.Message{{Role: "user", Content: "old"}, {Role: "tool", Content: "legacy output"}}, Counter: charCounter{}, MaxTokens: 10000, Tools: []tools.Spec{{Name: "huge", Schema: `{"type":"object","description":"` + strings.Repeat("x", 12000) + `"}`}}}
	if msgs, meta, err := BuildMessagesChecked(context.Background(), in, nil); !errors.Is(err, ErrBudget) || len(msgs) != 0 || !meta.BudgetExceeded {
		t.Fatal(meta, err)
	}
	in.Tools = nil
	msgs, _, err := BuildMessagesChecked(context.Background(), in, nil)
	if err != nil || msgs[2].Role != "user" || !strings.Contains(msgs[2].Content, "Historical tool result") {
		t.Fatal(msgs, err)
	}
}
