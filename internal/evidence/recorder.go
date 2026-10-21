package evidence

import (
	"context"
	"sync"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/redact"
	"github.com/Lundomn/gopherharness/internal/store"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

type Recorder struct {
	Run       *store.RunStore
	Sessions  *store.SessionStore
	SessionID string
	Redactor  *redact.Redactor
	mu        sync.Mutex
}

func (r *Recorder) EmitContext(ctx context.Context, kind string, payload map[string]any) error {
	copy := make(map[string]any, len(payload)+2)
	for k, v := range payload {
		copy[k] = v
	}
	id := tracing.Current(ctx)
	copy["trace_id"], copy["span_id"] = id.TraceID, id.SpanID
	return r.Emit(kind, copy)
}

func New(run *store.RunStore, sessions *store.SessionStore, sessionID string, redactor *redact.Redactor) *Recorder {
	if redactor == nil {
		redactor = redact.New()
	}
	return &Recorder{Run: run, Sessions: sessions, SessionID: sessionID, Redactor: redactor}
}

func (r *Recorder) Emit(kind string, payload map[string]any) error {
	event := model.TraceEvent{Type: kind, RunID: r.Run.RunID, SessionID: r.SessionID, Timestamp: time.Now().UTC(), Payload: r.Redactor.Map(payload)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.Run.AppendTrace(event); err != nil {
		return err
	}
	return r.Sessions.AppendEvent(r.SessionID, event)
}
