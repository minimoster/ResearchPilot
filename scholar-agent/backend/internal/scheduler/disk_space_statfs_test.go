//go:build aix || darwin || dragonfly || freebsd || linux || openbsd

package scheduler

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestDetectDiskFreeGBUsesAvailableBlocks(t *testing.T) {
	path := t.TempDir()
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		t.Fatal(err)
	}
	got := detectDiskFreeGB(path)
	// Available space may change between calls; a broad bound avoids depending
	// on unrelated filesystem activity while catching missing/incorrect units.
	totalGB := float64(stat.Blocks) * float64(stat.Bsize) / 1024 / 1024 / 1024
	if got <= 0 || got > totalGB {
		t.Fatalf("available space = %v GiB, total space = %v GiB", got, totalGB)
	}
	if got := detectDiskFreeGB(filepath.Join(path, "missing")); got != 0 {
		t.Fatalf("missing path returned %v GiB, want 0", got)
	}
}
