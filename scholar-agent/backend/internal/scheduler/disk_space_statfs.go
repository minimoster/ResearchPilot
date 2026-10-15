//go:build aix || darwin || dragonfly || freebsd || linux || openbsd

package scheduler

import "syscall"

func detectDiskFreeGB(path string) float64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	return float64(stat.Bavail) * float64(stat.Bsize) / 1024 / 1024 / 1024
}
