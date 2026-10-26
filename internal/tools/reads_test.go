package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/workspace"
)

type confinedReadHost struct {
	Host
	w *workspace.Workspace
}

func (h confinedReadHost) ReadSnapshot(ctx context.Context, path string) ([]byte, error) {
	return h.w.ReadSnapshot(ctx, path)
}
func (h confinedReadHost) ListFiles(ctx context.Context, path string) (string, error) {
	return h.w.ListFiles(ctx, path)
}
func (h confinedReadHost) SearchFiles(ctx context.Context, pattern, path string) (string, error) {
	return h.w.SearchFiles(ctx, pattern, path)
}

func TestReadToolsUseConfinedHostAndBoundResults(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(strings.Repeat("x", 100000)), 0600); err != nil {
		t.Fatal(err)
	}
	w, _ := workspace.Open(root)
	h := confinedReadHost{w: w}
	r := NewRegistry()
	result := r.Run(context.Background(), h, model.ToolCall{Name: "read_file", Args: map[string]any{"path": "long.txt"}})
	if result.Status != "ok" || !strings.Contains(result.Output, "truncated") || len(result.Output) > workspace.MaxReadToolOutputBytes+200 {
		t.Fatalf("%#v", result)
	}
	for _, call := range []model.ToolCall{{Name: "list_files"}, {Name: "search", Args: map[string]any{"pattern": "x"}}, {Name: "read_file", Args: map[string]any{"path": "long.txt"}}} {
		if result := r.Run(context.Background(), nil, call); result.Status != "error" || !strings.Contains(result.Error, "confined") {
			t.Fatalf("unsafe fallback: %#v", result)
		}
	}
}
