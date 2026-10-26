package contextbuilder

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
)

type boundaryCounter struct {
	history int
	fail    bool
}

func (c boundaryCounter) Name() string { return "test-boundary" }
func (c boundaryCounter) Count(s string) (int, error) {
	if c.fail {
		return 0, errors.New("counter unavailable")
	}
	if s == "" {
		return 0, nil
	}
	if strings.HasPrefix(s, "Transcript:") {
		return c.history, nil
	}
	if strings.Contains(s, "Transcript:\n- none") {
		return 100, nil
	}
	return 200, nil
}

func TestFastThresholdUsesRemainingTokenBudget(t *testing.T) {
	// 1000 - 100 output - (100 fixed + 8 framing) = 792 history;
	// floor(792 * .60) = 475. Equality must NOT take the fast path.
	for _, n := range []int{474, 475, 476} {
		called := false
		_, m, err := BuildChecked(context.Background(), Input{MaxTokens: 1000, OutputReserve: 100, Counter: boundaryCounter{history: n}, History: []model.Message{{Role: "user", Content: "q"}}, UserMessage: "q"}, func(context.Context, string, []Group) ([]Selection, error) { called = true; return nil, nil })
		if err != nil || m.HistoryBudget != 792 || m.FastThreshold != 475 || called != (n >= 475) {
			t.Fatalf("n=%d meta=%+v called=%v err=%v", n, m, called, err)
		}
	}
}

func TestRealBPEAndEncodingValidation(t *testing.T) {
	for _, enc := range []string{"cl100k_base", "o200k_base"} {
		c, err := NewCounter(enc)
		if err != nil {
			t.Fatal(err)
		}
		n, err := c.Count("hello world")
		if err != nil || n != 2 {
			t.Fatalf("%s: %d %v", enc, n, err)
		}
		chinese := "上下文预算不是字节上限"
		n, err = c.Count(chinese)
		if err != nil || n <= 0 || n >= len(chinese) {
			t.Fatalf("%s: %d %v", enc, n, err)
		}
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				if _, e := c.Count(chinese); e != nil {
					t.Error(e)
				}
			})
		}
		wg.Wait()
	}
	if _, err := NewCounter("made_up_tokenizer"); err == nil {
		t.Fatal("unknown encoding accepted")
	}
}

func TestDefaultWindowOutputAndLargeBytePrompt(t *testing.T) {
	p, m, err := BuildChecked(context.Background(), Input{UserMessage: strings.Repeat("hello ", 12000)}, nil)
	if err != nil || len(p) <= 60000 || m.ContextWindow != 65536 || m.OutputReserve != 8192 || m.InputBudget != 57344 || m.PromptTokens >= len(p) || m.CountSource != "cl100k_base_estimate" {
		t.Fatalf("len=%d meta=%+v err=%v", len(p), m, err)
	}
	if m.SectionTokens["current_request"] <= 0 || m.PromptTokens > m.InputBudget {
		t.Fatal(m)
	}
}

func TestAllPathsFinalGateAndCounterFailure(t *testing.T) {
	for _, selector := range []Selector{nil, func(context.Context, string, []Group) ([]Selection, error) {
		return []Selection{{ID: 0, Score: 1}}, nil
	}, func(context.Context, string, []Group) ([]Selection, error) { return nil, errors.New("model failed") }} {
		p, m, err := BuildChecked(context.Background(), Input{UserMessage: strings.Repeat("独立内容。", 2000), History: []model.Message{{Role: "user", Content: "head"}}, MaxTokens: 1000, OutputReserve: 100}, selector)
		if !errors.Is(err, ErrBudget) || p != "" || !m.BudgetExceeded {
			t.Fatalf("%d %+v %v", len(p), m, err)
		}
	}
	if p, _, err := BuildChecked(context.Background(), Input{Counter: boundaryCounter{fail: true}}, nil); err == nil || p != "" {
		t.Fatal("counter error must fail closed")
	}
}

