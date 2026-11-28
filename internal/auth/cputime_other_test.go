//go:build !unix

package auth

import "time"

// processCPU is not available here: the timing test falls back to wall time.
func processCPU() (time.Duration, bool) { return 0, false }
