package provider

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
)

type budgetFake struct {
	run   func(context.Context) (Completion, error)
	model string
}

func (f *budgetFake) Name() string { return "budget-test" }
func (f *budgetFake) Model() string {
	if f.model != "" {
		return f.model
	}
	return "model"
}
func (f *budgetFake) ContextPolicy() (int, int, string) { return 1000, 100, "cl100k_base" }
func (f *budgetFake) Complete(ctx context.Context, _ string, _ int) (Completion, error) {
	return f.run(ctx)
}
func (f *budgetFake) Stream(ctx context.Context, _ string, _ int, delta func(string) error) (Completion, error) {
	r, e := f.run(ctx)
	if e == nil {
		e = delta(r.Text)
	}
	return r, e
}
func (f *budgetFake) NativeTools() bool                                           { return true }
func (f *budgetFake) Chat(ctx context.Context, _ ChatRequest) (Completion, error) { return f.run(ctx) }
func (f *budgetFake) ChatStream(ctx context.Context, _ ChatRequest, delta func(string) error) (Completion, error) {
	r, e := f.run(ctx)
	if e == nil {
		e = delta(r.Text)
	}
	return r, e
}
func (f *budgetFake) InspectImage(ctx context.Context, _ string, _ []byte, _ string, _ int) (Completion, error) {
	return f.run(ctx)
}
func testPrice() map[string]FixedPrice {
	return map[string]FixedPrice{"model": {Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000}}
}
func openTestBudget(t *testing.T, limit int64) *BudgetAccount {
	t.Helper()
	a, err := OpenBudget(context.Background(), filepath.Join(t.TempDir(), "budget.json"), limit)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func budgetView(t *testing.T, a *BudgetAccount) BudgetSnapshot {
	t.Helper()
	s, e := a.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func settledCompletion() Completion {
	return Completion{Text: "done", ActualModel: "model", Usage: Usage{InputTokens: 10, OutputTokens: 5, Complete: true}}
}

func TestFixedPriceCurrencyAndCacheAccounting(t *testing.T) {
	for _, v := range []struct {
		usd  float64
		want int64
	}{{1, 1_000_000_000}, {0.1, 100_000_000}, {0.0000000001, 1}, {0, 0}} {
		got, err := MoneyFromUSD(v.usd)
		if err != nil || got != v.want {
			t.Fatal(v, got, err)
		}
	}
	for _, v := range []float64{-1, math.NaN(), math.Inf(1), 1e20} {
		if _, err := MoneyFromUSD(v); err == nil {
			t.Fatal("invalid money", v)
		}
	}
	prices, err := FixedPrices(map[string]Price{"model": {Input: .002, Output: .003, CacheRead: .001, CacheWrite: .004}})
	if err != nil {
		t.Fatal(err)
	}
	cost, err := prices["model"].cost(Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 20, CacheWriteTokens: 3, Complete: true})
	if err != nil || cost != 67 {
		t.Fatal(cost, err)
	}
	if _, err := prices["model"].cost(Usage{InputTokens: -1, Complete: true}); err == nil {
		t.Fatal("negative usage accepted")
	}
}

func TestBudgetConcurrentCallsCannotOverdrawAndSettleOnce(t *testing.T) {
	account := openTestBudget(t, 2000)
	gate := make(chan struct{})
	started := make(chan struct{}, 20)
	var count atomic.Int32
	fake := &budgetFake{run: func(ctx context.Context) (Completion, error) {
		if BudgetAttemptID(ctx) == "" {
			return Completion{}, errors.New("missing durable attempt ID")
		}
		count.Add(1)
		started <- struct{}{}
		<-gate
		return settledCompletion(), nil
	}}
	client, err := WrapBudget(fake, account, testPrice())
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 20)
	for range 20 {
		go func() { _, e := client.Complete(context.Background(), "q", 100); results <- e }()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("admission stalled")
		}
	}
	for range 18 {
		select {
		case e := <-results:
			if !errors.Is(e, ErrBudgetExhausted) {
				t.Fatal(e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("rejected calls stalled")
		}
	}
	view := budgetView(t, account)
	if view.HeldNanoUSD != 2000 || view.SpentNanoUSD != 0 || count.Load() != 2 {
		t.Fatal(view, count.Load())
	}
	close(gate)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	view = budgetView(t, account)
	if view.HeldNanoUSD != 0 || view.SpentNanoUSD != 30 || view.RemainingNanoUSD != 1970 {
		t.Fatal(view)
	}
}

func TestBudgetMissingUsageCancellationAndManualReconciliation(t *testing.T) {
	account := openTestBudget(t, 1500)
	ctx, cancel := context.WithCancel(context.Background())
	fake := &budgetFake{run: func(context.Context) (Completion, error) { cancel(); return Completion{}, context.Canceled }}
	client, _ := WrapBudget(fake, account, testPrice())
	if _, err := client.Complete(ctx, "q", 100); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	view := budgetView(t, account)
	if view.HeldNanoUSD != 1000 || view.SpentNanoUSD != 0 {
		t.Fatal(view)
	}
	var id string
	for key, r := range view.Attempts {
		id = key
		if r.State != "unknown" {
			t.Fatal(r)
		}
	}
	if _, err := client.Complete(context.Background(), "q", 100); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatal(err)
	}
	if err := account.Reconcile(context.Background(), id, 20, ""); err == nil {
		t.Fatal("missing explanation accepted")
	}
	if err := account.Reconcile(context.Background(), id, 20, "provider invoice confirmed 20 nano-USD"); err != nil {
		t.Fatal(err)
	}
	if err := account.Reconcile(context.Background(), id, 20, "repeat"); err != nil {
		t.Fatal(err)
	}
	if err := account.Reconcile(context.Background(), id, 0, "conflicting refund"); err == nil {
		t.Fatal("conflicting settlement accepted")
	}
	view = budgetView(t, account)
	if view.HeldNanoUSD != 0 || view.SpentNanoUSD != 20 {
		t.Fatal(view)
	}
}