func TestSelectorTimeoutLateResultAndParentCancellation(t *testing.T) {
	in := Input{MaxTokens: 1000, OutputReserve: 100, Counter: boundaryCounter{history: 500}, History: []model.Message{{Role: "user", Content: "q"}}, UserMessage: "q", SelectorTimeout: time.Millisecond}
	_, m, err := BuildChecked(context.Background(), in, func(ctx context.Context, _ string, _ []Group) ([]Selection, error) {
		<-ctx.Done()
		return []Selection{{ID: 0, Score: 1}}, nil
	})
	if err != nil || m.Path != "rule_fallback" || m.SelectionError == "" {
		t.Fatalf("%+v %v", m, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, _, err = BuildChecked(ctx, in, func(context.Context, string, []Group) ([]Selection, error) { cancel(); return nil, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestProtectionAndSelectionDoNotAlterLedger(t *testing.T) {
	history := []model.Message{}
	for range 6 {
		history = append(history, model.Message{Role: "user", Content: "head"})
	}
	history = append(history, model.Message{Role: "user", Content: strings.Repeat("obsolete ", 1500)})
	for range 14 {
		history = append(history, model.Message{Role: "user", Content: "recent"})
	}
	history = append(history, model.Message{Role: "user", Content: "CURRENT_UNIQUE"})
	p, m, err := BuildChecked(context.Background(), Input{History: history, UserMessage: "CURRENT_UNIQUE", MaxTokens: 1000, OutputReserve: 100}, func(_ context.Context, _ string, g []Group) ([]Selection, error) {
		return []Selection{{ID: 6, Score: math.NaN()}}, nil
	})
	if err != nil || m.Path != "rule_fallback" || !m.Reduced || strings.Contains(p, "obsolete") || strings.Count(p, "CURRENT_UNIQUE") != 1 || !strings.Contains(history[6].Content, "obsolete") {
		t.Fatalf("%+v %v", m, err)
	}
	gs := makeGroups(history, "unrelated")
	if !gs[5].Protected || gs[6].Protected || !gs[len(gs)-1].Protected || gs[len(gs)-2].Score < .75 || gs[0].Score < .8 {
		t.Fatal("protection/floors broken")
	}
}

func TestInvalidBudgets(t *testing.T) {
	for _, in := range []Input{{MaxTokens: -1}, {MaxTokens: 100, OutputReserve: 100}, {MaxTokens: 100, OutputReserve: -1}, {FastThreshold: math.NaN()}, {FastThreshold: 1.1}, {MaxChars: -1}, {SelectorTimeout: -1}} {
		if _, _, err := BuildChecked(context.Background(), in, nil); err == nil {
			t.Fatalf("accepted %+v", in)
		}
	}
}

func TestFinalInputEqualityAndOptionalByteCeiling(t *testing.T) {
	// This deterministic counter gives the complete rendered prompt 200 tokens
	// plus the eight framing tokens. Equality is admitted, one less is rejected.
	for _, limit := range []int{207, 208, 209} {
		p, m, err := BuildChecked(context.Background(), Input{UserMessage: "q", History: []model.Message{{Role: "user", Content: "q"}}, MaxTokens: limit, Counter: boundaryCounter{history: 1}}, nil)
		if (err == nil) != (limit >= 208) || m.PromptTokens != 208 || ((p != "") != (err == nil)) {
			t.Fatalf("limit=%d meta=%+v err=%v", limit, m, err)
		}
	}
	p, _, err := BuildChecked(context.Background(), Input{UserMessage: "q"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{len(p) - 1, len(p)} {
		_, _, err := BuildChecked(context.Background(), Input{UserMessage: "q", MaxChars: limit}, nil)
		if (err == nil) != (limit == len(p)) {
			t.Fatalf("byte ceiling %d: %v", limit, err)
		}
	}
}

func TestStructuredToolGroupIsNeverSplit(t *testing.T) {
	history := []model.Message{}
	for range 6 {
		history = append(history, model.Message{Role: "user", Content: "head"})
	}
	history = append(history,
		model.Message{Role: "user", Content: "old exchange"},
		model.Message{Role: "assistant", Content: "PAIR_CALL", ToolCalls: []model.ToolCall{{ID: "call1", Name: "read_file"}}},
		model.Message{Role: "tool", Content: "PAIR_RESULT" + strings.Repeat("large ", 2000), ToolCallID: "call1"},
		model.Message{Role: "user", Content: "latest"})
	p, m, err := BuildChecked(context.Background(), Input{History: history, UserMessage: "latest", MaxTokens: 1000, OutputReserve: 100}, nil)
	if err != nil || !m.Reduced || strings.Contains(p, "PAIR_CALL") || strings.Contains(p, "PAIR_RESULT") {
		t.Fatalf("%+v %v", m, err)
	}
	history[8].ToolCallID = "orphan"
	if p, _, err = BuildChecked(context.Background(), Input{History: history}, nil); err == nil || p != "" {
		t.Fatal("orphan exchange accepted")
	}
}
