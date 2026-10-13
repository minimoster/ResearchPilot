//go:build unix

package scheduler

import (
	"os/exec"
	"syscall"
)

// Kill the process group so git's transport helpers cannot outlive a clone.
func configureRepoCloneCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
