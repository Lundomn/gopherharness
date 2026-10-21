// Package tracing carries immutable trace identities explicitly through context.
// Recording is best-effort and has an independently observable health counter.
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type Identity struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
}

type Record struct {
	Identity
	ParentSpanID string    `json:"parent_span_id,omitempty"`
	Name         string    `json:"name"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	Outcome      string    `json:"outcome"`
}

type identityKey struct{}

func Current(ctx context.Context) Identity {
	id, _ := ctx.Value(identityKey{}).(Identity)
	return id
}

// Attach does not change the parent context. Cancellation follows ctx, not the
// originating request; a queue owner chooses the appropriate worker lifetime.
func Attach(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

func newID(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("trace identity entropy unavailable: %v", err))
	}
	return hex.EncodeToString(b)
}

type Sink func(Record) error
type Tracer struct {
	sink Sink
	lost atomic.Uint64
}

func New(sink Sink) *Tracer    { return &Tracer{sink: sink} }
func (t *Tracer) Lost() uint64 { return t.lost.Load() }

// Start creates a child unless root explicitly requests a new trace.
// The returned end function is idempotent. It must be called on every path.
func (t *Tracer) Start(ctx context.Context, name string, root bool) (context.Context, func(error)) {
	parent := Current(ctx)
	id := Identity{TraceID: parent.TraceID, SpanID: newID(8)}
	if root || id.TraceID == "" {
		id.TraceID = newID(16)
		parent = Identity{}
	}
	started := time.Now().UTC()
	var once sync.Once
	return Attach(ctx, id), func(err error) {
		once.Do(func() {
			outcome := "completed"
			switch {
			case errors.Is(err, context.Canceled):
				outcome = "cancelled"
			case errors.Is(err, context.DeadlineExceeded):
				outcome = "timed_out"
			case err != nil:
				outcome = "failed"
			}
			if t != nil && t.sink != nil {
				if t.sink(Record{Identity: id, ParentSpanID: parent.SpanID, Name: name, StartedAt: started, FinishedAt: time.Now().UTC(), Outcome: outcome}) != nil {
					t.lost.Add(1)
				}
			}
		})
	}
}
