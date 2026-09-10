package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/governance"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/runtime"
)

type Benchmark struct {
	SchemaVersion int    `json:"schema_version"`
	Tasks         []Task `json:"tasks"`
}
type Task struct {
	ID                 string   `json:"id"`
	Prompt             string   `json:"prompt"`
	FixtureRepo        string   `json:"fixture_repo"`
	ExpectedArtifact   string   `json:"expected_artifact"`
	Verifier           string   `json:"verifier"`
	Category           string   `json:"category"`
	AllowedTools       []string `json:"allowed_tools"`
	StepBudget         int      `json:"step_budget"`
	Responses          []string `json:"responses,omitempty"`
	ExpectFailure      bool     `json:"expect_failure,omitempty"`
	ExpectedStopReason string   `json:"expected_stop_reason,omitempty"`
}
type Row struct {
	ID                 string `json:"id"`
	Status             string `json:"status"`
	FailureCategory    string `json:"failure_category,omitempty"`
	Answer             string `json:"answer,omitempty"`
	Workspace          string `json:"workspace"`
	RunID              string `json:"run_id"`
	Passed             bool   `json:"passed"`
	ArtifactExists     bool   `json:"artifact_exists"`
	VerifierPassed     bool   `json:"verifier_passed"`
	WithinBudget       bool   `json:"within_budget"`
	ToolSteps          int    `json:"tool_steps"`
	Attempts           int    `json:"attempts"`
	StopReason         string `json:"stop_reason,omitempty"`
	ExpectedStopReason string `json:"expected_stop_reason,omitempty"`
	ExpectedFailure    bool   `json:"expected_failure,omitempty"`
	Category           string `json:"category,omitempty"`
	VerifierExitCode   int    `json:"verifier_exit_code"`
	DurationMS         int64  `json:"duration_ms"`
}
type Summary struct {
	TotalTasks       int            `json:"total_tasks"`
	Passed           int            `json:"passed"`
	Failed           int            `json:"failed"`
	WithinBudget     int            `json:"within_budget"`
	VerifierPasses   int            `json:"verifier_passes"`
	ExpectedFailures int            `json:"expected_failures"`
	PassRate         float64        `json:"pass_rate"`
	WithinBudgetRate float64        `json:"within_budget_rate"`
	VerifierPassRate float64        `json:"verifier_pass_rate"`
	CategoryCounts   map[string]int `json:"category_counts"`
}
type Artifact struct {
	SchemaVersion string    `json:"schema_version"`
	CapturedAt    time.Time `json:"captured_at"`
	Passed        int       `json:"passed"`
	Failed        int       `json:"failed"`
	Summary       Summary   `json:"summary"`
	Rows          []Row     `json:"rows"`
}
type Options struct {
	BenchmarkPath, FixtureRoot, WorkspaceRoot, ArtifactPath string
	Config                                                  config.Config
	Provider                                                provider.Client
}

