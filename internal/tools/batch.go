package tools

import (
	"context"
	"fmt"
	"sync"

	"github.com/Lundomn/gopherharness/internal/model"
)

type Effect string

const (
	Unknown  Effect = "unknown"
	Read     Effect = "read"
	Write    Effect = "write"
	Execute  Effect = "execute"
	External Effect = "external"
)

type Capability struct {
	Effect          Effect
	ConcurrencySafe bool
}

func (r *Registry) Capability(name string) Capability {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.capabilities[name]; ok {
		return c
	}
	return Capability{Effect: Unknown}
}

// RunBatch overlaps only consecutive explicitly safe reads. Each bounded group
// is joined before the next group or side-effect barrier. Results stay in input
// order. Ordinary error results do not cancel siblings; cancellation does.
// Refreshable remote tools remain Unknown and are never promoted to safe reads
// by untrusted server annotations.
func (r *Registry) RunBatch(ctx context.Context, calls []model.ToolCall, limit int, run func(context.Context, model.ToolCall) model.ToolResult) []model.ToolResult {
	return r.RunIndexedBatch(ctx, calls, limit, func(ctx context.Context, _ int, call model.ToolCall) model.ToolResult { return run(ctx, call) })
}

func (r *Registry) RunIndexedBatch(ctx context.Context, calls []model.ToolCall, limit int, run func(context.Context, int, model.ToolCall) model.ToolResult) []model.ToolResult {
	if limit < 1 {
		limit = 1
	}
	results := make([]model.ToolResult, len(calls))
	for i := 0; i < len(calls); {
		if err := ctx.Err(); err != nil {
			for j := i; j < len(calls); j++ {
				results[j] = model.ToolResult{Name: calls[j].Name, Status: "error", Error: err.Error(), ErrorCode: "cancelled"}
			}
			break
		}
		end := i + 1
		if c := r.Capability(calls[i].Name); c.Effect == Read && c.ConcurrencySafe {
			for end < len(calls) && end-i < limit {
				c = r.Capability(calls[end].Name)
				if c.Effect != Read || !c.ConcurrencySafe {
					break
				}
				end++
			}
		}
		batchCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for j := i; j < end; j++ {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				defer func() {
					if p := recover(); p != nil {
						results[j] = model.ToolResult{Name: calls[j].Name, Status: "error", Error: fmt.Sprint(p), ErrorCode: "internal_panic"}
						cancel()
					}
				}()
				results[j] = run(batchCtx, j, calls[j])
				if results[j].ErrorCode == "cancelled" {
					cancel()
				}
			}(j)
		}
		wg.Wait()
		cancel()
		// A child cancellation aborts the remaining batch, not just its siblings.
		aborted := false
		for j := i; j < end; j++ {
			if results[j].ErrorCode == "cancelled" || results[j].ErrorCode == "internal_panic" {
				aborted = true
			}
		}
		if aborted {
			for j := end; j < len(calls); j++ {
				results[j] = model.ToolResult{Name: calls[j].Name, Status: "error", Error: "batch cancelled", ErrorCode: "cancelled"}
			}
			break
		}
		i = end
	}
	return results
}
