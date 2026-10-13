//go:build !unix

package scheduler

import (
	"context"
	"os/exec"
	"testing"
)

func TestRepoCloneCancellationKeepsCommandContextDefault(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "git", "--version")
	configureRepoCloneCancellation(cmd)
	if cmd.Cancel == nil {
		t.Fatal("CommandContext cancellation was removed")
	}
	if cmd.SysProcAttr != nil {
		t.Fatal("unexpected platform-specific process attributes")
	}
}
