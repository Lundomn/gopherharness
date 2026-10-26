package mcpbridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPDiscoverySchemaFailureAndSessionClose(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "remote failure"}}}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registry := tools.NewRegistry()
	manager, err := Connect(ctx, registry, []Server{{Name: "test", Transport: "streamableHttp", URL: httpServer.URL}})
	if err != nil {
		t.Fatal(err)
	}
	cancel() // Initialization timeout is not the session's lifetime.
	ctx = context.Background()
	t.Cleanup(func() { _ = manager.Close() })
	bad := registry.Run(ctx, nil, model.ToolCall{Name: "mcp_test_echo", Args: map[string]any{"text": 1}})
	if bad.ErrorCode != "invalid_parameters" {
		t.Fatal(bad)
	}
	good := registry.Run(ctx, nil, model.ToolCall{Name: "mcp_test_echo", Args: map[string]any{"text": "hi"}})
	if good.Status != "ok" {
		t.Fatal(good)
	}
	fail := registry.Run(ctx, nil, model.ToolCall{Name: "mcp_test_fail"})
	if fail.Status != "error" {
		t.Fatal("remote isError lost")
	}
	if registry.Capability("mcp_test_echo").ConcurrencySafe {
		t.Fatal("untrusted MCP annotation granted concurrency")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSingleCallCancelDoesNotCloseSharedSession(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	server := mcp.NewServer(&mcp.Implementation{Name: "cancel", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "wait", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
		case <-release:
		}
		return nil, ctx.Err()
	})
	server.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := tools.NewRegistry()
	manager, err := Connect(ctx, r, []Server{{Name: "test", Transport: "streamableHttp", URL: httpServer.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	callCtx, stop := context.WithCancel(ctx)
	result := make(chan model.ToolResult, 1)
	go func() { result <- r.Run(callCtx, nil, model.ToolCall{Name: "mcp_test_wait"}) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("call did not start")
	}
	stop()
	select {
	case res := <-result:
		if res.Status != "error" {
			t.Fatal(res)
		}
	case <-ctx.Done():
		t.Fatal("cancel did not return")
	}
	if res := r.Run(ctx, nil, model.ToolCall{Name: "mcp_test_ping"}); res.Status != "ok" {
		t.Fatal("single cancellation closed shared session", res)
	}
	go func() { result <- r.Run(ctx, nil, model.ToolCall{Name: "mcp_test_wait"}) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("second call did not start")
	}
	if err := manager.CloseContext(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	select {
	case <-result:
	case <-ctx.Done():
		t.Fatal("close did not join active call")
	}
	if res := r.Run(ctx, nil, model.ToolCall{Name: "mcp_test_ping"}); res.Status != "error" {
		t.Fatal("closed manager admitted call")
	}
}

type heldConnection struct{ release, closed chan struct{} }

func (c *heldConnection) Read(context.Context) (jsonrpc.Message, error) {
	return nil, errors.New("unused")
}
func (c *heldConnection) Write(context.Context, jsonrpc.Message) error { return nil }
func (c *heldConnection) SessionID() string                            { return "held" }
func (c *heldConnection) Close() error                                 { close(c.closed); <-c.release; return nil }

func TestCloseTimeoutRetainsOwnerAndCanBeJoined(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	conn := &heldConnection{release: make(chan struct{}), closed: make(chan struct{})}
	peerCtx, peerCancel := context.WithCancel(lifetime)
	m := &Manager{ctx: lifetime, cancel: cancel, done: make(chan struct{}), connections: []*serverConnection{{ctx: peerCtx, cancel: peerCancel, done: make(chan struct{}), transport: &ownedTransport{conn: conn}}}}
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if err := m.CloseContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	select {
	case <-conn.closed:
	default:
		t.Fatal("owner did not request transport close")
	}
	select {
	case <-m.done:
		t.Fatal("released ownership before connection exited")
	default:
	}
	close(conn.release)
	joined, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	if err := m.CloseContext(joined); err != nil {
		t.Fatal(err)
	}
}

func TestMCPStdioProcessLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := tools.NewRegistry()
	m, err := Connect(ctx, r, []Server{{Name: "stdio", Transport: "stdio", Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$", "--", "gopher-mcp-helper"}}})
	if err != nil {
		t.Fatal(err)
	}
	result := r.Run(ctx, nil, model.ToolCall{Name: "mcp_stdio_ping"})
	if result.Status != "ok" {
		t.Fatal(result)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMCPHelperProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "gopher-mcp-helper" {
		return
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "helper", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
	})
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
