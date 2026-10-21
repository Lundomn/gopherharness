package mcpbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tools"
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
