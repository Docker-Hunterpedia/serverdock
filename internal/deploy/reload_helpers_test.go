package deploy

import (
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
)

// Helpers of the reload-cases measurement (reload_cases_integration_test.go). They live in
// an untagged file so their own unit tests run in the normal test run.

// loadCounter counts Caddy config loads in this process without a log writer:
//   - attempts are the calls of proxy.Load (proxy.LoadAttempts, a plain atomic counter),
//     whether the load succeeds or fails. An earlier version counted Caddy's "admin
//     endpoint disabled" stderr lines, but Caddy pools its log writer per process, so a
//     counter installed after any earlier test's load saw none of them;
//   - a successful load installs a new root context (caddy.ActiveContext), so a poller that
//     sees it change counts the successful ones (a load takes several milliseconds; the
//     poller ticks every millisecond).
//
// Failed = attempts - successful. Both are relative to the moment the counter started, so
// it can start at any time.
type loadCounter struct {
	base ctrBase
	ok   atomic.Int64
	// attempts is read through snap; kept as a method value for the unit test's fake.
	attemptsFn func() int64
	stop       chan struct{}
	wg         sync.WaitGroup
}

type ctrBase struct{ attempts int64 }

func (c *loadCounter) attempts() int64 { return c.attemptsFn() - c.base.attempts }

func startLoadCounter() (*loadCounter, error) {
	c := &loadCounter{attemptsFn: proxy.LoadAttempts, stop: make(chan struct{})}
	c.base.attempts = proxy.LoadAttempts()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		last := caddy.ActiveContext().Context
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-tick.C:
			}
			if cur := caddy.ActiveContext().Context; cur != last {
				last = cur
				c.ok.Add(1)
			}
		}
	}()
	return c, nil
}

// snap returns the loads so far: successful and failed.
func (c *loadCounter) snap() (ok, failed int64) {
	ok, a := c.ok.Load(), c.attempts()
	return ok, max(a-ok, 0)
}

// consistent reports that no more loads succeeded than were attempted (before snap clamps).
func (c *loadCounter) consistent() bool { return c.attempts() >= c.ok.Load() }

// Close stops the poller.
func (c *loadCounter) Close() {
	close(c.stop)
	c.wg.Wait()
}

// cpuTime is the user plus system CPU time of this process.
func cpuTime() (time.Duration, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, err
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), nil
}

// netCPU is the CPU of an operation that ran for dur and used gross CPU, minus what the
// load generator alone burns in that time (rate = CPU per wall second measured with no
// operation). It is clamped at 0: the difference of two noisy numbers can go negative.
func netCPU(gross, dur time.Duration, rate float64) time.Duration {
	n := gross - time.Duration(rate*float64(dur))
	return max(n, 0)
}

// ratio is a/b, or 0 when b is 0 (no loads in an operation that never reloads).
func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