func Run(ctx context.Context, opts Options) (Artifact, error) {
	b, err := load(opts.BenchmarkPath)
	if err != nil {
		return Artifact{}, err
	}
	if opts.FixtureRoot == "" {
		opts.FixtureRoot = filepath.Dir(opts.BenchmarkPath)
	}
	if err := validateBenchmark(b, opts.FixtureRoot); err != nil {
		return Artifact{}, err
	}
	if opts.WorkspaceRoot == "" {
		opts.WorkspaceRoot = filepath.Join(filepath.Dir(opts.BenchmarkPath), "artifacts", "go-workspaces")
	}
	artifact := Artifact{SchemaVersion: "pico.go.benchmark.v1", CapturedAt: time.Now().UTC()}
	for _, task := range b.Tasks {
		row := runTask(ctx, opts, task)
		artifact.Rows = append(artifact.Rows, row)
		if row.Passed {
			artifact.Passed++
		} else {
			artifact.Failed++
		}
	}
	artifact.Summary = summarize(artifact.Rows)
	if opts.ArtifactPath != "" {
		if err = writeJSON(opts.ArtifactPath, artifact); err != nil {
			return artifact, err
		}
	}
	return artifact, nil
}
func runTask(ctx context.Context, opts Options, task Task) Row {
	started := time.Now()
	row := Row{ID: task.ID, Status: "fail"}
	src := filepath.Join(opts.FixtureRoot, task.FixtureRepo)
	dst := filepath.Join(opts.WorkspaceRoot, task.ID, filepath.Base(task.FixtureRepo))
	row.Workspace = dst
	// Every task owns its workspace. Remove a previous run so stale artifacts
	// cannot turn a failed task into a false positive on the next baseline run.
	if err := os.RemoveAll(dst); err != nil {
		row.FailureCategory = "workspace_reset_failed"
		return row
	}
	if err := copyTree(src, dst); err != nil {
		row.FailureCategory = "fixture_copy_failed"
		return row
	}
	steps := task.StepBudget
	if steps <= 0 {
		steps = 6
	}
	client := opts.Provider
	if len(task.Responses) > 0 {
		client = &provider.Fake{Responses: append([]string(nil), task.Responses...)}
	}
	if client == nil {
		row.FailureCategory = "provider_missing"
		return row
	}
	agent, err := runtime.New(runtime.Options{Root: dst, Config: opts.Config, Provider: client, Approval: permission.Auto, FinalReadiness: governance.Off, MaxSteps: steps, AllowedTools: task.AllowedTools, DisableAutoDream: true})
	if err != nil {
		row.FailureCategory = "agent_init_failed"
		return row
	}
	defer agent.Close()
	answer, askErr := agent.Ask(ctx, task.Prompt)
	row.Answer = answer
	row.RunID = agent.LastRunID()
	row.DurationMS = time.Since(started).Milliseconds()
	reportPath := filepath.Join(dst, ".pico", "runs", row.RunID, "report.json")
	var report struct {
		ToolSteps  int    `json:"tool_steps"`
		Attempts   int    `json:"attempts"`
		StopReason string `json:"stop_reason"`
	}
	if b, err := os.ReadFile(reportPath); err == nil {
		_ = json.Unmarshal(b, &report)
	}
	row.ToolSteps = report.ToolSteps
	row.Attempts = report.Attempts
	row.StopReason = report.StopReason
	row.ExpectedStopReason = task.ExpectedStopReason
	row.ExpectedFailure = task.ExpectFailure
	row.Category = task.Category
	row.WithinBudget = row.ToolSteps <= steps
	artifactPath := filepath.Join(dst, task.ExpectedArtifact)
	if task.ExpectedArtifact == "" {
		row.ArtifactExists = true
	} else {
		_, err = os.Stat(artifactPath)
		row.ArtifactExists = err == nil
	}
	row.VerifierPassed = true
	if task.Verifier != "" {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-lc", task.Verifier)
		cmd.Dir = dst
		if err = cmd.Run(); err != nil {
			row.VerifierPassed = false
			row.VerifierExitCode = 1
			if exit, ok := err.(*exec.ExitError); ok {
				row.VerifierExitCode = exit.ExitCode()
			}
		}
	}
	if task.ExpectFailure {
		row.Passed = askErr == nil && row.StopReason == task.ExpectedStopReason && row.VerifierPassed
	} else {
		row.Passed = askErr == nil && row.WithinBudget && row.ArtifactExists && row.VerifierPassed
	}
	if row.Passed {
		row.Status = "pass"
	} else {
		switch {
		case askErr != nil:
			row.FailureCategory = "agent_failed"
		case !row.WithinBudget:
			row.FailureCategory = "budget_exceeded"
		case !row.ArtifactExists:
			row.FailureCategory = "missing_artifact"
		case !row.VerifierPassed:
			row.FailureCategory = "verifier_failed"
		case task.ExpectFailure && row.StopReason != task.ExpectedStopReason:
			row.FailureCategory = "unexpected_stop_reason"
		}
	}
	return row
}

