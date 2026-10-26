package delivery

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/journal"
	"github.com/Lundomn/gopherharness/internal/scheduler"
)

func TestDurableResultSurvivesQueueFailureAndRejectsChangedRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	j, err := journal.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	h := New(1, 1, 0, time.Second, nil)
	h.Journal = j
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	m := Message{ID: "worker:run-1", Source: scheduler.Source{Channel: "x", ChatID: "trusted-chat", SenderID: "owner"}, Text: "already computed"}
	if err := h.PostDurable(ctx, m); err == nil {
		t.Fatal("closed queue accepted result")
	}
	if e, ok, err := j.Get(ctx, "outbox", m.ID); err != nil || !ok || e.State != "pending" {
		t.Fatalf("result not durable before queue admission: %+v %v %v", e, ok, err)
	}
	h2 := New(1, 1, 0, time.Second, nil)
	h2.Journal = j
	defer h2.Close(ctx)
	var calls atomic.Int32
	if err := h2.Register("x", outletFunc(func(ctx context.Context, got Message) (Receipt, error) {
		calls.Add(1)
		e, ok, err := j.Get(ctx, "outbox", got.ID)
		if err != nil || !ok || e.State != "sending" || got.Source != m.Source || got.Text != m.Text {
			t.Error("result was not persisted and claimed before send", e, err)
		}
		return Receipt{MessageID: "platform-receipt"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := h2.PostDurable(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := h2.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h2.PostDurable(ctx, m); err != nil {
		t.Fatal(err)
	}
	changed := m
	changed.Source.ChatID = "other-chat"
	if err := h2.PostDurable(ctx, changed); err == nil {
		t.Fatal("same result ID redirected to another chat")
	}
	if calls.Load() != 1 {
		t.Fatal("result was delivered more than once", calls.Load())
	}
}

func TestDurableUnknownResultIsNotReplayedBySummaryRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	j, err := journal.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	h := New(2, 3, 0, time.Second, nil)
	h.Journal = j
	defer h.Close(ctx)
	var calls atomic.Int32
	_ = h.Register("x", outletFunc(func(context.Context, Message) (Receipt, error) {
		calls.Add(1)
		return Receipt{}, errors.New("response lost after send")
	}))
	m := Message{ID: "worker:unknown", Source: scheduler.Source{Channel: "x", ChatID: "chat"}, Text: "complete"}
	if err := h.PostDurable(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := h.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.PostDurable(ctx, m); err != nil {
		t.Fatal(err)
	}
	if e, ok, err := j.Get(ctx, "outbox", m.ID); err != nil || !ok || e.State != "unknown" || calls.Load() != 1 {
		t.Fatalf("unknown send was retried: %+v %v calls=%d", e, err, calls.Load())
	}
}
