package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/sandbox"
	"github.com/Lundomn/gopherharness/internal/store"
	"github.com/Lundomn/gopherharness/internal/workspace"
)

var ErrNoCandidate = errors.New("no further candidate")
var ErrEvaluationBudget = errors.New("maintenance verifier budget exhausted")

type EvalCase struct {
	ID             string `json:"id"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// EvalContract is fixed before any proposal. Holdout is deliberately absent
// from ProposalInput and never fed back into candidate generation. This is a
// data-flow boundary, not an OS confidentiality claim for arbitrary callbacks.
type EvalContract struct {
	Train           []EvalCase `json:"train"`
	Holdout         []EvalCase `json:"holdout"`
	AllowedPaths    []string   `json:"allowed_paths"`
	ProtectedPaths  []string   `json:"protected_paths,omitempty"`
	MaxCandidates   int        `json:"max_candidates"`
	MaxVerifierRuns int        `json:"max_verifier_runs"`
	TimeoutSeconds  int        `json:"timeout_seconds"`
}
type CaseResult struct {
	ID         string `json:"id"`
	Passed     bool   `json:"passed"`
	ExitCode   int    `json:"exit_code"`
	Error      string `json:"error,omitempty"`
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}
type CandidateResult struct {
	ID          string       `json:"id"`
	Status      string       `json:"status"`
	EditPath    string       `json:"edit_path"`
	TrainPath   string       `json:"train_path"`
	PatchPath   string       `json:"patch_path"`
	PatchDigest string       `json:"patch_digest"`
	Paths       []string     `json:"paths"`
	Train       []CaseResult `json:"train"`
	Error       string       `json:"error,omitempty"`
}
type EvolutionRun struct {
	Schema            string            `json:"schema"`
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	Root              string            `json:"root"`
	BaseRevision      string            `json:"base_revision"`
	Contract          EvalContract      `json:"contract"`
	ContractDigest    string            `json:"contract_digest"`
	Baseline          []CaseResult      `json:"baseline"`
	Candidates        []CandidateResult `json:"candidates"`
	SelectedCandidate string            `json:"selected_candidate,omitempty"`
	Validation        []CaseResult      `json:"validation"`
	ValidatedWorktree string            `json:"validated_worktree,omitempty"`
	VerifierRuns      int               `json:"verifier_runs"`
	CreatedAt         time.Time         `json:"created_at"`
	FinishedAt        time.Time         `json:"finished_at,omitempty"`
	Error             string            `json:"error,omitempty"`
}
type ProposalInput struct {
	Index        int
	Worktree     string
	BaseRevision string
	Request      string
	Train        []EvalCase
	Baseline     []CaseResult
	Previous     []CandidateResult
}
type Propose func(context.Context, ProposalInput) error
type Evolver struct {
	Config  Config
	Propose Propose
}

func cleanRelative(path string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(path) || strings.ContainsRune(clean, 0) {
		return "", errors.New("paths must be explicit repository-relative paths")
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".git" || part == ".pico" {
			return "", errors.New("maintenance metadata cannot be a candidate target")
		}
	}
	return clean, nil
}
func validateContract(c *EvalContract) error {
	if c.MaxCandidates == 0 {
		c.MaxCandidates = 3
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 600
	}
	if c.MaxCandidates < 1 || c.MaxCandidates > 20 || c.TimeoutSeconds < 1 || c.TimeoutSeconds > 3600 {
		return errors.New("invalid maintenance candidate/time limit")
	}
	if len(c.Train) == 0 || len(c.Holdout) == 0 || len(c.AllowedPaths) == 0 {
		return errors.New("train, disjoint holdout, and explicit allowed paths are required")
	}
	if c.MaxVerifierRuns == 0 {
		c.MaxVerifierRuns = len(c.Train)*(c.MaxCandidates+1) + len(c.Holdout)
	}
	if c.MaxVerifierRuns < len(c.Train)*2+len(c.Holdout) || c.MaxVerifierRuns > 10000 {
		return errors.New("invalid verifier run budget")
	}
	ids, commands := map[string]bool{}, map[string]bool{}
	for _, set := range [][]EvalCase{c.Train, c.Holdout} {
		for i := range set {
			v := &set[i]
			v.Command = strings.TrimSpace(v.Command)
			if v.ID == "" || v.Command == "" || ids[v.ID] || commands[v.Command] {
				return errors.New("case IDs and commands must be nonempty and disjoint across splits")
			}
			ids[v.ID] = true
			commands[v.Command] = true
			if v.TimeoutSeconds == 0 {
				v.TimeoutSeconds = 60
			}
			if v.TimeoutSeconds < 1 || v.TimeoutSeconds > 600 {
				return errors.New("case timeout must be in [1,600]")
			}
		}
	}
	for _, paths := range [][]string{c.AllowedPaths, c.ProtectedPaths} {
		for i, p := range paths {
			clean, err := cleanRelative(p)
			if err != nil {
				return err
			}
			paths[i] = clean
		}
	}
	return nil
}
func digest(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }
func saveRun(run EvolutionRun) error {
	b, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	return durable.Replace(filepath.Join(run.Root, ".pico", "maintenance", run.ID, "evolution.json"), b)
}

var runIDPattern = regexp.MustCompile(`^evolve_[0-9]{8}-[0-9]{6}\.[0-9]{6}-[a-f0-9]{8}$`)

func ReadEvolution(root, id string) (EvolutionRun, error) {
	var run EvolutionRun
	if !runIDPattern.MatchString(id) {
		return run, errors.New("invalid evolution run ID")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return run, err
	}
	b, err := os.ReadFile(filepath.Join(abs, ".pico", "maintenance", id, "evolution.json"))
	if err != nil {
		return run, err
	}
	if err = json.Unmarshal(b, &run); err != nil {
		return run, err
	}
	if run.Schema != "gopherharness.evolution.v1" || run.ID != id || run.Root != abs {
		return run, errors.New("invalid evolution record identity")
	}
	contract, _ := json.Marshal(run.Contract)
	if digest(contract) != run.ContractDigest {
		return run, errors.New("evaluation contract changed")
	}
	return run, nil
}

func (e *Evolver) Run(parent context.Context, request string, contract EvalContract) (run EvolutionRun, err error) {
	if !e.Config.ApproveVerifiers {
		return run, errors.New("external verifier execution requires explicit approval")
	}
	if e.Propose == nil {
		return run, errors.New("candidate generator is required")
	}
	contract.Train = append([]EvalCase(nil), contract.Train...)
	contract.Holdout = append([]EvalCase(nil), contract.Holdout...)
	contract.AllowedPaths = append([]string(nil), contract.AllowedPaths...)
	contract.ProtectedPaths = append([]string(nil), contract.ProtectedPaths...)
	if err = validateContract(&contract); err != nil {
		return run, err
	}
	root, err := filepath.Abs(e.Config.Root)
	if err != nil {
		return run, err
	}
	base := e.Config.BaseRevision
	if base == "" {
		base = "HEAD"
	}
	sha, err := git(parent, root, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return run, err
	}
	contractJSON, _ := json.Marshal(contract)
	run = EvolutionRun{Schema: "gopherharness.evolution.v1", ID: store.NewID("evolve"), Status: "running", Root: root, BaseRevision: strings.TrimSpace(string(sha)), Contract: contract, ContractDigest: digest(contractJSON), CreatedAt: time.Now().UTC()}
	if err = saveRun(run); err != nil {
		return run, err
	}
	defer func() {
		if err != nil {
			run.Error = err.Error()
			if run.Status == "running" {
				run.Status = "failed"
			}
		}
		run.FinishedAt = time.Now().UTC()
		err = errors.Join(err, saveRun(run))
	}()
	ctx, cancel := context.WithTimeout(parent, time.Duration(contract.TimeoutSeconds)*time.Second)
	defer cancel()
	dir := filepath.Join(root, ".pico", "maintenance", run.ID)
	baselinePath := filepath.Join(dir, "baseline")
	if _, err = git(ctx, root, "worktree", "add", "--detach", baselinePath, run.BaseRevision); err != nil {
		return run, err
	}
	run.Baseline, err = e.evaluate(ctx, &run, baselinePath, contract.Train)
	if err != nil {
		return run, err
	}
	baselineChanges, statusErr := git(ctx, baselinePath, "status", "--porcelain", "--untracked-files=all")
	if statusErr != nil {
		return run, statusErr
	}
	if len(baselineChanges) > 0 {
		return run, errors.New("baseline verifier changed its pinned worktree")
	}
	best := -1
	for index := 1; index <= contract.MaxCandidates; index++ {
		if err = ctx.Err(); err != nil {
			return run, err
		}
		id := fmt.Sprintf("candidate-%02d", index)
		c := CandidateResult{ID: id, Status: "generating", EditPath: filepath.Join(dir, id, "edit"), TrainPath: filepath.Join(dir, id, "train"), PatchPath: filepath.Join(dir, id, "candidate.patch")}
		if _, err = git(ctx, root, "worktree", "add", "--detach", c.EditPath, run.BaseRevision); err != nil {
			return run, err
		}
		input := ProposalInput{Index: index, Worktree: c.EditPath, BaseRevision: run.BaseRevision, Request: request, Train: append([]EvalCase(nil), contract.Train...), Baseline: append([]CaseResult(nil), run.Baseline...), Previous: cloneCandidates(run.Candidates)}
		proposalErr := e.Propose(ctx, input)
		if errors.Is(proposalErr, ErrNoCandidate) {
			break
		}
		if proposalErr != nil {
			c.Status = "generation_failed"
			c.Error = proposalErr.Error()
			run.Candidates = append(run.Candidates, c)
			if err = saveRun(run); err != nil {
				return run, err
			}
			continue
		}
		patch, paths, patchErr := capturePatch(ctx, c.EditPath, run.BaseRevision)
		if patchErr == nil {
			patchErr = allowedChanges(paths, contract)
		}
		if patchErr != nil {
			c.Status = "rejected"
			c.Error = patchErr.Error()
			run.Candidates = append(run.Candidates, c)
			if err = saveRun(run); err != nil {
				return run, err
			}
			continue
		}
		c.PatchDigest = digest(patch)
		c.Paths = paths
		if err = durable.Replace(c.PatchPath, patch); err != nil {
			return run, err
		}
		if _, err = git(ctx, root, "worktree", "add", "--detach", c.TrainPath, run.BaseRevision); err != nil {
			return run, err
		}
		if _, err = git(ctx, c.TrainPath, "apply", "--check", c.PatchPath); err != nil {
			return run, err
		}
		if _, err = git(ctx, c.TrainPath, "apply", c.PatchPath); err != nil {
			return run, err
		}
		c.Status = "training"
		run.Candidates = append(run.Candidates, c)
		ci := len(run.Candidates) - 1
		if err = saveRun(run); err != nil {
			return run, err
		}
		c.Train, err = e.evaluate(ctx, &run, c.TrainPath, contract.Train)
		if err != nil {
			c.Status = "inconclusive"
			c.Error = err.Error()
			run.Candidates[ci] = c
			return run, err
		}
		measured, _, measureErr := capturePatch(ctx, c.TrainPath, run.BaseRevision)
		if measureErr != nil || digest(measured) != c.PatchDigest {
			c.Status = "rejected"
			c.Error = "training verifier modified candidate files"
		} else if improves(run.Baseline, c.Train) {
			c.Status = "train_accepted"
			if best < 0 || passCount(c.Train) > passCount(run.Candidates[best].Train) {
				best = ci
			}
		} else {
			c.Status = "rejected"
			c.Error = "no strict train improvement or regression of a passing train case"
		}
		run.Candidates[ci] = c
		if err = saveRun(run); err != nil {
			return run, err
		}
	}
	if best < 0 {
		run.Status = "rejected"
		return run, nil
	}
	// Freeze the train-selected winner before examining holdout. A failure does
	// not select the next candidate or start another generation round.
	winner := run.Candidates[best]
	run.SelectedCandidate = winner.ID
	run.Status = "validating"
	run.ValidatedWorktree = filepath.Join(dir, "validated")
	if err = saveRun(run); err != nil {
		return run, err
	}
	if _, err = git(ctx, root, "worktree", "add", "--detach", run.ValidatedWorktree, run.BaseRevision); err != nil {
		return run, err
	}
	if _, err = git(ctx, run.ValidatedWorktree, "apply", "--check", winner.PatchPath); err != nil {
		return run, err
	}
	if _, err = git(ctx, run.ValidatedWorktree, "apply", winner.PatchPath); err != nil {
		return run, err
	}
	run.Validation, err = e.evaluate(ctx, &run, run.ValidatedWorktree, contract.Holdout)
	if err != nil {
		run.Status = "validation_inconclusive"
		return run, err
	}
	measured, _, measureErr := capturePatch(ctx, run.ValidatedWorktree, run.BaseRevision)
	if measureErr != nil || digest(measured) != winner.PatchDigest {
		run.Status = "rejected"
		run.Error = "holdout verifier modified the candidate"
		return run, nil
	}
	if passCount(run.Validation) != len(contract.Holdout) {
		run.Status = "rejected"
		run.Error = "held-out validation failed"
		return run, nil
	}
	run.Status = "candidate_ready"
	return run, nil
}

func cloneCandidates(items []CandidateResult) []CandidateResult {
	b, _ := json.Marshal(items)
	var result []CandidateResult
	_ = json.Unmarshal(b, &result)
	return result
}
func passCount(results []CaseResult) int {
	n := 0
	for _, r := range results {
		if r.Passed {
			n++
		}
	}
	return n
}
func improves(baseline, candidate []CaseResult) bool {
	if len(baseline) != len(candidate) || passCount(candidate) <= passCount(baseline) {
		return false
	}
	for i, b := range baseline {
		if b.ID != candidate[i].ID || b.Passed && !candidate[i].Passed {
			return false
		}
	}
	return true
}
func (e *Evolver) evaluate(ctx context.Context, run *EvolutionRun, root string, cases []EvalCase) ([]CaseResult, error) {
	results := []CaseResult{}
	runner := sandbox.New(e.Config.Sandbox)
	for _, c := range cases {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		if run.VerifierRuns >= run.Contract.MaxVerifierRuns {
			return results, ErrEvaluationBudget
		}
		run.VerifierRuns++
		if err := saveRun(*run); err != nil {
			return results, err
		}
		start := time.Now()
		r, err := runner.Run(ctx, c.Command, root, workspace.ShellEnv(), c.TimeoutSeconds)
		result := CaseResult{ID: c.ID, Passed: err == nil && r.ExitCode == 0, ExitCode: r.ExitCode, Output: boundedOutput(r.Stdout + "\n" + r.Stderr), DurationMS: time.Since(start).Milliseconds()}
		if err == nil && (r.ExitCode == 126 || r.ExitCode == 127) {
			err = errors.New("verifier command unavailable or not executable")
		}
		if err != nil {
			result.Error = err.Error()
		}
		results = append(results, result)
		// Infrastructure/cancellation is not a failed task or a zero score.
		if err != nil {
			return results, err
		}
	}
	return results, nil
}
func boundedOutput(s string) string {
	r := []rune(s)
	if len(r) > 8000 {
		return string(r[:8000]) + " [truncated]"
	}
	return s
}
func capturePatch(ctx context.Context, root, base string) ([]byte, []string, error) {
	return captureCandidatePatch(ctx, root, base, true)
}
func captureCandidatePatch(ctx context.Context, root, base string, stageNew bool) ([]byte, []string, error) {
	head, err := git(ctx, root, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != base {
		return nil, nil, errors.New("candidate changed its pinned base revision")
	}
	top, err := git(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, nil, err
	}
	actual, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil, err
	}
	actualTop, err := filepath.EvalSymlinks(strings.TrimSpace(string(top)))
	if err != nil || actualTop != actual {
		return nil, nil, errors.New("candidate worktree root changed")
	}
	if stageNew {
		if _, err = git(ctx, root, "add", "-N", "--", "."); err != nil {
			return nil, nil, err
		}
	} else {
		untracked, e := git(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
		if e != nil {
			return nil, nil, e
		}
		if len(untracked) > 0 {
			return nil, nil, errors.New("untracked files appeared after validation")
		}
	}
	pathsRaw, err := git(ctx, root, "diff", "--name-only", "-z", "HEAD", "--", ".")
	if err != nil {
		return nil, nil, err
	}
	paths := strings.Split(strings.TrimSuffix(string(pathsRaw), "\x00"), "\x00")
	for _, path := range paths {
		if _, err := cleanRelative(path); err != nil {
			return nil, nil, err
		}
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		if err == nil && !info.Mode().IsRegular() {
			return nil, nil, errors.New("candidate symlinks/submodules/non-regular files are not allowed")
		}
	}
	patch, err := git(ctx, root, "diff", "--binary", "HEAD", "--", ".")
	if err != nil {
		return nil, nil, err
	}
	if len(patch) == 0 {
		return nil, nil, errors.New("candidate produced no patch")
	}
	return patch, paths, nil
}
func matchesPath(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
func allowedChanges(paths []string, c EvalContract) error {
	for _, p := range paths {
		path, err := cleanRelative(p)
		if err != nil {
			return err
		}
		// Evaluation and guard code are not a legal repair target. An explicit
		// allowlist cannot override this gate or the fixed protected paths.
		for _, part := range strings.Split(path, "/") {
			if part == "test" || part == "tests" || part == "testdata" || part == "benchmarks" || part == ".github" {
				return fmt.Errorf("candidate changes verifier material %s", path)
			}
		}
		if strings.HasSuffix(path, "_test.go") {
			return fmt.Errorf("candidate changes tests %s", path)
		}
		for _, protected := range c.ProtectedPaths {
			if matchesPath(path, protected) {
				return fmt.Errorf("candidate changes protected path %s", path)
			}
		}
		allowed := false
		for _, prefix := range c.AllowedPaths {
			if matchesPath(path, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("candidate target is not allowed: %s", path)
		}
	}
	return nil
}

// PatchProposal is a no-model generator for reviewed patches. It never commits,
// invokes a shell or fetches data. The caller supplies local patch files.
func PatchProposal(paths []string) Propose {
	copy := append([]string(nil), paths...)
	return func(ctx context.Context, in ProposalInput) error {
		if in.Index < 1 || in.Index > len(copy) {
			return ErrNoCandidate
		}
		path, err := filepath.Abs(copy[in.Index-1])
		if err != nil {
			return err
		}
		if _, err = git(ctx, in.Worktree, "apply", "--check", path); err != nil {
			return err
		}
		_, err = git(ctx, in.Worktree, "apply", path)
		return err
	}
}
