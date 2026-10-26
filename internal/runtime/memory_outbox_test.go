package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/memory"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

type commitObservingMemory struct {
	root      string
	started   chan struct{}
	cancelled chan struct{}
	trace     tracing.Identity
	evidence  []memory.Evidence
	err       error
}

func (*commitObservingMemory) Recall(context.Context, memory.Scope, string, int) ([]memory.Fact, error) {
	return nil, nil
}
func (*commitObservingMemory) Close() error { return nil }
func (b *commitObservingMemory) Store(ctx context.Context, _ memory.Scope, turn string, evidence []memory.Evidence) error {
	b.trace = tracing.Current(ctx)
	b.evidence = evidence
	data, err := os.ReadFile(filepath.Join(b.root, ".pico", "runs", turn, "report.json"))
	var report model.RunReport
	if err == nil {
		err = json.Unmarshal(data, &report)
	}
	if err == nil && report.Status != model.StatusCompleted {
		err = context.Canceled
	}
	b.err = err
	close(b.started)
	<-ctx.Done()
	close(b.cancelled)
	return ctx.Err()
}
func TestCompletedAnswerDoesNotWaitForMemoryAndShutdownLeavesDurableRetry(t *testing.T) {
	root := t.TempDir()
	backend := &commitObservingMemory{root: root, started: make(chan struct{}), cancelled: make(chan struct{})}
	a, err := New(Options{Root: root, Provider: &provider.Fake{Responses: []string{"<final>done sk-abcdefghijklmnop</final>"}}, FactBackend: backend, DisableAutoDream: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx, cancel := context.WithCancel(tracing.Attach(context.Background(), tracing.Identity{TraceID: "origin-trace", SpanID: "origin-span"}))
	answer, err := a.Ask(ctx, "finish")
	if err != nil || !strings.Contains(answer, "done") {
		t.Fatalf("%s %v", answer, err)
	}
	select {
	case <-backend.started:
	case <-time.After(3 * time.Second):
		t.Fatal("memory write not submitted")
	}
	if backend.err != nil {
		t.Fatal("memory ran before committed result", backend.err)
	}
	if backend.trace.TraceID != "origin-trace" {
		t.Fatal(backend.trace)
	}
	for _, e := range backend.evidence {
		if strings.Contains(e.Text, "sk-abcdefghijklmnop") {
			t.Fatal("secret entered outbox")
		}
	}
	cancel()
	select {
	case <-backend.cancelled:
		t.Fatal("old turn cancellation killed maintenance")
	case <-time.After(10 * time.Millisecond):
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-backend.cancelled:
	default:
		t.Fatal("writer not cancelled at Agent close")
	}
	if n, err := a.FactWriter.Pending(context.Background()); err != nil || n != 1 {
		t.Fatalf("pending=%d err=%v", n, err)
	}
}
