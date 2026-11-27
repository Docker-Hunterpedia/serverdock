package deploy

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// Helpers of the reload-cases measurement (reload_cases_integration_test.go). They live in
// an untagged file so their own unit tests run in the normal test run.

// loadCounter counts Caddy config loads in this process without touching the product:
//   - every load logs "admin endpoint disabled" once, whether it succeeds or fails (the
//     admin app is provisioned before the servers start and bind), so the number of those
//     lines on stderr is the number of load ATTEMPTS;
//   - a successful load installs a new root context (caddy.ActiveContext), so a poller that
//     sees it change counts the successful ones (a load takes several milliseconds; the poller ticks every millisecond).
//
// Failed = attempts - successful. It must be started BEFORE the first load: Caddy keeps the log
// writer it opened at its first load, so a later counter sees successes but no attempts. The counter replaces os.Stderr for its lifetime, passes
// every other line through, and puts the original back on Close.
type loadCounter struct {
	attempts atomic.Int64
	ok       atomic.Int64
	orig     *os.File
	w        *os.File
	stop     chan struct{}
	wg       sync.WaitGroup
}

const adminLogLine = "admin endpoint disabled"

func startLoadCounter() (*loadCounter, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	c := &loadCounter{orig: os.Stderr, w: w, stop: make(chan struct{})}
	os.Stderr = w
	c.wg.Add(2)
	go func() {
		defer c.wg.Done()
		defer r.Close()
		c.countLines(r, c.orig)
	}()
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

// countLines reads r to its end, counting the load lines and forwarding every other line to
// out (the HTTP/2 and HTTP/3 "skipped" warnings of every load are dropped as noise).
func (c *loadCounter) countLines(r io.Reader, out io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, adminLogLine):
			c.attempts.Add(1)
		case strings.Contains(line, "skipped because it requires TLS"):
		default:
			fmt.Fprintln(out, line)
		}
	}
}

// snap returns the loads so far: successful and failed.
func (c *loadCounter) snap() (ok, failed int64) {
	ok, a := c.ok.Load(), c.attempts.Load()
	return ok, max(a-ok, 0)
}

// consistent reports that no more loads succeeded than were attempted (before snap clamps).
func (c *loadCounter) consistent() bool { return c.attempts.Load() >= c.ok.Load() }

// Close restores os.Stderr and stops both goroutines.
func (c *loadCounter) Close() {
	os.Stderr = c.orig
	close(c.stop)
	_ = c.w.Close()
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
