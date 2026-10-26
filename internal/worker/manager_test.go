package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/tracing"
)

func TestWorkerCompletes(t *testing.T) {
	m := New(func(_ context.Context, role, prompt string, _ <-chan string) (string, error) {
		return role + ":" + prompt, nil
	})
	id, err := m.Start("explore", "inspect repo")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, w := range m.List() {
			if w.ID == id && w.Status == "completed" {
				if w.Result != "explore:inspect repo" {
					t.Fatal(w.Result)
				}
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("worker did not complete")
}

func TestDefaultConcurrencyQueuesAndCancellationWaitsForRealExit(t *testing.T) {
	started := make(chan string, 5)
	finish := make(chan struct{})
	var running atomic.Int32
	m := New(func(ctx context.Context, _, prompt string, _ <-chan string) (string, error) {
		if running.Add(1) > 4 {
			t.Error("worker concurrency exceeded")
		}
		defer running.Add(-1)
		started <- prompt
		<-ctx.Done()
		<-finish
		return "", ctx.Err()
	})
	var ids []string
	for i := 0; i < 4; i++ {
		id, err := m.Start("explore", "active")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for i := 0; i < 4; i++ {
		<-started
	}
	queued, err := m.Start("explore", "queued")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := m.StopContext(ctx, ids[0]); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop=%v", err)
	}
	if w := m.List()[0]; w.Status != "stopping" || !w.FinishedAt.IsZero() {
		t.Fatalf("premature finish=%+v", w)
	}
	select {
	case <-started:
		t.Fatal("slot released before real exit")
	default:
	}
	if err := m.Stop(queued); err != nil {
		t.Fatal(err)
	}
	m.CancelAll()
	close(finish)
	m.Close()
	for _, w := range m.List() {
		if w.Status != "stopped" {
			t.Fatal(w.Status)
		}
	}
}

func TestRollingHourQuotaAndTurnLifetimeTrace(t *testing.T) {
	now := time.Now()
	seen := make(chan tracing.Identity, 31)
	m := NewWithOptions(func(ctx context.Context, _, _ string, _ <-chan string) (string, error) {
		seen <- tracing.Current(ctx)
		<-ctx.Done()
		return "", ctx.Err()
	}, Options{Now: func() time.Time { return now }})
	origin, cancel := context.WithCancel(tracing.Attach(context.Background(), tracing.Identity{TraceID: "trace-a", SpanID: "submit"}))
	first, err := m.StartContext(origin, "explore", "first")
	if err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got.TraceID != "trace-a" {
		t.Fatal(got)
	}
	cancel()
	if err := m.Send(first, "still alive"); err != nil {
		t.Fatal("turn cancellation leaked into manager lifetime", err)
	}
	for i := 1; i < 30; i++ {
		if _, err := m.Start("explore", "next"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Start("explore", "over limit"); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
	// New time is assigned only while holding the manager lock; runners also use it.
	m.mu.Lock()
	now = now.Add(time.Hour)
	m.mu.Unlock()
	if _, err := m.Start("explore", "new hour"); err != nil {
		t.Fatal(err)
	}
	m.Close()
}

func TestPanicResolvesWorkerAndReleasesCapacity(t *testing.T) {
	m := NewWithOptions(func(context.Context, string, string, <-chan string) (string, error) { panic("broken runner") }, Options{Concurrency: 1})
	_, _ = m.Start("explore", "panic")
	deadline := time.Now().Add(time.Second)
	for len(m.RunningIDs()) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if w := m.List()[0]; w.Status != "failed" || w.Error == "" {
		t.Fatal(w)
	}
	m.Close()
}

func TestWorkerReceivesMessage(t *testing.T) {
	received := make(chan string, 1)
	m := New(func(ctx context.Context, _, _ string, inbox <-chan string) (string, error) {
		select {
		case message := <-inbox:
			received <- message
			return message, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	id, err := m.Start("explore", "inspect repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(id, "also inspect tests"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "also inspect tests" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive message")
	}
}

func TestCloseCancelsWorkersAndRejectsNewWork(t *testing.T) {
	started := make(chan struct{})
	m := New(func(ctx context.Context, _, _ string, _ <-chan string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	id, err := m.Start("explore", "wait")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	m.Close()
	if err = m.Send(id, "late"); err == nil {
		t.Fatal("message accepted after close")
	}
	if _, err = m.Start("explore", "late"); err == nil {
		t.Fatal("worker started after close")
	}
	if workers := m.List(); len(workers) != 1 || workers[0].Status != "stopped" {
		t.Fatalf("workers=%#v", workers)
	}
}
