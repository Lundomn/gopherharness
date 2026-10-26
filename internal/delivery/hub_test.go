package delivery

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

type outletFunc func(context.Context, Message) (Receipt, error)

func (f outletFunc) Send(ctx context.Context, m Message) (Receipt, error) { return f(ctx, m) }
func TestPerOutletIsolationRetryAndTrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	blocked := make(chan struct{})
	started := make(chan struct{})
	fast := make(chan struct{})
	var mu sync.Mutex
	var records []Record
	h := New(2, 2, 0, time.Second, func(r Record) { mu.Lock(); records = append(records, r); mu.Unlock() })
	tries := 0
	_ = h.Register("slow", outletFunc(func(ctx context.Context, m Message) (Receipt, error) {
		tries++
		if tracing.Current(ctx).TraceID != "trace-a" {
			t.Error("trace association lost")
		}
		if tries == 1 {
			close(started)
			<-blocked
			return Receipt{}, RejectedError{errors.New("temporary rejection before send")}
		}
		return Receipt{MessageID: "a"}, nil
	}))
	_ = h.Register("fast", outletFunc(func(context.Context, Message) (Receipt, error) { close(fast); return Receipt{MessageID: "b"}, nil }))
	if err := h.Post(ctx, Message{Source: scheduler.Source{Channel: "slow", ChatID: "group-a"}, Identity: tracing.Identity{TraceID: "trace-a"}}); err != nil {
		t.Fatal(err)
	}
	<-started
	_ = h.Post(ctx, Message{Source: scheduler.Source{Channel: "fast", ChatID: "group-b"}})
	select {
	case <-fast:
	case <-ctx.Done():
		t.Fatal("slow outlet blocked other channel")
	}
	close(blocked)
	if err := h.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(records) != 2 || tries != 2 {
		t.Fatal(records, tries)
	}
}
func TestTerminalFailureNotRetriedAndNoOutletRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	h := New(1, 3, 0, time.Second, nil)
	_ = h.Register("x", outletFunc(func(context.Context, Message) (Receipt, error) {
		calls++
		return Receipt{}, TerminalError{errors.New("denied")}
	}))
	if err := h.Post(ctx, Message{Source: scheduler.Source{Channel: "absent", ChatID: "g"}}); err == nil {
		t.Fatal("missing outlet accepted")
	}
	_ = h.Post(ctx, Message{Source: scheduler.Source{Channel: "x", ChatID: "g"}})
	_ = h.WaitIdle(ctx)
	if calls != 1 {
		t.Fatal("terminal error retried")
	}
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownNonIdempotentOutcomeIsNotRetriedWithoutJournal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var calls int
	records := make(chan Record, 1)
	h := New(1, 3, 0, time.Second, func(r Record) { records <- r })
	_ = h.Register("x", outletFunc(func(context.Context, Message) (Receipt, error) {
		calls++
		return Receipt{}, errors.New("response lost after send")
	}))
	_ = h.Post(ctx, Message{ID: "stable", Source: scheduler.Source{Channel: "x", ChatID: "chat"}})
	if err := h.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-records
	if calls != 1 || r.Outcome != "unknown" {
		t.Fatal(calls, r)
	}
}

func TestOutletPanicSettlesPendingWithoutClaimingFailureOrSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	records := make(chan Record, 1)
	h := New(1, 3, 0, time.Second, func(r Record) { records <- r })
	_ = h.Register("x", outletFunc(func(context.Context, Message) (Receipt, error) { panic("adapter bug") }))
	_ = h.Post(ctx, Message{ID: "id", Source: scheduler.Source{Channel: "x", ChatID: "chat"}})
	if err := h.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-records; r.Outcome != "unknown" || r.Attempts != 1 {
		t.Fatal(r)
	}
}
