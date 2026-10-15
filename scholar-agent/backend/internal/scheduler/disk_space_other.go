//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !openbsd

package scheduler

func detectDiskFreeGB(path string) float64 {
	// Unknown capacity must not satisfy the default full-reproduction disk
	// threshold. Windows and other unsupported hosts conservatively use smoke
	// mode unless the user explicitly lowers the resource thresholds.
	return 0
}
