package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/contextbuilder"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/provider"
)

func TestContextPolicyProfilesOverridesAndFallback(t *testing.T) {
	p := config.Provider{ContextWindowTokens: 32768, MaxOutputTokens: 4096, Tokenizer: "o200k_base"}
	opts := Options{Provider: provider.NewOpenAI(p, 1)}
	if err := resolveContextPolicy(&opts); err != nil {
		t.Fatal(err)
	}
	if opts.ContextWindowTokens != 32768 || opts.MaxNewTokens != 4096 || opts.TokenCounter.Name() != "o200k_base_estimate" {
		t.Fatalf("%+v", opts)
	}
	opts = Options{Provider: provider.NewOpenAI(config.Provider{}, 1), ContextWindowTokens: 48000, MaxNewTokens: 2048, Tokenizer: "o200k_base", FallbackClients: []provider.Client{provider.NewOpenAI(config.Provider{ContextWindowTokens: 16000, Tokenizer: "cl100k_base"}, 1)}}
	if err := resolveContextPolicy(&opts); err != nil {
		t.Fatal(err)
	}
	if opts.ContextWindowTokens != 16000 || !strings.Contains(opts.TokenCounter.Name(), "max(") {
		t.Fatalf("%+v", opts)
	}
	for _, bad := range []Options{{ContextWindowTokens: 100}, {Tokenizer: "unknown"}, {PromptBudget: -1}, {HistorySelection: "unknown"}, {FallbackClients: []provider.Client{provider.NewOpenAI(config.Provider{ContextWindowTokens: 4000}, 1)}}} {
		if err := resolveContextPolicy(&bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestRuntimeUsesTokenBudgetBeforeModelCall(t *testing.T) {
	fake := &provider.Fake{Responses: []string{"<final>done</final>"}}
	a, err := New(Options{Root: t.TempDir(), Provider: fake, ContextWindowTokens: 1000, MaxNewTokens: 100, DisableAutoDream: true, HistorySelection: "rule"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, err = a.Ask(context.Background(), strings.Repeat("独立约束。", 2000))
	if !errors.Is(err, contextbuilder.ErrBudget) || fake.Index != 0 {
		t.Fatalf("err=%v calls=%d", err, fake.Index)
	}
}

func TestDefaultModelScoringIsMeteredAndRuleModeSkipsIt(t *testing.T) {
	for _, mode := range []string{"model", "rule"} {
		responses := []string{"<final>done</final>"}
		if mode == "model" {
			responses = append([]string{
				`{"tool":"search","query":"history"}`,
				`{"tool":"score","scores":[{"id":6,"score":0.9}]}`,
				`{"tool":"state","state":{"goals":["finish requested change"],"open_threads":[],"decisions":["preserve the existing API"]}}`,
				`{"tool":"finish","ids":[6,8]}`,
			}, responses...)
		}
		fake := &provider.Fake{Responses: responses}
		a, err := New(Options{Root: t.TempDir(), Provider: fake, ContextWindowTokens: 6000, MaxNewTokens: 100, DisableAutoDream: true, HistorySelection: mode})
		if err != nil {
			t.Fatal(err)
		}
		for range 8 {
			a.Session.Messages = append(a.Session.Messages, model.Message{Role: "user", Content: strings.Repeat("history ", 500)})
		}
		answer, err := a.Ask(context.Background(), "finish")
		if err != nil || answer != "done" || fake.Index != len(responses) {
			t.Fatalf("mode=%s answer=%s err=%v calls=%d", mode, answer, err, fake.Index)
		}
		if err = a.Close(); err != nil {
			t.Fatal(err)
		}
		if got := a.CallLedger.Health().Accepted; got != uint64(len(responses)) {
			t.Fatalf("unmetered call: %d", got)
		}
	}
}
