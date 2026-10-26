package mcpbridge

import "os/exec"

func ownProcessGroup(cmd *exec.Cmd) {}

// Windows currently owns only the server process. A Job Object adapter is
// needed before making process-tree cleanup guarantees on that platform.
func killOwnedProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
