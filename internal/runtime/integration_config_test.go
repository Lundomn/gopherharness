package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/memory"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/provider"
)

func TestConfiguredExternalMemoryIsUsedAndScoped(t *testing.T) {
	var got memory.HTTPRecallRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-memory-key" {
			t.Error("missing credential")
		}
		switch r.URL.Path {
		case "/v1/recall":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(memory.HTTPRecallResponse{Repo: got.Repo, User: got.User, Memories: []memory.HTTPMemory{{ID: "f", Text: "project uses Go", Score: 1}}})
		case "/v1/store":
			var s memory.HTTPStoreRequest
			_ = json.NewDecoder(r.Body).Decode(&s)
			_ = json.NewEncoder(w).Encode(memory.HTTPStoreResponse{Repo: s.Repo, User: s.User, TurnID: s.TurnID, Durable: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	root := t.TempDir()
	cfg := config.Config{Memory: config.Memory{BaseURL: srv.URL, APIKey: "local-memory-key", TimeoutSeconds: 1}}
	a, e := New(Options{Root: root, Config: cfg, MemoryUser: "alice", Provider: &provider.Fake{Responses: []string{"<final>done</final>"}}, DisableAutoDream: true, HistorySelection: "rule"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if _, e = a.Ask(context.Background(), "stack?"); e != nil {
		t.Fatal(e)
	}
	if got.User != "alice" || got.Repo != a.Workspace.Root || got.TopK != 5 {
		t.Fatalf("%+v", got)
	}
	if !a.ownsFacts || a.Redactor.Text("local-memory-key") == "local-memory-key" {
		t.Fatal("backend ownership/redaction missing")
	}
}

func TestRuntimeBudgetSettlesNativeAttemptAndDeniesBeforeCall(t *testing.T) {
	for _, limit := range []int64{1, 1_000_000_000} {
		calls := 0
		c := &scriptedChat{call: func(context.Context, provider.ChatRequest) (provider.Completion, error) {
			calls++
			return provider.Completion{Text: "done", ActualModel: "test-native-model", Usage: provider.Usage{InputTokens: 10, OutputTokens: 5, Complete: true}}, nil
		}}
		a, e := New(Options{Root: t.TempDir(), Provider: c, CostLimitNanoUSD: limit, Prices: map[string]provider.Price{"test-native-model": {Input: 1, Output: 2, CacheRead: 1, CacheWrite: 1}}, DisableAutoDream: true, HistorySelection: "rule"})
		if e != nil {
			t.Fatal(e)
		}
		_, e = a.Ask(context.Background(), "hi")
		snapshot, se := a.CostBudget.Snapshot(context.Background())
		if se != nil {
			t.Fatal(se)
		}
		if limit == 1 {
			if !errors.Is(e, provider.ErrBudgetExhausted) || calls != 0 {
				t.Fatalf("calls=%d error=%v", calls, e)
			}
		} else if e != nil || calls != 1 || snapshot.SpentNanoUSD != 20_000 || snapshot.HeldNanoUSD != 0 {
			t.Fatalf("%+v calls=%d err=%v", snapshot, calls, e)
		}
		if e := a.Close(); e != nil {
			t.Fatal(e)
		}
	}
}

func TestQueuedAskCancellationDoesNotWaitForCurrentTurn(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	c := &scriptedChat{call: func(ctx context.Context, _ provider.ChatRequest) (provider.Completion, error) {
		close(entered)
		<-release
		return provider.Completion{Text: "done"}, nil
	}}
	a, e := New(Options{Root: t.TempDir(), Provider: c, DisableAutoDream: true, HistorySelection: "rule"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	done := make(chan error, 1)
	go func() { _, e := a.Ask(context.Background(), "first"); done <- e }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := a.Ask(ctx, "queued")
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}

func TestDreamAndResumeDoNotCrossUsers(t *testing.T) {
	root := t.TempDir()
	makeAgent := func(user string) *Agent {
		t.Helper()
		a, e := New(Options{Root: root, MemoryUser: user, Provider: &provider.Fake{}, DisableAutoDream: true, HistorySelection: "rule"})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = a.Close() })
		return a
	}
	alice, bob := makeAgent("alice"), makeAgent("bob")
	alice.Session.Messages = []model.Message{{Role: "user", Content: "alice private fact"}}
	if e := alice.saveSession(); e != nil {
		t.Fatal(e)
	}
	bob.Session.Messages = []model.Message{{Role: "user", Content: "bob private fact"}}
	if e := bob.saveSession(); e != nil {
		t.Fatal(e)
	}
	text := alice.recentSessionText(5, 24000)
	if !strings.Contains(text, "alice private") || strings.Contains(text, "bob private") {
		t.Fatal(text)
	}
	if _, e := alice.Memory.FinishDream("private alice topic", 1); e != nil {
		t.Fatal(e)
	}
	if !bob.Memory.LastDreamAt().IsZero() {
		t.Fatal("shared dream watermark")
	}
	if _, e := os.Stat(filepath.Join(root, ".pico", "memory", "topics", "dream.md")); !os.IsNotExist(e) {
		t.Fatal("user topic leaked to global path")
	}
	wrong, e := New(Options{Root: root, MemoryUser: "bob", Resume: alice.Session.ID, Provider: &provider.Fake{}})
	if wrong != nil {
		wrong.Close()
	}
	if e == nil {
		t.Fatal("cross-user resume accepted")
	}
}

func TestWorkerWritesRequireExplicitParentAuthority(t *testing.T) {
	bad, e := New(Options{Root: t.TempDir(), Provider: &provider.Fake{}, Approval: permission.Ask, AllowWorkerWrites: true})
	if bad != nil {
		bad.Close()
	}
	if e == nil {
		t.Fatal("worker bypassed parent approval")
	}
	for _, allowed := range []bool{false, true} {
		root := t.TempDir()
		c := &provider.Fake{Responses: []string{`<tool>{"name":"write_file","args":{"path":"child.txt","content":"child"}}</tool>`, `<final>done</final>`}}
		a, e := New(Options{Root: root, Provider: c, Approval: permission.Auto, AllowWorkerWrites: allowed, HistorySelection: "rule", DisableAutoDream: true})
		if e != nil {
			t.Fatal(e)
		}
		_, e = a.runWorker(context.Background(), "implement", "write child", nil)
		if e != nil {
			t.Fatal(e)
		}
		_, stat := os.Stat(filepath.Join(root, "child.txt"))
		if allowed && stat != nil {
			t.Fatal(stat)
		}
		if !allowed && !os.IsNotExist(stat) {
			t.Fatal("ungranted child wrote")
		}
		if e = a.Close(); e != nil {
			t.Fatal(e)
		}
	}
}
