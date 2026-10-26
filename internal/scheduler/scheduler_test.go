package scheduler

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/tracing"
)

func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestQueueTraceIdentityIsExplicitAndIndependent(t *testing.T) {
	ctx := deadline(t)
	seen := make(chan tracing.Identity, 2)
	s := New(func(ctx context.Context, r Request, _ func() []Request) (string, error) {
		id := tracing.Current(ctx)
		if id != r.Trace {
			t.Error("queue envelope and execution trace differ")
		}
		seen <- id
		return r.Text, nil
	}, 2, 1, 8)
	defer s.Close(ctx)
	h1, err := s.Submit(tracing.Attach(ctx, tracing.Identity{TraceID: "trace-a", SpanID: "submit-a"}), Request{Conversation: "a", Text: "one"})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := s.Submit(ctx, Request{Conversation: "b", Text: "two", Trace: tracing.Identity{TraceID: "trace-b", SpanID: "submit-b"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []*Handle{h1, h2} {
		if _, err := h.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]bool{}
	for i := 0; i < 2; i++ {
		ids[(<-seen).TraceID] = true
	}
	if !ids["trace-a"] || !ids["trace-b"] {
		t.Fatal(ids)
	}
}

func TestForeignSenderCannotInjectOrInterruptActiveLane(t *testing.T) {
	ctx := deadline(t)
	started := make(chan struct{})
	s := New(func(ctx context.Context, _ Request, _ func() []Request) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}, 1, 1, 8)
	source := Source{Channel: "test", ChatID: "group", SenderID: "alice"}
	_, err := s.Submit(ctx, Request{Conversation: "fixed", Text: "first", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	for _, policy := range []Policy{Inject, Interrupt, Append} {
		source.SenderID = "mallory"
		if _, err := s.Submit(ctx, Request{Conversation: "fixed", Text: "attack", Source: source, Busy: policy}); err == nil {
			t.Fatal("accepted foreign sender", policy)
		}
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptWaitsForOldCleanupBeforeNextTurn(t *testing.T) {
	ctx := deadline(t)
	started := make(chan string, 2)
	cancelled := make(chan struct{})
	cleanup := make(chan struct{})
	s := New(func(ctx context.Context, r Request, _ func() []Request) (string, error) {
		started <- r.Text
		if r.Text == "old" {
			<-ctx.Done()
			close(cancelled)
			<-cleanup
			return "", ctx.Err()
		}
		return r.Text, nil
	}, 2, 1, 8)
	old, _ := s.Submit(ctx, Request{Conversation: "a", Text: "old"})
	<-started
	next, _ := s.Submit(ctx, Request{Conversation: "a", Text: "next", Busy: Interrupt})
	<-cancelled
	select {
	case <-started:
		t.Fatal("replacement overlapped cancelled runner cleanup")
	default:
	}
	close(cleanup)
	_, _ = old.Wait(ctx)
	if answer, err := next.Wait(ctx); err != nil || answer != "next" {
		t.Fatalf("%s %v", answer, err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSameConversationSerialAndDifferentConversationOverlap(t *testing.T) {
	ctx := deadline(t)
	started := make(chan string, 4)
	release := make(chan struct{})
	var active atomic.Int32
	s := New(func(ctx context.Context, r Request, _ func() []Request) (string, error) {
		if r.Conversation == "a" && active.Add(1) != 1 {
			t.Error("same conversation overlapped")
		}
		if r.Conversation == "a" {
			defer active.Add(-1)
		}
		started <- r.Text
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return r.Text, nil
	}, 2, 1, 8)
	t.Cleanup(func() { _ = s.Close(ctx) })
	h1, _ := s.Submit(ctx, Request{Conversation: "a", Text: "a1"})
	h2, _ := s.Submit(ctx, Request{Conversation: "a", Text: "a2"})
	h3, _ := s.Submit(ctx, Request{Conversation: "b", Text: "b"})
	for range 2 {
		select {
		case name := <-started:
			if name == "a2" {
				t.Fatal("a2 crossed a1")
			}
		case <-ctx.Done():
			t.Fatal("no cross-conversation overlap")
		}
	}
	close(release)
	for _, h := range []*Handle{h1, h2, h3} {
		if _, err := h.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInjectedMessageDrainedOnlyAtSafePointAndUnconsumedFallback(t *testing.T) {
	ctx := deadline(t)
	started := make(chan struct{})
	resume := make(chan struct{})
	injected := make(chan string, 1)
	s := New(func(_ context.Context, r Request, drain func() []Request) (string, error) {
		if r.Text == "first" {
			close(started)
			<-resume
			for _, r := range drain() {
				injected <- r.Text
			}
		}
		return r.Text, nil
	}, 1, 1, 8)
	h, _ := s.Submit(ctx, Request{Conversation: "a", Text: "first"})
	<-started
	i, _ := s.Submit(ctx, Request{Conversation: "a", Text: "follow", Busy: Inject})
	select {
	case <-injected:
		t.Fatal("injection mutated running work")
	default:
	}
	close(resume)
	if _, err := h.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if answer, err := i.Wait(ctx); err != nil || answer != "first" {
		t.Fatalf("injected=%s %v", answer, err)
	}
	if <-injected != "follow" {
		t.Fatal("missing injection")
	}
	_ = s.Close(ctx)
	ready := make(chan struct{})
	goOn := make(chan struct{})
	var calls atomic.Int32
	s = New(func(_ context.Context, r Request, _ func() []Request) (string, error) {
		if calls.Add(1) == 1 {
			close(ready)
			<-goOn
		}
		return r.Text, nil
	}, 1, 1, 8)
	h, _ = s.Submit(ctx, Request{Conversation: "b", Text: "first"})
	<-ready
	i, _ = s.Submit(ctx, Request{Conversation: "b", Text: "fallback", Busy: Inject})
	close(goOn)
	_, _ = h.Wait(ctx)
	if answer, err := i.Wait(ctx); err != nil || answer != "fallback" {
		t.Fatalf("fallback=%s %v", answer, err)
	}
	_ = s.Close(ctx)
}

func TestSeparatePoolsInterruptAndCloseSeal(t *testing.T) {
	ctx := deadline(t)
	started := make(chan string, 3)
	s := New(func(ctx context.Context, r Request, _ func() []Request) (string, error) {
		started <- r.Text
		if r.Text == "background" || r.Text == "old" {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return r.Text, nil
	}, 1, 1, 8)
	b, _ := s.Submit(ctx, Request{Conversation: "b", Text: "background", Background: true})
	<-started
	o, _ := s.Submit(ctx, Request{Conversation: "a", Text: "old"})
	<-started
	n, _ := s.Submit(ctx, Request{Conversation: "a", Text: "new", Busy: Interrupt})
	if _, err := o.Wait(ctx); err != context.Canceled {
		t.Fatalf("old=%v", err)
	}
	if text, err := n.Wait(ctx); err != nil || text != "new" {
		t.Fatalf("new=%s %v", text, err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Wait(ctx); err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := s.Submit(ctx, Request{Conversation: "a", Text: "late"}); err == nil {
		t.Fatal("accepted after close")
	}
}

func TestAcceptedHandlesAllResolveUnderConcurrentSubmission(t *testing.T) {
	ctx := deadline(t)
	s := New(func(_ context.Context, r Request, _ func() []Request) (string, error) { return r.Text, nil }, 4, 2, 2048)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, err := s.Submit(ctx, Request{Conversation: fmt.Sprint(i % 4), Text: fmt.Sprint(i)})
			if err != nil {
				t.Error(err)
				return
			}
			if text, err := h.Wait(ctx); err != nil || text != fmt.Sprint(i) {
				t.Errorf("result=%s %v", text, err)
			}
		}(i)
	}
	wg.Wait()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
