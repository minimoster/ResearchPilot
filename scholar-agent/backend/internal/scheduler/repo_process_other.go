//go:build !unix

package scheduler

import "os/exec"

func configureRepoCloneCancellation(cmd *exec.Cmd) {
	// Keep CommandContext's default Process.Kill cancellation on Windows and
	// other non-Unix hosts. It kills git itself, not the whole process tree;
	// the caller's WaitDelay bounds waiting on inherited output pipes.
}
