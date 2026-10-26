package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/sandbox"
	"github.com/Lundomn/gopherharness/internal/store"
	"github.com/Lundomn/gopherharness/internal/workspace"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Root, BaseRevision, IssueEndpoint, IssueToken, IssueRepo string
	VerifyCommands                                           []string
	Sandbox                                                  sandbox.Config
	TimeoutSeconds                                           int
	ApproveVerifiers                                         bool
}
type Repair func(context.Context, string, string) error
type Result struct {
	ID, Status, BaseRevision, PatchPath, RepairPath, VerifierPath string
	Checks                                                        []sandbox.Result
	Error                                                         string
}
type Manager struct {
	Config Config
	Repair Repair
	HTTP   *http.Client
}

func git(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	cmd.Env = workspace.ShellEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", args[0], err, out)
	}
	return out, nil
}
func (m *Manager) Issue(ctx context.Context, title, body string) (string, error) {
	if strings.TrimSpace(title) == "" || m.Config.IssueEndpoint == "" || m.Config.IssueToken == "" {
		return "", errors.New("issue title and configured endpoint/token required")
	}
	raw, _ := json.Marshal(map[string]string{"title": title, "body": body, "repo": m.Config.IssueRepo, "access_token": m.Config.IssueToken})
	req, err := http.NewRequestWithContext(ctx, "POST", m.Config.IssueEndpoint, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("issue API HTTP %d; verify remote state before resubmission", resp.StatusCode)
	}
	var data struct {
		URL string `json:"html_url"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return "", err
	}
	if data.URL == "" {
		return "", errors.New("issue response lacks URL; reconcile before retry")
	}
	return data.URL, nil
}
func (m *Manager) Fix(ctx context.Context, request string) (result Result, err error) {
	if !m.Config.ApproveVerifiers {
		return result, errors.New("verification commands require explicit approval")
	}
	if m.Repair == nil || len(m.Config.VerifyCommands) == 0 {
		return result, errors.New("repair callback and trusted verification commands required")
	}
	timeout := m.Config.TimeoutSeconds
	if timeout <= 0 {
		timeout = 600
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	result = Result{ID: store.NewID("fix"), Status: "blocked"}
	dir := filepath.Join(m.Config.Root, ".pico", "maintenance", result.ID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			result.Error = err.Error()
		}
		data, _ := json.MarshalIndent(result, "", "  ")
		writeErr := durable.Replace(filepath.Join(dir, "result.json"), data)
		err = errors.Join(err, writeErr)
	}()
	base := m.Config.BaseRevision
	if base == "" {
		base = "HEAD"
	}
	sha, e := git(ctx, m.Config.Root, "rev-parse", "--verify", base+"^{commit}")
	if e != nil {
		return result, e
	}
	result.BaseRevision = strings.TrimSpace(string(sha))
	result.RepairPath = filepath.Join(dir, "repair")
	result.VerifierPath = filepath.Join(dir, "verify")
	for _, path := range []string{result.RepairPath, result.VerifierPath} {
		if _, err = git(ctx, m.Config.Root, "worktree", "add", "--detach", path, result.BaseRevision); err != nil {
			return result, err
		}
	}
	// No auto commit, push, PR or merge. Worktrees and artifacts remain reviewable.
	if err = m.Repair(ctx, result.RepairPath, request); err != nil {
		return result, err
	}
	if _, err = git(ctx, result.RepairPath, "add", "-N", "--", "."); err != nil {
		return result, err
	}
	patch, e := git(ctx, result.RepairPath, "diff", "--binary", "HEAD", "--", ".", ":(exclude).pico")
	if e != nil {
		return result, e
	}
	if len(patch) == 0 {
		return result, errors.New("repair produced no candidate patch")
	}
	result.PatchPath = filepath.Join(dir, "candidate.patch")
	if err = durable.Replace(result.PatchPath, patch); err != nil {
		return result, err
	}
	if _, err = git(ctx, result.VerifierPath, "apply", "--check", result.PatchPath); err != nil {
		result.Status = "verification_failed"
		return result, err
	}
	if _, err = git(ctx, result.VerifierPath, "apply", result.PatchPath); err != nil {
		result.Status = "verification_failed"
		return result, err
	}
	runner := sandbox.New(m.Config.Sandbox)
	for _, command := range m.Config.VerifyCommands {
		r, e := runner.Run(ctx, command, result.VerifierPath, workspace.ShellEnv(), timeout)
		result.Checks = append(result.Checks, r)
		if e != nil || r.ExitCode != 0 {
			result.Status = "verification_failed"
			if e == nil {
				e = errors.New("verifier returned nonzero exit")
			}
			return result, e
		}
	}
	result.Status = "candidate_ready"
	return result, nil
}
