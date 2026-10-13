//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !openbsd

package scheduler

import "testing"

func TestDetectDiskFreeGBUnsupportedHost(t *testing.T) {
	if got := detectDiskFreeGB(t.TempDir()); got != 0 {
		t.Fatalf("unsupported disk probe returned %v GiB, want 0", got)
	}
}
