package evaluation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/provider"
)

func TestEvaluatorRunsFixtureAndVerifier(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture")
	_ = os.MkdirAll(fixture, 0o755)
	_ = os.WriteFile(filepath.Join(fixture, "README.md"), []byte("demo"), 0o600)
	benchmark := Benchmark{SchemaVersion: 1, Tasks: []Task{{ID: "write", Prompt: "write result", FixtureRepo: "fixture", AllowedTools: []string{"write_file"}, StepBudget: 2, ExpectedArtifact: "result.txt", Verifier: "test -f result.txt"}}}
	raw, _ := json.Marshal(benchmark)
	path := filepath.Join(root, "benchmark.json")
	_ = os.WriteFile(path, raw, 0o600)
	fake := &provider.Fake{Responses: []string{`<tool>{"name":"write_file","args":{"path":"result.txt","content":"ok"}}</tool>`, `<final>done</final>`}}
	artifact, err := Run(context.Background(), Options{BenchmarkPath: path, FixtureRoot: root, WorkspaceRoot: filepath.Join(root, "workspaces"), ArtifactPath: filepath.Join(root, "artifact.json"), Config: config.Defaults(), Provider: fake})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Passed != 1 || artifact.Failed != 0 {
		t.Fatalf("artifact=%#v", artifact)
	}
}
