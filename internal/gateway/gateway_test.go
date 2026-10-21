package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/delivery"
	"github.com/Lundomn/gopherharness/internal/scheduler"
)

type fakeOutlet struct{ messages chan delivery.Message }

func (f fakeOutlet) Send(_ context.Context, m delivery.Message) (delivery.Receipt, error) {
	f.messages <- m
	return delivery.Receipt{MessageID: "message"}, nil
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
