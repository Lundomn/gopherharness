//go:build !windows

package mcpbridge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPStdioCloseAlsoStopsDescendant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := tools.NewRegistry()
	m, err := Connect(ctx, r, []Server{{Name: "child", Transport: "stdio", Command: os.Args[0], Args: []string{"-test.run=^TestMCPProcessGroupHelper$", "--", "mcp-process-group-helper"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	result := r.Run(ctx, nil, model.ToolCall{Name: "mcp_child_pid"})
	pid, err := strconv.Atoi(strings.TrimSpace(result.Output))
	if result.Status != "ok" || err != nil {
		t.Fatalf("pid response %#v: %v", result, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("descendant did not start", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// An orphan may briefly remain a zombie until the OS reaps it; it must not
	// remain a running process after ownership is released.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	if err == nil && strings.TrimSpace(string(out)) != "" && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
		t.Fatalf("MCP descendant still running: pid=%d status=%q", pid, out)
	}
}

func TestMCPProcessGroupHelper(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "mcp-process-group-helper" {
		return
	}
	child := exec.Command("/bin/sleep", "30")
	if child.Start() != nil {
		os.Exit(2)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "child-helper", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "pid", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprint(child.Process.Pid)}}}, nil
	})
	if s.Run(context.Background(), &mcp.StdioTransport{}) != nil {
		os.Exit(3)
	}
	// Intentionally do not clean the child: the Manager must own this cleanup.
	os.Exit(0)
}
