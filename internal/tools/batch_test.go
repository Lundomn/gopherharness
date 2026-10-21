package tools

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Lundomn/gopherharness/internal/model"
)

func TestReadBatchBoundBarrierAndInputOrder(t *testing.T) {
	r := NewRegistry()
	calls := []model.ToolCall{{Name: "read_file"}, {Name: "search"}, {Name: "write_file"}, {Name: "list_files"}}
	started := make(chan string, 4)
	release := make(chan struct{})
	done := make(chan []model.ToolResult, 1)
	var active atomic.Int32
	go func() {
		done <- r.RunBatch(context.Background(), calls, 2, func(_ context.Context, c model.ToolCall) model.ToolResult {
			if c.Name == "write_file" {
				if active.Load() != 0 {
					t.Error("write crossed read barrier")
				}
			} else {
				active.Add(1)
				defer active.Add(-1)
			}
			started <- c.Name
			if c.Name == "read_file" || c.Name == "search" {
				<-release
			}
			return model.ToolResult{Name: c.Name, Status: "ok"}
		})
	}()
	for range 2 {
		name := <-started
		if name != "read_file" && name != "search" {
			t.Fatalf("barrier crossed: %s", name)
		}
	}
	select {
	case name := <-started:
		t.Fatalf("unbounded execution: %s", name)
	default:
	}
	close(release)
	results := <-done
	for i, c := range calls {
		if results[i].Name != c.Name {
			t.Fatal("completion order leaked into results")
		}
	}
}

func TestCancellationJoinsReadsAndSkipsWrite(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 2)
	done := make(chan []model.ToolResult, 1)
	var exited atomic.Int32
	go func() {
		done <- r.RunBatch(ctx, []model.ToolCall{{Name: "read_file"}, {Name: "search"}, {Name: "write_file"}}, 2, func(ctx context.Context, c model.ToolCall) model.ToolResult {
			if c.Name == "write_file" {
				t.Error("write executed after cancellation")
			}
			started <- struct{}{}
			<-ctx.Done()
			exited.Add(1)
			return model.ToolResult{Name: c.Name, Status: "error", ErrorCode: "cancelled"}
		})
	}()
	<-started
	<-started
	cancel()
	results := <-done
	if exited.Load() != 2 || results[2].ErrorCode != "cancelled" {
		t.Fatal("cancellation returned before join")
	}
}

func TestOrdinaryFailureDoesNotCancelSiblingAndUnknownSerial(t *testing.T) {
	r := NewRegistry()
	results := r.RunBatch(context.Background(), []model.ToolCall{{Name: "read_file"}, {Name: "search"}, {Name: "unknown"}}, 4, func(ctx context.Context, c model.ToolCall) model.ToolResult {
		if ctx.Err() != nil {
			t.Error("ordinary failure cancelled batch")
		}
		return model.ToolResult{Name: c.Name, Status: "error", ErrorCode: "not_found"}
	})
	if len(results) != 3 || results[2].Name != "unknown" {
		t.Fatal(results)
	}
	if c := r.Capability("unknown"); c.ConcurrencySafe || c.Effect != Unknown {
		t.Fatal(c)
	}
}
