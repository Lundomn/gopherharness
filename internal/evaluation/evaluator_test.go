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
	if artifact.Summary.TotalTasks != 1 || artifact.Summary.PassRate != 1 || artifact.Summary.VerifierPassRate != 1 {
		t.Fatalf("summary=%#v", artifact.Summary)
	}
	row := artifact.Rows[0]
	if row.FixtureDigest == "" || row.ArtifactDigest == "" {
		t.Fatalf("expected reproducibility digests: %#v", row)
	}
	if row.VerifierExitCode != 0 {
		t.Fatalf("verifier exit code=%d", row.VerifierExitCode)
	}
}

func TestTruncateOutput(t *testing.T) {
	short := "ok"
	if got := truncateOutput(short); got != short {
		t.Fatalf("short output changed: %q", got)
	}
	long := make([]byte, maxVerifierOutput+10)
	for i := range long {
		long[i] = 'x'
	}
	got := truncateOutput(string(long))
	if len(got) != maxVerifierOutput+len("...<truncated>") || got[len(got)-len("...<truncated>"):] != "...<truncated>" {
		t.Fatalf("unexpected truncation length/suffix: %d %q", len(got), got[len(got)-len("...<truncated>"):])
	}
}

func TestEvaluatorUsesTaskLocalResponsesWithoutProvider(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture")
	if err := os.MkdirAll(fixture, 0o755); err != nil {
		t.Fatal(err)
	}
	benchmark := Benchmark{SchemaVersion: 1, Tasks: []Task{{
		ID: "offline", Prompt: "create marker", FixtureRepo: "fixture", ExpectedArtifact: "marker.txt",
		AllowedTools: []string{"write_file"}, StepBudget: 2, Category: "workspace-write",
		Responses: []string{
			`<tool>{"name":"write_file","args":{"path":"marker.txt","content":"offline"}}</tool>`,
			`<final>done</final>`,
		},
		Verifier: `test "$(cat marker.txt)" = offline`,
	}}}
	raw, err := json.Marshal(benchmark)
	if err != nil {
		t.Fatal(err)
	}
	benchmarkPath := filepath.Join(root, "benchmark.json")
	if err = os.WriteFile(benchmarkPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, err := Run(context.Background(), Options{
		BenchmarkPath: benchmarkPath, FixtureRoot: root, WorkspaceRoot: filepath.Join(root, "workspaces"),
		Config: config.Defaults(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Passed != 1 || artifact.Failed != 0 {
		t.Fatalf("artifact=%#v", artifact)
	}
	if artifact.Summary.ExpectedFailures != 0 || artifact.Summary.CategoryCounts["workspace-write"] != 1 {
		t.Fatalf("summary=%#v", artifact.Summary)
	}
}

func TestEvaluatorAcceptsDeclaredNegativeControl(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture")
	if err := os.MkdirAll(fixture, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	benchmark := Benchmark{SchemaVersion: 1, Tasks: []Task{{
		ID: "limit", Prompt: "keep listing", FixtureRepo: "fixture", Category: "reliability",
		AllowedTools: []string{"list_files"}, StepBudget: 1, ExpectFailure: true,
		ExpectedStopReason: "step_limit_reached", Responses: []string{
			`<tool>{"name":"list_files","args":{"path":"."}}</tool>`,
		}, Verifier: "test -f README.md",
	}}}
	raw, err := json.Marshal(benchmark)
	if err != nil {
		t.Fatal(err)
	}
	benchmarkPath := filepath.Join(root, "benchmark.json")
	if err = os.WriteFile(benchmarkPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, err := Run(context.Background(), Options{BenchmarkPath: benchmarkPath, FixtureRoot: root, WorkspaceRoot: filepath.Join(root, "workspaces"), Config: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Passed != 1 || artifact.Summary.ExpectedFailures != 1 || artifact.Rows[0].StopReason != "step_limit_reached" {
		t.Fatalf("artifact=%#v", artifact)
	}
}

func TestValidateBenchmarkRejectsUnsafeTaskPaths(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture")
	if err := os.MkdirAll(fixture, 0o755); err != nil {
		t.Fatal(err)
	}
	base := Task{ID: "safe", Prompt: "p", FixtureRepo: "fixture", AllowedTools: []string{"read_file"}, StepBudget: 1, Verifier: "true"}
	for name, mutate := range map[string]func(*Task){
		"task id traversal":  func(task *Task) { task.ID = "../escape" },
		"absolute fixture":   func(task *Task) { task.FixtureRepo = filepath.Join(root, "fixture") },
		"artifact traversal": func(task *Task) { task.ExpectedArtifact = "../escape.txt" },
	} {
		t.Run(name, func(t *testing.T) {
			task := base
			mutate(&task)
			if err := validateBenchmark(Benchmark{SchemaVersion: 1, Tasks: []Task{task}}, root); err == nil {
				t.Fatal("unsafe benchmark was accepted")
			}
		})
	}
}
