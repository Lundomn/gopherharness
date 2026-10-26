package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
)

type Activation struct {
	RunID        string    `json:"run_id"`
	CandidateID  string    `json:"candidate_id"`
	Worktree     string    `json:"worktree"`
	BaseRevision string    `json:"base_revision"`
	PatchDigest  string    `json:"patch_digest"`
	ActivatedAt  time.Time `json:"activated_at"`
}

var regexpCandidate = regexp.MustCompile(`^candidate-[0-9]{2}$`)

type ActivationState struct {
	Version uint64       `json:"version"`
	Active  *Activation  `json:"active,omitempty"`
	History []Activation `json:"history,omitempty"`
}

func activationFile(root string) durable.File[ActivationState] {
	return durable.File[ActivationState]{Path: filepath.Join(root, ".pico", "maintenance", "active.json")}
}
func Active(ctx context.Context, root string) (ActivationState, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return ActivationState{}, err
	}
	return activationFile(abs).Read(ctx)
}

func validateReady(ctx context.Context, root, id string) (Activation, error) {
	run, err := ReadEvolution(root, id)
	if err != nil {
		return Activation{}, err
	}
	if run.Status != "candidate_ready" || run.SelectedCandidate == "" || len(run.Validation) != len(run.Contract.Holdout) || passCount(run.Validation) != len(run.Validation) {
		return Activation{}, errors.New("candidate has no complete successful held-out validation")
	}
	var candidate *CandidateResult
	for i := range run.Candidates {
		if run.Candidates[i].ID == run.SelectedCandidate {
			candidate = &run.Candidates[i]
			break
		}
	}
	if candidate == nil || candidate.Status != "train_accepted" || !improves(run.Baseline, candidate.Train) {
		return Activation{}, errors.New("candidate train evidence is invalid")
	}
	for i, c := range run.Contract.Holdout {
		if run.Validation[i].ID != c.ID {
			return Activation{}, errors.New("held-out case identity changed")
		}
	}
	if !regexpCandidate.MatchString(candidate.ID) || candidate.PatchPath != filepath.Join(run.Root, ".pico", "maintenance", run.ID, candidate.ID, "candidate.patch") {
		return Activation{}, errors.New("candidate patch path does not belong to run")
	}
	expected := filepath.Join(run.Root, ".pico", "maintenance", run.ID, "validated")
	if filepath.Clean(run.ValidatedWorktree) != expected {
		return Activation{}, errors.New("validated worktree path does not belong to run")
	}
	patch, paths, err := captureCandidatePatch(ctx, expected, run.BaseRevision, false)
	if err != nil {
		return Activation{}, err
	}
	if digest(patch) != candidate.PatchDigest {
		return Activation{}, errors.New("candidate changed after validation")
	}
	if err := allowedChanges(paths, run.Contract); err != nil {
		return Activation{}, err
	}
	storedPatch, err := os.ReadFile(candidate.PatchPath)
	if err != nil {
		return Activation{}, err
	}
	if digest(storedPatch) != candidate.PatchDigest {
		return Activation{}, errors.New("candidate patch artifact changed")
	}
	return Activation{RunID: run.ID, CandidateID: candidate.ID, Worktree: expected, BaseRevision: run.BaseRevision, PatchDigest: candidate.PatchDigest, ActivatedAt: time.Now().UTC()}, nil
}

// Activate switches an explicit local worktree pointer, not the source checkout
// or a running process. Launchers call ResolveActiveRoot for a new process.
// The expected version prevents an old confirmation replacing a newer choice.
func Activate(ctx context.Context, root, runID string, expectedVersion uint64, approved bool) (ActivationState, error) {
	if !approved {
		return ActivationState{}, errors.New("candidate activation requires explicit approval")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ActivationState{}, err
	}
	// The activation operation lock covers revalidation plus pointer update;
	// no model or verifier runs while it is held.
	release, err := durable.Lease(ctx, filepath.Join(abs, ".pico", "maintenance", "activation.operation.lock"))
	if err != nil {
		return ActivationState{}, err
	}
	defer release()
	activation, err := validateReady(ctx, abs, runID)
	if err != nil {
		return ActivationState{}, err
	}
	var out ActivationState
	err = activationFile(abs).Update(ctx, func(s *ActivationState) error {
		if s.Version != expectedVersion {
			return fmt.Errorf("activation version changed: expected %d, current %d", expectedVersion, s.Version)
		}
		if s.Active != nil && s.Active.RunID == runID {
			out = *s
			return nil
		}
		if s.Active != nil {
			s.History = append(s.History, *s.Active)
		}
		s.Active = &activation
		s.Version++
		out = *s
		return nil
	})
	return out, err
}

func Rollback(ctx context.Context, root string, expectedVersion uint64, approved bool) (ActivationState, error) {
	if !approved {
		return ActivationState{}, errors.New("candidate rollback requires explicit approval")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ActivationState{}, err
	}
	release, err := durable.Lease(ctx, filepath.Join(abs, ".pico", "maintenance", "activation.operation.lock"))
	if err != nil {
		return ActivationState{}, err
	}
	defer release()
	current, err := activationFile(abs).Read(ctx)
	if err != nil {
		return current, err
	}
	if current.Version != expectedVersion || current.Active == nil {
		return current, errors.New("activation changed or no active candidate")
	}
	var previous *Activation
	if len(current.History) > 0 {
		value := current.History[len(current.History)-1]
		checked, err := validateReady(ctx, abs, value.RunID)
		if err != nil {
			return current, fmt.Errorf("rollback target no longer valid: %w", err)
		}
		if checked.PatchDigest != value.PatchDigest {
			return current, errors.New("rollback patch digest changed")
		}
		previous = &checked
	}
	var out ActivationState
	err = activationFile(abs).Update(ctx, func(s *ActivationState) error {
		if s.Version != expectedVersion {
			return errors.New("activation version changed")
		}
		s.Active = previous
		if len(s.History) > 0 {
			s.History = s.History[:len(s.History)-1]
		}
		s.Version++
		out = *s
		return nil
	})
	return out, err
}

// ResolveActiveRoot validates evidence and immutable candidate contents on each
// launch. Editing a validated candidate invalidates activation until rerun.
func ResolveActiveRoot(ctx context.Context, root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	s, err := Active(ctx, abs)
	if err != nil {
		return "", err
	}
	if s.Active == nil {
		return abs, nil
	}
	checked, err := validateReady(ctx, abs, s.Active.RunID)
	if err != nil {
		return "", err
	}
	if checked.PatchDigest != s.Active.PatchDigest || checked.Worktree != s.Active.Worktree {
		return "", errors.New("active candidate identity changed")
	}
	return checked.Worktree, nil
}

// ReadContract is a bounded strict decoder for the local CLI contract.
func ReadContract(path string) (EvalContract, error) {
	var c EvalContract
	info, err := os.Stat(path)
	if err != nil {
		return c, err
	}
	if info.Size() > 1<<20 {
		return c, errors.New("evaluation contract exceeds 1 MiB")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return c, errors.New("contract must contain exactly one JSON object")
	}
	return c, validateContract(&c)
}