func summarize(rows []Row) Summary {
	s := Summary{TotalTasks: len(rows), CategoryCounts: map[string]int{}}
	for _, row := range rows {
		s.CategoryCounts[row.Category]++
		if row.Passed {
			s.Passed++
		} else {
			s.Failed++
		}
		if row.WithinBudget {
			s.WithinBudget++
		}
		if row.VerifierPassed {
			s.VerifierPasses++
		}
		if row.ExpectedFailure {
			s.ExpectedFailures++
		}
	}
	if s.TotalTasks > 0 {
		s.PassRate = float64(s.Passed) / float64(s.TotalTasks)
		s.WithinBudgetRate = float64(s.WithinBudget) / float64(s.TotalTasks)
		s.VerifierPassRate = float64(s.VerifierPasses) / float64(s.TotalTasks)
	}
	return s
}
func load(path string) (Benchmark, error) {
	var b Benchmark
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	err = json.Unmarshal(raw, &b)
	if err == nil && len(b.Tasks) == 0 {
		err = fmt.Errorf("benchmark has no tasks")
	}
	return b, err
}

func validateBenchmark(b Benchmark, fixtureRoot string) error {
	if b.SchemaVersion != 1 {
		return fmt.Errorf("unsupported benchmark schema_version %d", b.SchemaVersion)
	}
	if len(b.Tasks) == 0 {
		return fmt.Errorf("benchmark has no tasks")
	}
	root, err := filepath.Abs(fixtureRoot)
	if err != nil {
		return fmt.Errorf("resolve benchmark fixture root: %w", err)
	}
	seen := map[string]bool{}
	for index, task := range b.Tasks {
		id := strings.TrimSpace(task.ID)
		if id == "" {
			return fmt.Errorf("benchmark task %d has an empty id", index)
		}
		if seen[id] {
			return fmt.Errorf("duplicate benchmark task id %q", id)
		}
		if id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
			return fmt.Errorf("benchmark task %q has an unsafe id", id)
		}
		seen[id] = true
		if strings.TrimSpace(task.Prompt) == "" {
			return fmt.Errorf("benchmark task %q has an empty prompt", id)
		}
		if strings.TrimSpace(task.FixtureRepo) == "" {
			return fmt.Errorf("benchmark task %q has no fixture_repo", id)
		}
		if filepath.IsAbs(task.FixtureRepo) {
			return fmt.Errorf("benchmark task %q fixture must be relative", id)
		}
		fixture := filepath.Join(root, filepath.Clean(task.FixtureRepo))
		rel, relErr := filepath.Rel(root, fixture)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("benchmark task %q fixture escapes fixture root", id)
		}
		st, statErr := os.Stat(fixture)
		if statErr != nil || !st.IsDir() {
			return fmt.Errorf("benchmark task %q fixture does not exist: %s", id, task.FixtureRepo)
		}
		if len(task.AllowedTools) == 0 {
			return fmt.Errorf("benchmark task %q allowed_tools must not be empty", id)
		}
		if task.StepBudget < 1 {
			return fmt.Errorf("benchmark task %q step_budget must be positive", id)
		}
		if strings.TrimSpace(task.Verifier) == "" {
			return fmt.Errorf("benchmark task %q verifier must not be empty", id)
		}
		if artifact := strings.TrimSpace(task.ExpectedArtifact); artifact != "" {
			if filepath.IsAbs(artifact) {
				return fmt.Errorf("benchmark task %q expected_artifact must be relative", id)
			}
			cleanArtifact := filepath.Clean(artifact)
			relArtifact, artifactErr := filepath.Rel(".", cleanArtifact)
			if artifactErr != nil || relArtifact == ".." || strings.HasPrefix(relArtifact, ".."+string(filepath.Separator)) {
				return fmt.Errorf("benchmark task %q expected_artifact escapes workspace", id)
			}
		}
		if task.ExpectFailure && strings.TrimSpace(task.ExpectedStopReason) == "" {
			return fmt.Errorf("benchmark task %q expected_stop_reason is required for a negative control", id)
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".pico") {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}
