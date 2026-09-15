package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Config struct {
	Mode, Backend  string
	WorkspaceWrite bool
}
type Result struct {
	ExitCode                int
	Stdout, Stderr, Backend string
}
type Runner struct{ Config Config }

func New(cfg Config) *Runner {
	if cfg.Mode == "" {
		cfg.Mode = "off"
	}
	if cfg.Backend == "" {
		cfg.Backend = "auto"
	}
	return &Runner{Config: cfg}
}

func (r *Runner) Run(parent context.Context, command, cwd string, env []string, seconds int) (Result, error) {
	backend, err := r.resolveBackend()
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(seconds)*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if backend == "bubblewrap" {
		args := []string{"--die-with-parent", "--new-session", "--proc", "/proc", "--dev", "/dev", "--ro-bind", "/", "/"}
		if r.Config.WorkspaceWrite {
			args = append(args, "--bind", cwd, cwd)
		} else {
			args = append(args, "--ro-bind", cwd, cwd)
		}
		args = append(args, "--chdir", cwd, "/bin/sh", "-lc", command)
		cmd = exec.CommandContext(ctx, "bwrap", args...)
	} else if backend == "sandbox-exec" {
		sandboxCWD, resolveErr := filepath.EvalSymlinks(cwd)
		if resolveErr != nil {
			return Result{}, fmt.Errorf("resolve sandbox workspace: %w", resolveErr)
		}
		profile := `(version 1)
(deny default)
(import "system.sb")
(allow process*)
(allow file-read*)
(allow file-write* (subpath "/private/tmp") (subpath "/tmp"))
(allow network*)`
		if r.Config.WorkspaceWrite {
			profile += `
(allow file-write* (subpath (param "WORKSPACE")))`
		}
		cmd = exec.CommandContext(ctx, "sandbox-exec", "-D", "WORKSPACE="+sandboxCWD, "-p", profile, "/bin/sh", "-lc", command)
		cmd.Dir = sandboxCWD
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-lc", command)
		cmd.Dir = cwd
	}
	configureProcessGroup(cmd)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return Result{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		// CommandContext only terminates the shell process. Kill the process
		// group as well so children cannot keep stdout/stderr pipes open and
		// delay timeout reporting.
		killProcessGroup(cmd)
		err = <-done
	}
	result := Result{Stdout: stdout.String(), Stderr: stderr.String(), Backend: backend}
	if err != nil {
		if ctx.Err() != nil {
			return result, fmt.Errorf("shell timeout after %ds", seconds)
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
			return result, nil
		}
		return result, err
	}
	return result, nil
}

func (r *Runner) resolveBackend() (string, error) {
	if r.Config.Mode == "off" || r.Config.Backend == "none" {
		return "none", nil
	}
	backend := r.Config.Backend
	if backend == "auto" {
		if runtime.GOOS == "linux" {
			if _, err := exec.LookPath("bwrap"); err == nil {
				backend = "bubblewrap"
			}
		}
		if runtime.GOOS == "darwin" {
			if _, err := exec.LookPath("sandbox-exec"); err == nil {
				backend = "sandbox-exec"
			}
		}
		if backend == "auto" {
			if r.Config.Mode == "required" {
				return "", fmt.Errorf("sandbox required but no supported backend is available")
			}
			return "none", nil
		}
	}
	if backend == "bubblewrap" {
		if runtime.GOOS != "linux" {
			return "", fmt.Errorf("bubblewrap sandbox is only supported on Linux")
		}
		if _, err := exec.LookPath("bwrap"); err != nil {
			return "", fmt.Errorf("bubblewrap sandbox unavailable: %w", err)
		}
		return backend, nil
	}
	if backend == "sandbox-exec" {
		if runtime.GOOS != "darwin" {
			return "", fmt.Errorf("sandbox-exec is only supported on macOS")
		}
		if _, err := exec.LookPath("sandbox-exec"); err != nil {
			return "", fmt.Errorf("sandbox-exec unavailable: %w", err)
		}
		return backend, nil
	}
	return "", fmt.Errorf("unknown sandbox backend %q", strings.TrimSpace(backend))
}
