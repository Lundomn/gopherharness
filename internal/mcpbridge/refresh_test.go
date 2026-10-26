package mcpbridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func textResult(text string) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
}

func TestTrustedRefreshPublishesCompleteRevocationsWithoutTrustingHints(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "refresh", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "old", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return textResult("old") })
	h := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := tools.NewRegistry()
	m, err := Connect(ctx, r, []Server{{Name: "test", Transport: "streamableHttp", URL: h.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	catalog := r.Snapshot()
	s.RemoveTools("old")
	s.AddTool(&mcp.Tool{Name: "new", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return textResult("new") })
	if err := m.Refresh(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Spec("mcp_test_old"); ok {
		t.Fatal("removed tool still admitted")
	}
	if _, err := catalog.Prepare(model.ToolCall{Name: "mcp_test_new"}); !errors.Is(err, tools.ErrUnknownTool) {
		t.Fatal("new tool accepted by old model catalog", err)
	}
	spec, ok := r.Spec("mcp_test_new")
	if !ok || !spec.Risky || r.Capability(spec.Name).ConcurrencySafe {
		t.Fatal("refresh granted unsafe capability", spec)
	}
	if result := r.Run(ctx, nil, model.ToolCall{Name: spec.Name}); result.Status != "ok" || strings.TrimSpace(result.Output) != "new" {
		t.Fatal(result)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err := m.Refresh(canceled, "test"); err == nil {
		t.Fatal("cancelled refresh succeeded")
	}
	if _, ok := r.Spec(spec.Name); !ok {
		t.Fatal("failed refresh removed last good catalog")
	}
}

func TestReconnectCancelsOldCallWithoutReplayingItsSideEffects(t *testing.T) {
	var writes atomic.Int64
	started, release := make(chan struct{}, 1), make(chan struct{})
	s := mcp.NewServer(&mcp.Implementation{Name: "reconnect", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "write", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		writes.Add(1)
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return textResult("released")
		}
	})
	s.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return textResult("ok") })
	h := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	defer h.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := tools.NewRegistry()
	m, err := Connect(ctx, r, []Server{{Name: "test", Transport: "streamableHttp", URL: h.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	finished := make(chan model.ToolResult, 1)
	go func() { finished <- r.Run(ctx, nil, model.ToolCall{Name: "mcp_test_write"}) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("call not started")
	}
	if err := m.Reconnect(ctx, "test", 2); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-finished:
		if result.Status != "error" || !strings.Contains(result.Error, "side effects") {
			t.Fatal(result)
		}
	case <-ctx.Done():
		t.Fatal("old call did not settle")
	}
	if writes.Load() != 1 {
		t.Fatal("interrupted write replayed", writes.Load())
	}
	if result := r.Run(ctx, nil, model.ToolCall{Name: "mcp_test_ping"}); result.Status != "ok" {
		t.Fatal(result)
	}
	if err := m.Reconnect(ctx, "test", 4); err == nil {
		t.Fatal("unbounded reconnect attempts accepted")
	}
}

func TestReconnectFailureIsBoundedAndLeavesToolsRevoked(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "retry", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return textResult("ok") })
	var unavailable atomic.Bool
	var rejected atomic.Int64
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() && r.Method == http.MethodPost {
			rejected.Add(1)
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := tools.NewRegistry()
	m, err := Connect(ctx, r, []Server{{Name: "test", Transport: "streamableHttp", URL: h.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	unavailable.Store(true)
	started := time.Now()
	if err := m.Reconnect(ctx, "test", 3); err == nil {
		t.Fatal("unavailable server reconnected")
	}
	if time.Since(started) > 5*time.Second || rejected.Load() < 3 || rejected.Load() > 6 {
		t.Fatalf("unexpected retry bound: %s attempts=%d", time.Since(started), rejected.Load())
	}
	if _, ok := r.Spec("mcp_test_ping"); ok {
		t.Fatal("failed reconnect left stale tool callable")
	}
	unavailable.Store(false)
	if err := m.Reconnect(ctx, "test", 1); err != nil {
		t.Fatal("later recovery failed", err)
	}
	if result := r.Run(ctx, nil, model.ToolCall{Name: "mcp_test_ping"}); result.Status != "ok" {
		t.Fatal(result)
	}
}
