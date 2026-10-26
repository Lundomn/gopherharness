package runtime

import (
	"context"
	"fmt"

	"github.com/Lundomn/gopherharness/internal/tools"
)

// The structured code comes from Wait/ProcessState, never text printed by the
// command. Readiness consumes this through the builtin tool's trusted dispatch.
func (a *Agent) ExecuteShellResult(ctx context.Context, command string, seconds int) (tools.ShellResult, error) {
	result, err := a.Sandbox.Run(ctx, command, a.Workspace.Root, a.ShellEnvironment(), seconds)
	return tools.ShellResult{
		Output:   fmt.Sprintf("exit_code: %d\nsandbox_backend: %s\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Backend, outputOrEmpty(result.Stdout), outputOrEmpty(result.Stderr)),
		ExitCode: result.ExitCode,
	}, err
}
