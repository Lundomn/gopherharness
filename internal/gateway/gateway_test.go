package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/delivery"
	"github.com/Lundomn/gopherharness/internal/journal"
	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

type fakeOutlet struct{ messages chan delivery.Message }

func (f fakeOutlet) Send(_ context.Context, m delivery.Message) (delivery.Receipt, error) {
	f.messages <- m
	return delivery.Receipt{MessageID: "message"}, nil
}

func TestReplayUsesSameCapacityAndDurableTrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	j, err := journal.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	const total = 12
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("event-%d", i)
		e := Event{ID: id, Identity: tracing.Identity{TraceID: fmt.Sprintf("trace-%d", i)}, Request: scheduler.Request{Conversation: id, Text: id, Source: scheduler.Source{Channel: "test", ChatID: "group", SenderID: "allowed"}}}
		if _, err := j.Put(ctx, "inbox", "test:"+id, e); err != nil {
			t.Fatal(err)
		}
	}
	var active, peak atomic.Int32
	s := scheduler.New(func(ctx context.Context, r scheduler.Request, _ func() []scheduler.Request) (string, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		if tracing.Current(ctx).TraceID == "" {
			t.Error("replayed request lost trace")
		}
		time.Sleep(time.Millisecond)
		return r.Text, nil
	}, 8, 1, 50)
	hub := delivery.New(16, 0, 0, time.Second, nil)
	hub.Journal = j
	messages := make(chan delivery.Message, total)
	_ = hub.Register("test", fakeOutlet{messages})
	g := New(s, hub, func(scheduler.Source) bool { return true }, func(err error) { t.Error(err) })
	g.Journal = j
	g.slots = make(chan struct{}, 3)
	if err := g.Replay(ctx); err != nil {
		t.Fatal(err)
	}
	// Graceful completion is requested before cancellation.
	for i := 0; i < total; i++ {
		select {
		case m := <-messages:
			if m.Identity.TraceID == "" {
				t.Fatal("delivery trace lost")
			}
		case <-ctx.Done():
			t.Fatal("missing replay delivery")
		}
	}
	if err := g.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 3 {
		t.Fatalf("replay bypassed admission bound: %d", peak.Load())
	}
}

func TestReconcileRecoveredTextKeepsTrustedRoute(t *testing.T) {
	ctx := context.Background()
	j, err := journal.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	e := Event{ID: "event", Identity: tracing.Identity{TraceID: "trace"}, Request: scheduler.Request{Source: scheduler.Source{Channel: "test", ChatID: "trusted-chat", SenderID: "sender"}}}
	_, _ = j.Put(ctx, "inbox", "test:event", e)
	_ = j.Move(ctx, "inbox", "test:event", "pending", "needs_reconcile", nil)
	if err := Reconcile(ctx, j, "inbox", "test:event", journal.Resolution{Actor: "operator", Reason: "verified result", Evidence: "durable session", Action: "recover_result"}, "verified text"); err != nil {
		t.Fatal(err)
	}
	entries, _ := j.List(ctx, "outbox", "pending")
	var m delivery.Message
	if len(entries) != 1 {
		t.Fatal(entries)
	}
	if err := json.Unmarshal(entries[0].Payload, &m); err != nil {
		t.Fatal(err)
	}
	if m.Source.ChatID != "trusted-chat" || m.Identity.TraceID != "trace" || m.Text != "verified text" {
		t.Fatal(m)
	}
}
func TestIngressDedupAndReplyCoordinates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var calls atomic.Int32
	s := scheduler.New(func(_ context.Context, r scheduler.Request, _ func() []scheduler.Request) (string, error) {
		calls.Add(1)
		return "answer", nil
	}, 2, 1, 8)
	hub := delivery.New(8, 0, 0, time.Second, nil)
	messages := make(chan delivery.Message, 2)
	_ = hub.Register("test", fakeOutlet{messages})
	g := New(s, hub, func(source scheduler.Source) bool { return source.SenderID == "allowed" }, func(err error) { t.Error(err) })
	e := Event{ID: "event", Request: scheduler.Request{Conversation: "group", Text: "question", Source: scheduler.Source{Channel: "test", ChatID: "group-a", SenderID: "allowed"}}}
	if err := g.Publish(e); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(e); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-messages:
		if msg.Source.ChatID != "group-a" || msg.Text != "answer" {
			t.Fatal(msg)
		}
	case <-ctx.Done():
		t.Fatal("no reply")
	}
	if err := g.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate event reran agent")
	}
}

func TestCloseTimeoutStillSealsDeliveryAndJoinsEventually(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s := scheduler.New(func(context.Context, scheduler.Request, func() []scheduler.Request) (string, error) {
		close(started)
		<-release
		return "late", nil
	}, 1, 1, 8)
	hub := delivery.New(8, 0, 0, time.Second, nil)
	_ = hub.Register("test", fakeOutlet{make(chan delivery.Message, 1)})
	g := New(s, hub, func(scheduler.Source) bool { return true }, nil)
	err := g.Publish(Event{ID: "event", Request: scheduler.Request{Text: "q", Source: scheduler.Source{Channel: "test", ChatID: "chat"}}})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := hub.Post(context.Background(), delivery.Message{Source: scheduler.Source{Channel: "test", ChatID: "chat"}}); err == nil {
		t.Fatal("delivery still accepts messages after close timeout")
	}
	close(release)
	joined, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err := g.Close(joined); err != nil {
		t.Fatal(err)
	}
	if err := g.Publish(Event{ID: "later", Request: scheduler.Request{Text: "q"}}); err == nil {
		t.Fatal("gateway reopened")
	}
}
