//go:build !linux

package auth

import "time"

// threadCPU is not available here: the timing test falls back to wall time.
func threadCPU() (time.Duration, bool) { return 0, false }
