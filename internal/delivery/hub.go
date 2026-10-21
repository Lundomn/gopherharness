// Package delivery separates completed computation from channel submission.
// It provides process-local per-outlet queues, not durable exactly-once delivery.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"github.com/Lundomn/gopherharness/internal/journal"
	"sync"
	"time"

	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

type Message struct {
	ID       string
	Source   scheduler.Source
	Text     string
	Identity tracing.Identity
}
type Receipt struct{ MessageID string }
type Outlet interface {
	Send(context.Context, Message) (Receipt, error)
}
type IdempotentOutlet interface{ Idempotent() bool }
type TerminalError struct{ Err error }

func (e TerminalError) Error() string { return e.Err.Error() }
func (e TerminalError) Unwrap() error { return e.Err }

type Record struct {
	Message  Message
	Receipt  Receipt
	Outcome  string
	Attempts int
}
type endpoint struct {
	outlet Outlet
	queue  chan Message
}
type Hub struct {
	Journal           *journal.Journal // Set before Register/Post; owner closes after Hub joins.
	mu                sync.Mutex
	endpoints         map[string]*endpoint
	sealed            bool
	pending           int
	idle              chan struct{}
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	posts             sync.WaitGroup
	done              chan struct{}
	closeOnce         sync.Once
	capacity, retries int
	delay, timeout    time.Duration
	sink              func(Record)
}

func New(capacity, retries int, delay, timeout time.Duration, sink func(Record)) *Hub {
	if capacity < 1 || retries < 0 || delay < 0 || timeout <= 0 {
		panic("invalid delivery configuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	return &Hub{endpoints: map[string]*endpoint{}, ctx: ctx, cancel: cancel, capacity: capacity, retries: retries, delay: delay, timeout: timeout, sink: sink, idle: idle, done: make(chan struct{})}
}
func (h *Hub) Register(name string, outlet Outlet) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sealed {
		return errors.New("delivery closed")
	}
	if name == "" || outlet == nil {
		return errors.New("name and outlet required")
	}
	if h.endpoints[name] != nil {
		return fmt.Errorf("duplicate outlet %q", name)
	}
	e := &endpoint{outlet: outlet, queue: make(chan Message, h.capacity)}
	h.endpoints[name] = e
	h.wg.Add(1)
	go h.work(e)
	return nil
}
func (h *Hub) Post(ctx context.Context, msg Message) error {
	h.mu.Lock()
	if h.sealed {
		h.mu.Unlock()
		return errors.New("delivery closed")
	}
	e := h.endpoints[msg.Source.Channel]
	if e == nil {
		h.mu.Unlock()
		return fmt.Errorf("no outlet for %q", msg.Source.Channel)
	}
	if msg.Source.ChatID == "" {
		h.mu.Unlock()
		return errors.New("reply target required")
	}
	if h.pending == 0 {
		h.idle = make(chan struct{})
	}
	h.pending++
	h.posts.Add(1)
	if msg.Identity.TraceID == "" {
		msg.Identity = tracing.Current(ctx)
	}
	h.mu.Unlock()
	defer h.posts.Done()
	select {
	case <-ctx.Done():
		h.finish()
		return ctx.Err()
	case <-h.ctx.Done():
		h.finish()
		return errors.New("delivery closed")
	case e.queue <- msg:
		return nil
	}
}
func (h *Hub) finish() {
	h.mu.Lock()
	h.pending--
	if h.pending == 0 {
		close(h.idle)
	}
	h.mu.Unlock()
}
func (h *Hub) WaitIdle(ctx context.Context) error {
	h.mu.Lock()
	idle := h.idle
	h.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (h *Hub) work(e *endpoint) {
	defer h.wg.Done()
	for {
		select {
		case <-h.ctx.Done():
			return
		case msg := <-e.queue:
			h.send(e.outlet, msg)
			h.finish()
		}
	}
}
func (h *Hub) send(outlet Outlet, msg Message) {
	record := Record{Message: msg, Outcome: "dropped"}
	if h.Journal != nil && msg.ID != "" {
		if err := h.Journal.Move(h.ctx, "outbox", msg.ID, "pending", "sending", nil); err != nil {
			if h.sink != nil {
				h.sink(Record{Message: msg, Outcome: "journal_error"})
			}
			return
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.Journal.Move(ctx, "outbox", msg.ID, "sending", record.Outcome, record.Receipt); err != nil {
				record.Outcome = "journal_error"
			}
			if h.sink != nil {
				h.sink(record)
			}
		}()
	}
	for attempt := 0; attempt <= h.retries; attempt++ {
		ctx, cancel := context.WithTimeout(tracing.Attach(h.ctx, msg.Identity), h.timeout)
		receipt, err := outlet.Send(ctx, msg)
		cancel()
		record.Attempts++
		if err == nil {
			record.Receipt = receipt
			record.Outcome = "delivered"
			break
		}
		var terminal TerminalError
		if h.Journal != nil && msg.ID != "" && !errors.As(err, &terminal) {
			idem, ok := outlet.(IdempotentOutlet)
			if !ok || !idem.Idempotent() {
				record.Outcome = "unknown"
				break
			}
		}
		if errors.As(err, &terminal) || h.ctx.Err() != nil || attempt == h.retries {
			if !errors.As(err, &terminal) {
				record.Outcome = "unknown"
			}
			break
		}
		timer := time.NewTimer(h.delay * time.Duration(1<<min(attempt, 10)))
		select {
		case <-timer.C:
		case <-h.ctx.Done():
			timer.Stop()
			record.Outcome = "unknown"
			if h.sink != nil && (h.Journal == nil || msg.ID == "") {
				h.sink(record)
			}
			return
		}
	}
	if h.sink != nil && (h.Journal == nil || msg.ID == "") {
		h.sink(record)
	}
}

// Close seals admission and attempts to drain accepted messages before cancelling
// workers. The caller's deadline bounds waiting, not uncooperative remote work.
func (h *Hub) Close(ctx context.Context) error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.sealed = true
		h.mu.Unlock()
		go func() {
			_ = h.WaitIdle(ctx)
			h.cancel()
			h.posts.Wait()
			h.wg.Wait()
			// No producers or consumers remain. Release cancelled queued payloads
			// and settle their accounting without closing a channel under senders.
			for _, endpoint := range h.endpoints {
				draining := true
				for draining {
					select {
					case msg := <-endpoint.queue:
						if h.sink != nil {
							h.sink(Record{Message: msg, Outcome: "cancelled"})
						}
						h.finish()
					default:
						draining = false
					}
				}
			}
			close(h.done)
		}()
	})
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		h.cancel()
		return ctx.Err()
	}
}
