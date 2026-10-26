package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/governance"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/tracing"
	"github.com/Lundomn/gopherharness/internal/worker"
)

func TestWorkerResultIngestionIsDurableAndDeduplicated(t *testing.T) {
	a, err := New(Options{Root: t.TempDir(), Provider: &provider.Fake{Responses: []string{"<final>child result</final>", "<final>main received</final>"}}, HistorySelection: "rule", DisableAutoDream: true, FinalReadiness: governance.Off})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id, err := a.Workers.Start("explore", "inspect only")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		pending, e := a.Workers.PendingResults(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if len(pending) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err = a.Ask(ctx, "what did the child find?"); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range a.Session.Messages {
		if m.ID == "worker-result:"+id {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("receipt message count=%d", count)
	}
	// Emulate crash after saving the message but before acknowledging outbox.
	hash := sha256.Sum256([]byte(a.SessionID()))
	type receiptState struct {
		Session, Principal string
		Records            map[string]worker.Receipt
		Admissions         []time.Time
	}
	file := durable.File[receiptState]{Path: filepath.Join(a.Workspace.Root, ".pico", "workers", hex.EncodeToString(hash[:])+".json")}
	if err = file.Update(ctx, func(s *receiptState) error { r := s.Records[id]; r.Acknowledged = false; s.Records[id] = r; return nil }); err != nil {
		t.Fatal(err)
	}
	if n, err := a.ingestWorkerResults(ctx); err != nil || n != 0 {
		t.Fatalf("duplicate ingest=%d %v", n, err)
	}
	if p, _ := a.Workers.PendingResults(ctx); len(p) != 0 {
		t.Fatal("duplicate receipt was not acknowledged")
	}
}
func TestServiceWorkerSummaryRetryDoesNotRerunModel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var calls, summaryCalls, sinkCalls atomic.Int32
	p := &scriptedChat{call: func(ctx context.Context, r provider.ChatRequest) (provider.Completion, error) {
		calls.Add(1)
		for _, m := range r.Messages {
			if strings.HasPrefix(m.Content, "Summarize the newly completed") {
				if len(r.Tools) != 0 {
					return provider.Completion{}, errors.New("summary was allowed to execute tools")
				}
				summaryCalls.Add(1)
				return provider.Completion{Text: "child found the answer"}, nil
			}
		}
		return provider.Completion{Text: "done"}, nil
	}}
	s := NewService(Options{Root: t.TempDir(), Provider: p, HistorySelection: "rule", DisableAutoDream: true, FinalReadiness: governance.Off}, 1, 1)
	defer s.Close(context.Background())
	delivered := make(chan WorkerSummary, 1)
	s.SetWorkerSummarySink(func(ctx context.Context, summary WorkerSummary) error {
		if sinkCalls.Add(1) == 1 {
			return errors.New("temporary delivery unavailable")
		}
		delivered <- summary
		return nil
	})
	source := scheduler.Source{Channel: "test", ChatID: "group", SenderID: "alice"}
	h, err := s.Scheduler.Submit(ctx, scheduler.Request{Conversation: "chat", Source: source, Text: "main request"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	a := s.agents["chat"]
	s.mu.Unlock()
	trace := tracing.Identity{TraceID: "trace-worker", SpanID: "parent-span"}
	if _, err = a.Workers.StartContext(tracing.Attach(ctx, trace), "explore", "inspect"); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-delivered:
		if result.Source != source || result.Identity.TraceID != trace.TraceID || result.Text != "child found the answer" {
			t.Fatalf("summary=%+v", result)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if summaryCalls.Load() != 1 || calls.Load() != 3 || sinkCalls.Load() != 2 {
		t.Fatalf("calls=%d summaries=%d sink=%d", calls.Load(), summaryCalls.Load(), sinkCalls.Load())
	}
}
func TestServiceCancelledWorkerDoesNotWakeSummary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	var calls atomic.Int32
	p := &scriptedChat{call: func(ctx context.Context, r provider.ChatRequest) (provider.Completion, error) {
		if calls.Add(1) == 1 {
			return provider.Completion{Text: "main"}, nil
		}
		close(started)
		<-ctx.Done()
		return provider.Completion{}, ctx.Err()
	}}
	s := NewService(Options{Root: t.TempDir(), Provider: p, HistorySelection: "rule", DisableAutoDream: true, FinalReadiness: governance.Off}, 1, 1)
	defer s.Close(context.Background())
	h, err := s.Scheduler.Submit(ctx, scheduler.Request{Conversation: "chat", Text: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	a := s.agents["chat"]
	s.mu.Unlock()
	id, err := a.Workers.Start("explore", "wait")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	s.CancelConversation("chat")
	if err = a.Workers.StopContext(ctx, id); err != nil {
		t.Fatal(err)
	}
	if pending, _ := a.Workers.PendingResults(ctx); len(pending) != 0 {
		t.Fatal("cancelled worker left a wakeup")
	}
	if calls.Load() != 2 {
		t.Fatal("cancel spawned an unsolicited summary")
	}
}

func TestServiceReplaysPersistedSummaryWithoutConstructingAgent(t *testing.T) {
	root := t.TempDir()
	source := scheduler.Source{Channel: "test", ChatID: "group", SenderID: "alice"}
	ctx := context.Background()
	routes := durable.File[serviceRoutes]{Path: filepath.Join(root, ".pico", "service", "routes.json")}
	if err := routes.Update(ctx, func(s *serviceRoutes) error {
		s.Routes = map[string]serviceRoute{"conversation": {Session: "session", Source: source}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := summaryReceipt{Summary: WorkerSummary{Conversation: "conversation", SessionID: "session", RunID: "run-summary", Source: source, Text: "persisted answer"}, Status: "pending", CreatedAt: time.Now()}
	if err := saveSummary(ctx, root, r); err != nil {
		t.Fatal(err)
	}
	// No provider is configured. Constructing an Agent would fail; replay must
	// only use the pre-existing durable route and summary text.
	s := NewService(Options{Root: root}, 1, 1)
	defer s.Close(ctx)
	got := make(chan WorkerSummary, 1)
	s.SetWorkerSummarySink(func(_ context.Context, r WorkerSummary) error { got <- r; return nil })
	select {
	case r := <-got:
		if r.Text != "persisted answer" || r.RunID != "run-summary" {
			t.Fatal(r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not replay outbox")
	}
	s.mu.Lock()
	n := len(s.agents)
	s.mu.Unlock()
	if n != 0 {
		t.Fatal("delivery recovery constructed an Agent")
	}
}
