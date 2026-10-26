package memory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

type retryBackend struct {
	mu     sync.Mutex
	calls  int
	fail   int
	traces []tracing.Identity
	block  bool
}

func (*retryBackend) Recall(context.Context, Scope, string, int) ([]Fact, error) { return nil, nil }
func (*retryBackend) Close() error                                               { return nil }
func (b *retryBackend) Store(ctx context.Context, _ Scope, _ string, _ []Evidence) error {
	b.mu.Lock()
	b.calls++
	n := b.calls
	b.traces = append(b.traces, tracing.Current(ctx))
	b.mu.Unlock()
	if b.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if n <= b.fail {
		return errors.New("temporary backend failure")
	}
	return nil
}
func commitMemoryRun(t *testing.T, root, turn, session string) {
	t.Helper()
	data, _ := json.Marshal(model.RunReport{RunID: turn, SessionID: session, Status: model.StatusCompleted})
	if err := durable.Replace(filepath.Join(root, ".pico", "runs", turn, "report.json"), data); err != nil {
		t.Fatal(err)
	}
}
func waitMemoryDone(t *testing.T, w *Writer) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, err := w.Pending(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("pending memory did not settle")
}
func TestMemoryOutboxRequiresCommittedMainResultRetriesAndPreservesTrace(t *testing.T) {
	root := t.TempDir()
	b := &retryBackend{fail: 1}
	w, err := NewWriter(root, Scope{Repo: root, User: "u"}, b, WriterOptions{PollInterval: 5 * time.Millisecond, RetryBase: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithCancel(tracing.Attach(context.Background(), tracing.Identity{TraceID: "main-trace", SpanID: "turn"}))
	if err := w.Prepare(ctx, "run-one", "session-a", []Evidence{{ID: "one", Kind: "conversation", Text: "use Go"}}); err != nil {
		t.Fatal(err)
	}
	cancel()
	w.Notify()
	time.Sleep(15 * time.Millisecond)
	b.mu.Lock()
	before := b.calls
	b.mu.Unlock()
	if before != 0 {
		t.Fatal("stored before main result commit")
	}
	commitMemoryRun(t, root, "run-one", "session-a")
	w.Notify()
	waitMemoryDone(t, w)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls != 2 {
		t.Fatalf("calls=%d", b.calls)
	}
	for _, trace := range b.traces {
		if trace.TraceID != "main-trace" {
			t.Fatal(trace)
		}
	}
}
func TestMemoryOutboxRestartAndAcknowledgementAreIdempotent(t *testing.T) {
	root := t.TempDir()
	scope := Scope{Repo: root, User: "u"}
	b := &retryBackend{}
	w, err := NewWriter(root, scope, b, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Prepare(context.Background(), "run-restart", "session-a", nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	commitMemoryRun(t, root, "run-restart", "session-a")
	w, err = NewWriter(root, scope, b, WriterOptions{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	waitMemoryDone(t, w)
	if err := w.Prepare(context.Background(), "run-restart", "session-a", nil); err != nil {
		t.Fatal(err)
	}
	w.Notify()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls != 1 {
		t.Fatalf("calls=%d", b.calls)
	}
}
func TestMemoryOutboxCancelLeavesRetryableWork(t *testing.T) {
	root := t.TempDir()
	scope := Scope{Repo: root, User: "u"}
	b := &retryBackend{block: true}
	w, err := NewWriter(root, scope, b, WriterOptions{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Prepare(context.Background(), "run-cancel", "session-a", nil); err != nil {
		t.Fatal(err)
	}
	commitMemoryRun(t, root, "run-cancel", "session-a")
	w.Notify()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		n := b.calls
		b.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Pending(context.Background()); err != nil || n != 1 {
		t.Fatalf("pending=%d err=%v", n, err)
	}
	other := &retryBackend{}
	w, err = NewWriter(root, scope, other, WriterOptions{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	waitMemoryDone(t, w)
}

func TestMemoryOutboxBoundsPermanentFailuresUntilExplicitRetry(t *testing.T) {
	root := t.TempDir()
	backend := &retryBackend{fail: 2}
	w, err := NewWriter(root, Scope{Repo: root, User: "u"}, backend, WriterOptions{PollInterval: time.Millisecond, RetryBase: time.Millisecond, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Prepare(context.Background(), "run-failed", "session-a", nil); err != nil {
		t.Fatal(err)
	}
	commitMemoryRun(t, root, "run-failed", "session-a")
	w.Notify()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, err := w.file.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if state.Jobs["run-failed"].Failed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	state, err := w.file.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !state.Jobs["run-failed"].Failed {
		t.Fatal("permanent failure not parked")
	}
	time.Sleep(10 * time.Millisecond)
	backend.mu.Lock()
	calls := backend.calls
	backend.mu.Unlock()
	if calls != 2 {
		t.Fatal("unbounded failed calls", calls)
	}
	if err := w.Retry(context.Background(), "run-failed"); err != nil {
		t.Fatal(err)
	}
	waitMemoryDone(t, w)
}
