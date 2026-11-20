package limits

import (
	"net"
	"sync"
	"time"
)

// Backoff is a lazy exponential back-off per key: a failure count, the time of
// the last failure and the next allowed time. No goroutine, no timer: everything
// is decided when a caller asks. It bounds guessing of passwords, second-factor
// codes and setup tokens without ever locking an account out for good.
//
// After free failures, each further failure blocks the key for base, then twice
// that, and so on up to max. A success calls Reset.
//
// The map holds at most MaxKeys keys. To make room, entries with no failure for
// ForgetAfter go first, then the unblocked key whose last failure is oldest. A
// blocked key is never evicted (that would hand its owner a fresh budget). When
// the map is full of blocked keys a new key is treated as blocked: the limiter
// fails closed. A caller that knows the key is legitimate (an IP that completed a
// full login recently) passes bypassFull to be admitted anyway; such keys are few
// by construction, and a hard overflow limit still bounds them.
type Backoff struct {
	mu     sync.Mutex
	m      map[string]*backoffEntry
	free   int
	base   time.Duration
	max    time.Duration
	maxKey int
	now    func() time.Time
}

type backoffEntry struct {
	fails int
	last  time.Time
	next  time.Time
}

const (
	// ForgetAfter is how long a key with no new failure is remembered.
	ForgetAfter = 24 * time.Hour
	// bypassOverflow is how far past MaxKeys bypassFull keys may push the map.
	bypassOverflow = 256
)

// NewBackoff returns a limiter allowing free failures per key, then blocking for
// base, doubling up to max. now may be nil (time.Now).
func NewBackoff(free int, base, max time.Duration, now func() time.Time) *Backoff {
	if now == nil {
		now = time.Now
	}
	return &Backoff{m: map[string]*backoffEntry{}, free: free, base: base, max: max, maxKey: MaxKeys, now: now}
}

// Check returns how long key must wait before another attempt (0: go ahead).
func (b *Backoff) Check(key string, bypassFull bool) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	e, ok := b.m[key]
	if ok && now.Sub(e.last) >= ForgetAfter {
		delete(b.m, key)
		ok = false
	}
	if ok {
		return max(0, e.next.Sub(now))
	}
	if len(b.m) >= b.maxKey && !bypassFull && !b.hasRoom(now) {
		return b.untilSoonest(now) // fail closed
	}
	return 0
}

// Fail records a failed attempt for key and returns the wait it now owes (0 while
// within the free failures). It reports tracked=false when the map is full of
// blocked keys and the key could not be admitted; the caller must then treat the
// key as blocked.
func (b *Backoff) Fail(key string, bypassFull bool) (wait time.Duration, tracked bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	e, ok := b.m[key]
	if ok && now.Sub(e.last) >= ForgetAfter {
		delete(b.m, key)
		ok = false
	}
	if !ok {
		if len(b.m) >= b.maxKey && !b.makeRoom(now) {
			if !bypassFull || len(b.m) >= b.maxKey+bypassOverflow {
				return b.untilSoonest(now), false
			}
		}
		e = &backoffEntry{}
		b.m[key] = e
	}
	e.fails++
	e.last = now
	if e.fails > b.free {
		w := b.base
		for i := b.free + 1; i < e.fails && w < b.max; i++ {
			w *= 2
		}
		e.next = now.Add(min(w, b.max))
	}
	return max(0, e.next.Sub(now)), true
}

// Reset forgets key (a success).
func (b *Backoff) Reset(key string) {
	b.mu.Lock()
	delete(b.m, key)
	b.mu.Unlock()
}

// Len returns the number of tracked keys.
func (b *Backoff) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.m)
}

// hasRoom reports whether makeRoom would succeed, without changing anything.
func (b *Backoff) hasRoom(now time.Time) bool {
	for _, e := range b.m {
		if now.Sub(e.last) >= ForgetAfter || !e.next.After(now) {
			return true
		}
	}
	return false
}

// makeRoom frees one slot: expired entries first, otherwise the unblocked key
// with the oldest last failure. It never removes a blocked key. Caller holds mu.
func (b *Backoff) makeRoom(now time.Time) bool {
	var oldest string
	var oldestAt time.Time
	for k, e := range b.m {
		if now.Sub(e.last) >= ForgetAfter {
			delete(b.m, k)
			return true
		}
		if !e.next.After(now) && (oldest == "" || e.last.Before(oldestAt)) {
			oldest, oldestAt = k, e.last
		}
	}
	if oldest == "" {
		return false
	}
	delete(b.m, oldest)
	return true
}

// untilSoonest is how long until the first blocked key is allowed again (at
// least one second): what a refused new key is told. Caller holds mu.
func (b *Backoff) untilSoonest(now time.Time) time.Duration {
	best := b.max
	for _, e := range b.m {
		if d := e.next.Sub(now); d < best {
			best = d
		}
	}
	return max(best, time.Second)
}

// Key48 normalises an address for the coarse limiter: IPv4 as is, IPv4-mapped
// IPv6 as IPv4, other IPv6 masked to its /48 (a typical site allocation holds
// 65,536 /64s, so per-/64 keys alone are cheap to rotate).
func Key48(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(48, 128)).String() + "/48"
}