func TestBudgetFrozenUnknownModelAndActualCostOverrun(t *testing.T) {
	account := openTestBudget(t, 2000)
	fake := &budgetFake{run: func(context.Context) (Completion, error) {
		r := settledCompletion()
		r.ActualModel = "unknown"
		return r, nil
	}}
	client, _ := WrapBudget(fake, account, testPrice())
	if _, err := client.Complete(context.Background(), "q", 100); !errors.Is(err, ErrBudgetAccounting) {
		t.Fatal(err)
	}
	view := budgetView(t, account)
	if !view.Frozen || view.HeldNanoUSD != 1000 {
		t.Fatal(view)
	}
	if _, err := client.Complete(context.Background(), "q", 100); !errors.Is(err, ErrBudgetAccounting) {
		t.Fatal(err)
	}
	for id := range view.Attempts {
		if err := account.Reconcile(context.Background(), id, 100, "confirmed invoice"); err != nil {
			t.Fatal(err)
		}
	}
	if budgetView(t, account).Frozen {
		t.Fatal("reconciled account still frozen")
	}
	fake.run = func(context.Context) (Completion, error) {
		return Completion{Usage: Usage{InputTokens: 3000, Complete: true}}, nil
	}
	if _, err := client.Complete(context.Background(), "q", 100); err != nil {
		t.Fatal(err)
	}
	view = budgetView(t, account)
	if view.SpentNanoUSD != 3100 || view.RemainingNanoUSD != 0 {
		t.Fatal("actual overrun hidden", view)
	}
	if _, err := client.Complete(context.Background(), "q", 100); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatal(err)
	}
}

