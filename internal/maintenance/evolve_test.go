package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lundomn/gopherharness/internal/durable"
)

func evolutionRepo(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "fixture"}} {
		if _, err := git(ctx, root, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := durable.Replace(filepath.Join(root, "code.txt"), []byte("bad\n")); err != nil {
		t.Fatal(err)
	}
	if err := durable.Replace(filepath.Join(root, "keep.txt"), []byte("stable\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "add", "code.txt", "keep.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "commit", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}
	return root
}
func contractFixture() EvalContract {
	return EvalContract{
		Train:   []EvalCase{{ID: "train-repair", Command: `test "$(cat code.txt)" = good`}, {ID: "train-stability", Command: `test "$(cat keep.txt)" = stable`}},
		Holdout: []EvalCase{{ID: "holdout", Command: `test "$(wc -c < code.txt | tr -d ' ')" = 5`}}, AllowedPaths: []string{"code.txt"}, MaxCandidates: 2, TimeoutSeconds: 30,
	}
}
func goodProposal(_ context.Context, in ProposalInput) error {
	return durable.Replace(filepath.Join(in.Worktree, "code.txt"), []byte("good\n"))
}

func TestEvolutionFixedSplitsAndManualActivationRollback(t *testing.T) {
	root := evolutionRepo(t)
	ctx := context.Background()
	calls := 0
	e := Evolver{Config: Config{Root: root, ApproveVerifiers: true}, Propose: func(ctx context.Context, in ProposalInput) error {
		calls++
		if len(in.Train) != 2 || in.Train[0].ID != "train-repair" {
			t.Fatal("wrong generation contract", in)
		}
		if in.Index == 1 {
			return durable.Replace(filepath.Join(in.Worktree, "code.txt"), []byte("still-bad\n"))
		}
		if len(in.Previous) != 1 || in.Previous[0].Status != "rejected" {
			t.Fatal("missing train feedback", in.Previous)
		}
		return goodProposal(ctx, in)
	}}
	run, err := e.Run(ctx, "fix code", contractFixture())
	if err != nil || run.Status != "candidate_ready" || calls != 2 || run.SelectedCandidate != "candidate-02" || run.VerifierRuns != 7 {
		t.Fatal(run, err, calls)
	}
	stored, err := ReadEvolution(root, run.ID)
	if err != nil || stored.ContractDigest != run.ContractDigest {
		t.Fatal(stored, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "code.txt")); string(b) != "bad\n" {
		t.Fatal("source checkout was modified")
	}
	if _, err := Activate(ctx, root, run.ID, 0, false); err == nil {
		t.Fatal("activation without approval")
	}
	state, err := Activate(ctx, root, run.ID, 0, true)
	if err != nil || state.Active == nil || state.Version != 1 {
		t.Fatal(state, err)
	}
	active, err := ResolveActiveRoot(ctx, root)
	if err != nil || active != run.ValidatedWorktree {
		t.Fatal(active, err)
	}
	if _, err := Rollback(ctx, root, 0, true); err == nil {
		t.Fatal("stale rollback version accepted")
	}
	state, err = Rollback(ctx, root, 1, true)
	if err != nil || state.Active != nil || state.Version != 2 {
		t.Fatal(state, err)
	}
	active, err = ResolveActiveRoot(ctx, root)
	if err != nil || active != root {
		t.Fatal(active, err)
	}
}

func TestEvolutionHoldoutFailureRejectsWithoutFurtherSearch(t *testing.T) {
	root := evolutionRepo(t)
	contract := contractFixture()
	contract.Holdout[0].Command = "exit 9"
	calls := 0
	e := Evolver{Config: Config{Root: root, ApproveVerifiers: true}, Propose: func(ctx context.Context, in ProposalInput) error { calls++; return goodProposal(ctx, in) }}
	run, err := e.Run(context.Background(), "fix", contract)
	if err != nil || run.Status != "rejected" || calls != 2 || len(run.Validation) != 1 || run.Validation[0].Passed {
		t.Fatal(run, err, calls)
	}
	if _, err := Activate(context.Background(), root, run.ID, 0, true); err == nil {
		t.Fatal("holdout failure activated")
	}
}

func TestEvolutionRejectsGuardEditsAndPassingCaseRegression(t *testing.T) {
	for _, mode := range []string{"guard", "regression"} {
		t.Run(mode, func(t *testing.T) {
			root := evolutionRepo(t)
			c := contractFixture()
			c.MaxCandidates = 1
			c.AllowedPaths = append(c.AllowedPaths, "keep.txt", "fake_test.go")
			e := Evolver{Config: Config{Root: root, ApproveVerifiers: true}, Propose: func(ctx context.Context, in ProposalInput) error {
				if err := goodProposal(ctx, in); err != nil {
					return err
				}
				if mode == "guard" {
					return durable.Replace(filepath.Join(in.Worktree, "fake_test.go"), []byte("disabled"))
				}
				return durable.Replace(filepath.Join(in.Worktree, "keep.txt"), []byte("regressed\n"))
			}}
			run, err := e.Run(context.Background(), "fix", c)
			if err != nil || run.Status != "rejected" || len(run.Validation) > 0 {
				t.Fatal(run, err)
			}
			if mode == "guard" && !strings.Contains(run.Candidates[0].Error, "tests") {
				t.Fatal(run.Candidates)
			}
		})
	}
}

func TestEvolutionBudgetApprovalAndNoPatch(t *testing.T) {
	root := evolutionRepo(t)
	e := Evolver{Config: Config{Root: root}, Propose: goodProposal}
	if _, err := e.Run(context.Background(), "fix", contractFixture()); err == nil {
		t.Fatal("unapproved verifier ran")
	}
	if _, err := os.Stat(filepath.Join(root, ".pico")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unapproved operation created worktrees")
	}
	e.Config.ApproveVerifiers = true
	c := contractFixture()
	c.MaxVerifierRuns = 5
	run, err := e.Run(context.Background(), "fix", c)
	if !errors.Is(err, ErrEvaluationBudget) || run.Status == "candidate_ready" || run.VerifierRuns != 5 {
		t.Fatal(run, err)
	}
	e.Propose = func(context.Context, ProposalInput) error { return nil }
	run, err = e.Run(context.Background(), "fix", contractFixture())
	if err != nil || run.Status != "rejected" {
		t.Fatal(run, err)
	}
}

func TestEvolutionVerifierMutationAndPostValidationTampering(t *testing.T) {
	root := evolutionRepo(t)
	ctx := context.Background()
	c := contractFixture()
	c.MaxCandidates = 1
	e := Evolver{Config: Config{Root: root, ApproveVerifiers: true}, Propose: goodProposal}
	run, err := e.Run(ctx, "fix", c)
	if err != nil || run.Status != "candidate_ready" {
		t.Fatal(run, err)
	}
	if err := durable.Replace(filepath.Join(run.ValidatedWorktree, "code.txt"), []byte("tampered\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(ctx, root, run.ID, 0, true); err == nil {
		t.Fatal("changed candidate activated")
	}
	c.Train[0].Command = `printf changed > code.txt`
	run, err = e.Run(ctx, "fix", c)
	if err == nil || run.Status == "candidate_ready" {
		t.Fatal("mutable baseline accepted", run, err)
	}
}

func TestEvolutionInfrastructureAndContractValidation(t *testing.T) {
	root := evolutionRepo(t)
	c := contractFixture()
	c.Train[0].Command = "command-that-does-not-exist-gopherharness"
	e := Evolver{Config: Config{Root: root, ApproveVerifiers: true}, Propose: goodProposal}
	run, err := e.Run(context.Background(), "fix", c)
	if err == nil || len(run.Candidates) != 0 || run.Baseline[0].Error == "" {
		t.Fatal(run, err)
	}
	for _, mutate := range []func(*EvalContract){func(c *EvalContract) { c.Holdout[0].ID = c.Train[0].ID }, func(c *EvalContract) { c.Holdout[0].Command = c.Train[0].Command }, func(c *EvalContract) { c.AllowedPaths = []string{"../outside"} }, func(c *EvalContract) { c.MaxCandidates = 21 }} {
		c := contractFixture()
		mutate(&c)
		if err := validateContract(&c); err == nil {
			t.Fatal("invalid contract accepted", c)
		}
	}
}
