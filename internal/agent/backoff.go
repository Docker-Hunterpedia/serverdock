package agent

import (
	"math/rand/v2"
	"time"
)

// Backoff is exponential backoff with full jitter: Next returns a uniform
// random duration in [0, min(Max, Min*2^n)).
type Backoff struct {
	Min, Max time.Duration
	// Rand returns a float64 in [0,1); defaults to math/rand/v2.
	Rand func() float64
	n    uint
}

// DefaultBackoff is 1 s growing to 30 s.
func DefaultBackoff() Backoff { return Backoff{Min: time.Second, Max: 30 * time.Second} }

// Ceiling returns the current upper bound without advancing.
func (b *Backoff) Ceiling() time.Duration {
	c := b.Max
	if b.n < 32 {
		if v := b.Min << b.n; v > 0 && v < b.Max {
			c = v
		}
	}
	return c
}

// Next returns the next delay and advances the schedule.
func (b *Backoff) Next() time.Duration {
	r := b.Rand
	if r == nil {
		r = rand.Float64
	}
	d := time.Duration(r() * float64(b.Ceiling()))
	if b.n < 32 {
		b.n++
	}
	return d
}

// Reset restarts the schedule.
func (b *Backoff) Reset() { b.n = 0 }
