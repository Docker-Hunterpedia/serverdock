//go:build linux

package auth

import (
	"syscall"
	"time"
)

// threadCPU is the CPU time (user plus system) the calling OS thread has used.
// The caller must have locked its goroutine to the thread. Unlike wall time it
// does not grow when the machine is busy with other processes, so a timing
// comparison of two code paths measures the work they do.
func threadCPU() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_THREAD, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
