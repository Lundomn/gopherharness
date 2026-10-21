package tracing

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestConcurrentContextIsolationAndParentage(t *testing.T) {
	var mu sync.Mutex
	var records []Record
	tr := New(func(r Record) error { mu.Lock(); defer mu.Unlock(); records = append(records, r); return nil })
	parent := context.Background()
	a, endA := tr.Start(parent, "A", true)
	b, endB := tr.Start(parent, "B", true)
	if Current(a).TraceID == Current(b).TraceID || Current(parent) != (Identity{}) {
		t.Fatal("root contexts leaked")
	}
	var wg sync.WaitGroup
	for _, ctx := range []context.Context{a, b} {
		wg.Add(1)
		go func(ctx context.Context) {
			defer wg.Done()
			child, end := tr.Start(ctx, "tool", false)
			if Current(child).TraceID != Current(ctx).TraceID || Current(child).SpanID == Current(ctx).SpanID {
				t.Error("incorrect child identity")
			}
			end(context.Canceled)
			end(nil)
		}(ctx)
	}
	wg.Wait()
	endA(nil)
	endB(nil)
	if len(records) != 4 {
		t.Fatalf("records=%d", len(records))
	}
	for _, r := range records {
		if r.Name == "tool" && (r.ParentSpanID == "" || r.Outcome != "cancelled") {
			t.Fatalf("record=%+v", r)
		}
	}
}

func TestQueueAttachAndRecordingFailure(t *testing.T) {
	tr := New(func(Record) error { return errors.New("disk full") })
	a, endA := tr.Start(context.Background(), "A", true)
	b, endB := tr.Start(context.Background(), "B", true)
	for _, source := range []context.Context{a, b} {
		workerCtx := Attach(context.Background(), Current(source))
		child, end := tr.Start(workerCtx, "delivery", false)
		if Current(child).TraceID != Current(source).TraceID {
			t.Fatal("queue origin lost")
		}
		end(nil)
	}
	endA(nil)
	endB(nil)
	if tr.Lost() != 4 {
		t.Fatal("record failure not observable")
	}
}
