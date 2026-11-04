//go:build windows

package harness

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on Windows: there is no process group to kill, so
// cancellation kills the child only. The supported delivery hosts are unix.
func setProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
