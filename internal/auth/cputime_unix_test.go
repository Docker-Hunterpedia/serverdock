//go:build unix

package auth

import (
	"syscall"
	"time"
)

// processCPU is the CPU time (user plus system) this whole process has used.
// Per-thread accounting would be wrong here: argon2's processBlocks runs every
// lane's segment in its own goroutine, even with one lane, so the passes do not
// run on the calling thread. Unlike wall time it does not grow when other
// processes keep the machine busy; what remains is the runtime's own GC and the
// race detector's work, which the interleaved samples share.
func processCPU() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
