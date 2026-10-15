//go:build unix

package scheduler

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestRepoCloneCancellationKillsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("requires a POSIX shell")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The descendant inherits stdout. Killing only the shell would leave the
	// pipe open until WaitDelay expires instead of finishing promptly.
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo ready; wait")
	configureRepoCloneCancellation(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("clone does not create a process group")
	}
	if err := cmd.Cancel(); err != nil {
		t.Fatalf("cancel before start: %v", err)
	}
	cmd.WaitDelay = 10 * time.Second
	// Force an output-copy goroutine, like the clone's CombinedOutput call.
	ready := make(chan struct{}, 1)
	cmd.Stdout = cloneReadyOutput{ready: ready}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil || pgid != cmd.Process.Pid {
		t.Fatalf("process group = %d, want %d (error: %v)", pgid, cmd.Process.Pid, err)
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		cancel()
		_ = cmd.Wait()
		t.Fatal("clone helper did not start")
	}
	cancel()
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	select {
	case err = <-finished:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-finished
		t.Fatal("descendant retained the output pipe after cancellation")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("canceled clone returned %v, want process exit error", err)
	}
}

type cloneReadyOutput struct{ ready chan<- struct{} }

func (w cloneReadyOutput) Write(p []byte) (int, error) {
	select {
	case w.ready <- struct{}{}:
	default:
	}
	return len(p), nil
}