func TestBudgetRecoveryDoesNotTouchActiveAttempt(t *testing.T) {
	account := openTestBudget(t, 2000)
	fake := &budgetFake{}
	r, err := account.reserve(context.Background(), fake, testPrice()["model"], 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer r.release()
	if err = r.sending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = account.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := budgetView(t, account)
	if view.Attempts[r.id].State != "sending" || view.HeldNanoUSD != 1000 {
		t.Fatal(view)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := account.Reconcile(ctx, r.id, 0, "not allowed while running"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestBudgetCrashHelper(t *testing.T) {
	path := os.Getenv("GOPHERHARNESS_BUDGET_CRASH_PATH")
	if path == "" {
		return
	}
	a, e := OpenBudget(context.Background(), path, 2000)
	if e != nil {
		os.Exit(2)
	}
	r, e := a.reserve(context.Background(), &budgetFake{}, testPrice()["model"], 1000)
	if e != nil {
		os.Exit(3)
	}
	if os.Getenv("GOPHERHARNESS_BUDGET_CRASH_STAGE") == "sending" {
		if e = r.sending(context.Background()); e != nil {
			os.Exit(4)
		}
	}
	os.Exit(0)
}
func TestBudgetRestartReleasesOnlyProvenUnsentReservations(t *testing.T) {
	for _, stage := range []string{"reserved", "sending"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "budget.json")
			cmd := exec.Command(os.Args[0], "-test.run=^TestBudgetCrashHelper$")
			cmd.Env = append(os.Environ(), "GOPHERHARNESS_BUDGET_CRASH_PATH="+path, "GOPHERHARNESS_BUDGET_CRASH_STAGE="+stage)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s %v", output, err)
			}
			a, err := OpenBudget(context.Background(), path, 2000)
			if err != nil {
				t.Fatal(err)
			}
			view := budgetView(t, a)
			want := "released"
			held := int64(0)
			if stage == "sending" {
				want = "unknown"
				held = 1000
			}
			if view.HeldNanoUSD != held || len(view.Attempts) != 1 {
				t.Fatal(view)
			}
			for _, r := range view.Attempts {
				if r.State != want {
					t.Fatal(r)
				}
			}
		})
	}
}

func TestBudgetWrapperPlacementAndPreflight(t *testing.T) {
	a := openTestBudget(t, 10000)
	fake := &budgetFake{run: func(context.Context) (Completion, error) {
		t.Fatal("provider unexpectedly called")
		return Completion{}, nil
	}}
	b, err := WrapBudget(fake, a, testPrice())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WrapBudget(b, a, testPrice()); !errors.Is(err, ErrNestedBudget) {
		t.Fatal(err)
	}
	if _, err := WrapBudget(Meter(b, nil, nil), a, testPrice()); !errors.Is(err, ErrNestedBudget) {
		t.Fatal(err)
	}
	if _, err := WrapBudget(&Resilient{Clients: []Client{fake}}, a, testPrice()); err == nil {
		t.Fatal("whole retry loop budget accepted")
	}
	if _, err := WrapBudget(fake, a, map[string]FixedPrice{}); err == nil {
		t.Fatal("unpriced model accepted")
	}
	for _, n := range []int{-1, 0, 101, 1000} {
		if _, err := b.Complete(context.Background(), "q", n); err == nil {
			t.Fatal("bad output limit accepted")
		}
	}
	if _, err := b.Chat(context.Background(), ChatRequest{Messages: []model.Message{{Role: "tool", ToolCallID: "orphan"}}, MaxOutputTokens: 100}); err == nil {
		t.Fatal("bad native request reserved")
	}
	if len(budgetView(t, a).Attempts) != 0 {
		t.Fatal("invalid local request consumed budget")
	}
}

