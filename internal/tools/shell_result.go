package tools

import (
	"context"
	"encoding/json"
)

// ShellResult carries the process exit status separately from untrusted output.
// Hosts without this optional interface can still execute commands, but their
// formatted strings cannot establish successful verification.
type ShellResult struct {
	Output   string
	ExitCode int
}

type shellResultHost interface {
	ExecuteShellResult(context.Context, string, int) (ShellResult, error)
}

type shellRecordingHost struct {
	Host
	metadata map[string]any
}

func (h shellRecordingHost) ExecuteShell(ctx context.Context, command string, seconds int) (string, error) {
	structured, ok := h.Host.(shellResultHost)
	if !ok {
		return h.Host.ExecuteShell(ctx, command, seconds)
	}
	result, err := structured.ExecuteShellResult(ctx, command, seconds)
	h.metadata["shell_exit_code"] = result.ExitCode
	h.metadata["shell_completed"] = err == nil
	return result.Output, err
}

// SuccessfulShellExit accepts only metadata captured by this runtime dispatch;
// tool stdout/stderr and model-generated argument strings are not consulted.
func SuccessfulShellExit(metadata map[string]any) bool {
	completed, _ := metadata["shell_completed"].(bool)
	if !completed {
		return false
	}
	switch exit := metadata["shell_exit_code"].(type) {
	case int:
		return exit == 0
	case float64: // Restored durable approval receipt decoded by encoding/json.
		return exit == 0
	case json.Number:
		n, err := exit.Int64()
		return err == nil && n == 0
	}
	return false
}
