//go:build unix

package harness

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so that killing it
// takes its helpers with it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the child's whole process group. ESRCH means the
// group is already gone, which is success for a cancel.
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return p.Kill()
	}
	return nil
}
