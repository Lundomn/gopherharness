// Package gateway receives normalized, authenticated channel events. Platform
// authentication belongs to the adapter, not model-generated message content.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/Lundomn/gopherharness/internal/delivery"
	"github.com/Lundomn/gopherharness/internal/journal"
	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

type Event struct {
	ID       string
	Request  scheduler.Request
	Identity tracing.Identity // persisted across inbox replay and outbox delivery
}
type Gateway struct {
	Journal   *journal.Journal // Startup-only; adapter must call Replay before intake.
	Scheduler *scheduler.Scheduler
	Hub       *delivery.Hub
	Allowed   func(scheduler.Source) bool
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	seen      map[string]bool
	closed    bool
	wg        sync.WaitGroup
	slots     chan struct{}
	onError   func(error)
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func New(s *scheduler.Scheduler, hub *delivery.Hub, allowed func(scheduler.Source) bool, onError func(error)) *Gateway {
	ctx, cancel := context.WithCancel(context.Background())
	return &Gateway{Scheduler: s, Hub: hub, Allowed: allowed, ctx: ctx, cancel: cancel, seen: map[string]bool{}, slots: make(chan struct{}, 128), onError: onError, closeDone: make(chan struct{})}
}
func (g *Gateway) Publish(event Event) error {
	return g.publish(g.ctx, event, false)
}
func (g *Gateway) publish(ctx context.Context, event Event, wait bool) error {
	if g.Allowed == nil || !g.Allowed(event.Request.Source) {
		return errors.New("sender not allowed")
	}
	if event.ID == "" {
		return errors.New("platform event id required")
	}
	if event.Request.Source.Channel == "" || event.Request.Source.ChatID == "" || strings.TrimSpace(event.Request.Text) == "" {
		return errors.New("channel, reply target and non-empty text required")
	}
	key := event.Request.Source.Channel + ":" + event.ID
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return errors.New("gateway closed")
	}
	if g.seen[key] {
		return nil
	}
	// Bound dedup memory without silently evicting accepted IDs. Restart clears it.
	if len(g.seen) >= 100000 {
		return errors.New("dedup capacity exhausted")
	}
	select {
	case g.slots <- struct{}{}:
	default:
		if !wait {
			return errors.New("gateway overloaded")
		}
		// Startup replay obeys the same capacity bound without failing simply
		// because more than one batch was durably accepted before restart.
		g.mu.Unlock()
		select {
		case g.slots <- struct{}{}:
			g.mu.Lock()
			if g.closed || g.seen[key] {
				<-g.slots
				if g.closed {
					return errors.New("gateway closed")
				}
				return nil
			}
		case <-ctx.Done():
			g.mu.Lock()
			return ctx.Err()
		}
	}
	if event.Identity.TraceID == "" {
		traceCtx, end := tracing.New(nil).Start(g.ctx, "gateway.request", true)
		event.Identity = tracing.Current(traceCtx)
		end(nil)
	}
	if g.Journal != nil {
		created, err := g.Journal.Put(g.ctx, "inbox", key, event)
		if err != nil {
			<-g.slots
			return err
		}
		if !created {
			entries, e := g.Journal.List(g.ctx, "inbox", "pending")
			if e != nil {
				<-g.slots
				return e
			}
			pending := false
			for _, entry := range entries {
				if entry.ID == key {
					if e = json.Unmarshal(entry.Payload, &event); e != nil {
						<-g.slots
						return e
					}
					pending = true
					break
				}
			}
			if !pending {
				<-g.slots
				return nil
			}
			if !g.Allowed(event.Request.Source) {
				<-g.slots
				return errors.New("persisted sender is no longer allowed")
			}
		}
	}
	id := event.Identity
	if id.TraceID == "" { // Compatibility for inbox entries written before trace persistence.
		traceCtx, end := tracing.New(nil).Start(g.ctx, "gateway.replay", true)
		id = tracing.Current(traceCtx)
		end(nil)
	}
	// Only carry the trace here; the runtime records the actual turn span.
	id.SpanID = ""
	requestCtx := tracing.Attach(g.ctx, id)
	if g.Journal != nil {
		if err := g.Journal.Move(g.ctx, "inbox", key, "pending", "running", nil); err != nil {
			<-g.slots
			return err
		}
	}
	handle, err := g.Scheduler.Submit(requestCtx, event.Request)
	if err != nil {
		if g.Journal != nil {
			_ = g.Journal.Move(g.ctx, "inbox", key, "running", "pending", nil)
		}
		<-g.slots
		return err
	}
	g.seen[key] = true
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer func() { <-g.slots }()
		text, err := handle.Wait(g.ctx)
		if err != nil {
			if g.Journal != nil {
				_ = g.Journal.Move(context.Background(), "inbox", key, "running", "needs_reconcile", map[string]string{"error": err.Error()})
			}
			if g.onError != nil {
				g.onError(err)
			}
			return
		}
		if handle.Merged() {
			if g.Journal != nil {
				_ = g.Journal.Move(context.Background(), "inbox", key, "running", "merged", nil)
			}
			return
		}
		// Delivery owns retries of this existing result, never reruns the turn.
		message := delivery.Message{ID: key, Source: event.Request.Source, Text: text, Identity: id}
		if g.Journal != nil {
			if err = g.Journal.Complete(g.ctx, key, message); err != nil {
				if g.onError != nil {
					g.onError(err)
				}
				return
			}
		}
		err = g.Hub.Post(g.ctx, message)
		if err != nil && g.onError != nil {
			g.onError(err)
		}
	}()
	return nil
}

// Replay only requeues known-unstarted work. Running/unknown records require
// explicit reconciliation because external writes cannot be undone by a lock.
func (g *Gateway) Replay(ctx context.Context) error {
	if g.Journal == nil {
		return nil
	}
	entries, err := g.Journal.List(ctx, "outbox", "pending")
	if err != nil {
		return err
	}
	for _, e := range entries {
		var m delivery.Message
		if err = json.Unmarshal(e.Payload, &m); err != nil {
			return err
		}
		if err = g.Hub.Post(ctx, m); err != nil {
			return err
		}
	}
	entries, err = g.Journal.List(ctx, "inbox", "pending")
	if err != nil {
		return err
	}
	for _, e := range entries {
		var event Event
		if err = json.Unmarshal(e.Payload, &event); err != nil {
			return err
		}
		// Publish accepts a fresh receipt; remove no records. Requeue through a
		// dedicated claim path by marking the existing record ready for retry.
		if err = g.publish(ctx, event, true); err != nil {
			return err
		}
	}
	return nil
}
func (g *Gateway) Close(ctx context.Context) error {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.Scheduler.Seal()
	g.closeOnce.Do(func() {
		go func() {
			// Keep ownership after a caller timeout: all accepted work must join
			// before resources such as the journal can be released by the host.
			g.closeErr = g.Scheduler.Close(context.Background())
			g.wg.Wait()
			g.closeErr = errors.Join(g.closeErr, g.Hub.Close(context.Background()))
			g.cancel()
			close(g.closeDone)
		}()
	})
	select {
	case <-g.closeDone:
		return g.closeErr
	case <-ctx.Done():
		g.cancel()
		// Do not skip delivery cleanup when execution refuses to stop promptly.
		return errors.Join(ctx.Err(), g.Hub.Close(ctx))
	}
}