func TestBudgetAllProviderInterfacesAndRetriesHaveIndependentAttempts(t *testing.T) {
	account := openTestBudget(t, 10000)
	var ids []string
	var mu sync.Mutex
	fake := &budgetFake{run: func(ctx context.Context) (Completion, error) {
		mu.Lock()
		ids = append(ids, BudgetAttemptID(ctx))
		mu.Unlock()
		return settledCompletion(), nil
	}}
	b, _ := WrapBudget(fake, account, testPrice())
	request := ChatRequest{Messages: []model.Message{{Role: "user", Content: "q"}}, MaxOutputTokens: 100}
	cb := func(string) error { return nil }
	calls := []func() (Completion, error){func() (Completion, error) { return b.Complete(context.Background(), "q", 100) }, func() (Completion, error) { return b.Stream(context.Background(), "q", 100, cb) }, func() (Completion, error) { return b.Chat(context.Background(), request) }, func() (Completion, error) { return b.ChatStream(context.Background(), request, cb) }, func() (Completion, error) {
		return b.InspectImage(context.Background(), "image/png", []byte{1}, "q", 100)
	}}
	for _, call := range calls {
		if _, err := call(); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			t.Fatal("duplicate attempt", ids)
		}
		seen[id] = true
	}
	if view := budgetView(t, account); len(view.Attempts) != 5 || view.SpentNanoUSD != 75 {
		t.Fatal(view)
	}
	tries := 0
	fake.run = func(ctx context.Context) (Completion, error) {
		tries++
		if tries == 1 {
			return Completion{}, &HTTPError{Status: 503}
		}
		return settledCompletion(), nil
	}
	retry := &Resilient{Clients: []Client{b}, Retries: 1, Delay: time.Millisecond}
	if _, err := retry.Complete(context.Background(), "q", 100); err != nil {
		t.Fatal(err)
	}
	view := budgetView(t, account)
	if len(view.Attempts) != 7 || view.SpentNanoUSD != 90 || view.HeldNanoUSD != 1000 {
		t.Fatal(view)
	}
}

func TestBudgetCorruptionAndLimitChangeFailClosed(t *testing.T) {
	account := openTestBudget(t, 1000)
	if _, err := OpenBudget(context.Background(), account.file.Path, 2000); err == nil {
		t.Fatal("silent limit increase")
	}
	if err := os.WriteFile(account.file.Path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &budgetFake{run: func(context.Context) (Completion, error) { t.Fatal("call on corrupt budget"); return Completion{}, nil }}
	b, _ := WrapBudget(fake, account, testPrice())
	if _, err := b.Complete(context.Background(), "q", 100); err == nil || strings.Contains(err.Error(), "exhausted") {
		t.Fatal(err)
	}
}

func TestBudgetSendingIsDurableBeforeProviderAndUsageOnErrorSettles(t *testing.T) {
	account := openTestBudget(t, 2000)
	fake := &budgetFake{run: func(ctx context.Context) (Completion, error) {
		id := BudgetAttemptID(ctx)
		view := budgetView(t, account)
		if view.Attempts[id].State != "sending" || view.HeldNanoUSD != 1000 {
			t.Fatal("provider called before sending claim", view)
		}
		return settledCompletion(), &HTTPError{Status: 503}
	}}
	b, _ := WrapBudget(fake, account, testPrice())
	if _, err := b.Complete(context.Background(), "q", 100); err == nil {
		t.Fatal("provider error swallowed")
	}
	view := budgetView(t, account)
	if view.SpentNanoUSD != 15 || view.HeldNanoUSD != 0 {
		t.Fatal(view)
	}
}

func TestBudgetCancelledBeforeSendCreatesNoAttempt(t *testing.T) {
	a := openTestBudget(t, 2000)
	fake := &budgetFake{run: func(context.Context) (Completion, error) {
		t.Fatal("cancelled call dispatched")
		return Completion{}, nil
	}}
	b, _ := WrapBudget(fake, a, testPrice())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Complete(ctx, "q", 100); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(budgetView(t, a).Attempts) != 0 {
		t.Fatal("cancelled preflight reserved funds")
	}
}

func TestBudgetCannotInitializeExistingInvalidJSONState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBudget(context.Background(), path, 1000); err == nil {
		t.Fatal("invalid existing account was reset")
	}
}
