// Package limits holds the small, goroutine-free building blocks that bound
// what an unauthenticated or low-trust peer can cost the control plane: a keyed
// token bucket, client-IP normalisation, and a listener that caps concurrent
// connections globally and per client IP. Everything is lazy: no timers, no
// background cleanup (docs/architecture.md, footprint rules).
package limits

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MaxKeys bounds every Keyed limiter's map.
const MaxKeys = 4096

// Keyed is a token bucket per key, with a bounded map and lazy eviction.
// A key that is absent behaves exactly like a full bucket, so evicting idle
// keys never changes a decision.
type Keyed struct {
	mu     sync.Mutex
	m      map[string]*bucket
	rate   float64 // tokens per second
	burst  float64
	maxKey int
	now    func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewKeyed returns a limiter that refills perMinute tokens per minute up to
// burst, tracking at most maxKeys keys. now may be nil (time.Now).
func NewKeyed(perMinute float64, burst, maxKeys int, now func() time.Time) *Keyed {
	if now == nil {
		now = time.Now
	}
	if maxKeys <= 0 || maxKeys > MaxKeys {
		maxKeys = MaxKeys
	}
	return &Keyed{m: map[string]*bucket{}, rate: perMinute / 60, burst: float64(burst), maxKey: maxKeys, now: now}
}

func (k *Keyed) refill(b *bucket, now time.Time) {
	if dt := now.Sub(b.last).Seconds(); dt > 0 {
		b.tokens = min(k.burst, b.tokens+dt*k.rate)
	}
	b.last = now
}

// Blocked reports whether key has no token left, without consuming one. It
// returns how long until a token is available.
func (k *Keyed) Blocked(key string) (bool, time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	b, ok := k.m[key]
	if !ok {
		// Fail toward refusing: with the map full of blocked keys a new key cannot
		// be tracked, so it is treated as blocked until a slot's block expires.
		if len(k.m) >= k.maxKey && k.allBlocked(now) {
			return true, k.untilFirstToken(now)
		}
		return false, 0
	}
	k.refill(b, now)
	if b.tokens >= 1 {
		return false, 0
	}
	return true, time.Duration((1 - b.tokens) / k.rate * float64(time.Second))
}

// allBlocked reports whether every tracked key is blocked. Caller holds mu.
func (k *Keyed) allBlocked(now time.Time) bool {
	for _, b := range k.m {
		if k.tokensAt(b, now) >= 1 {
			return false
		}
	}
	return true
}

// untilFirstToken is how long until the soonest blocked key gets a token.
func (k *Keyed) untilFirstToken(now time.Time) time.Duration {
	best := time.Duration(1<<62 - 1)
	for _, b := range k.m {
		if d := time.Duration((1 - k.tokensAt(b, now)) / k.rate * float64(time.Second)); d < best {
			best = d
		}
	}
	return max(best, time.Second)
}

// Take consumes one token, creating the key if needed. It reports whether a
// token was available. When the map is full of blocked keys a new key is
// refused (false): a blocked bucket is never evicted to make room.
func (k *Keyed) Take(key string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	b, ok := k.m[key]
	if !ok {
		if len(k.m) >= k.maxKey && !k.evict(now) {
			return false
		}
		b = &bucket{tokens: k.burst, last: now}
		k.m[key] = b
	}
	k.refill(b, now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// tokensAt is a bucket's token count at now, without touching it.
func (k *Keyed) tokensAt(b *bucket, now time.Time) float64 {
	return min(k.burst, b.tokens+max(0, now.Sub(b.last).Seconds())*k.rate)
}

// evict makes room: it drops every idle (full) bucket, and if none is idle the
// least recently touched bucket that is NOT blocked. A currently blocked bucket
// is never evicted (that would hand its owner a fresh budget). It reports
// whether room was made; false means every key is blocked. Caller holds mu.
func (k *Keyed) evict(now time.Time) bool {
	var oldest string
	var oldestAt time.Time
	freed := false
	for key, b := range k.m {
		t := k.tokensAt(b, now) // read-only: the scan must not touch b.last, which orders them
		switch {
		case t >= k.burst:
			delete(k.m, key)
			freed = true
		case t >= 1 && (oldest == "" || b.last.Before(oldestAt)):
			oldest, oldestAt = key, b.last
		}
	}
	if !freed && oldest != "" {
		delete(k.m, oldest)
		freed = true
	}
	return freed
}

// Len returns the number of tracked keys.
func (k *Keyed) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.m)
}

// Key normalises an address for limiting: IPv4 as is, IPv4-mapped IPv6 as
// IPv4, other IPv6 masked to its /64 (one customer is usually a whole /64, so
// per-address keys would be free to rotate).
func Key(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// AddrKey is Key for a net.Addr (host:port); an unparsable address maps to its
// own string so it is still counted.
func AddrKey(a net.Addr) string {
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		host = a.String()
	}
	if ip := net.ParseIP(host); ip != nil {
		return Key(ip)
	}
	return host
}

// ClientIP returns the limiting key for the request's client. It uses
// RemoteAddr only, except when trustLoopbackProxy is set and the peer is a
// loopback address (our own Caddy): then the last hop in X-Forwarded-For is
// used. X-Forwarded-For from any other peer is ignored.
func ClientIP(r *http.Request, trustLoopbackProxy bool) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return host
	}
	if trustLoopbackProxy && peer.IsLoopback() {
		if vals := r.Header.Values("X-Forwarded-For"); len(vals) > 0 {
			parts := strings.Split(vals[len(vals)-1], ",")
			if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
				return Key(ip)
			}
		}
	}
	return Key(peer)
}

// Listener caps concurrent connections: at most global in total (Accept waits
// when full) and at most perIP per client address (a connection over the cap is
// accepted and closed at once, inside Accept, before any handler or TLS
// handshake work). Zero disables a cap.
type Listener struct {
	net.Listener
	perIP int
	key   func(net.Addr) string
	sem   chan struct{}
	done  chan struct{}
	once  sync.Once

	mu     sync.Mutex
	counts map[string]int
}

// NewListener wraps ln. key maps a peer address to its counting key; nil uses AddrKey.
func NewListener(ln net.Listener, global, perIP int, key func(net.Addr) string) *Listener {
	if key == nil {
		key = AddrKey
	}
	l := &Listener{Listener: ln, perIP: perIP, key: key, done: make(chan struct{}), counts: map[string]int{}}
	if global > 0 {
		l.sem = make(chan struct{}, global)
	}
	return l
}

// Accept implements net.Listener.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		if l.sem != nil {
			select {
			case l.sem <- struct{}{}:
			case <-l.done:
				return nil, net.ErrClosed
			}
		}
		c, err := l.Listener.Accept()
		if err != nil {
			l.releaseSlot()
			return nil, err
		}
		k := ""
		if l.perIP > 0 {
			k = l.key(c.RemoteAddr())
			l.mu.Lock()
			if l.counts[k] >= l.perIP {
				l.mu.Unlock()
				_ = c.Close()
				l.releaseSlot()
				continue
			}
			l.counts[k]++
			l.mu.Unlock()
		}
		return &conn{Conn: c, l: l, key: k}, nil
	}
}

// Close implements net.Listener.
func (l *Listener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

func (l *Listener) releaseSlot() {
	if l.sem != nil {
		<-l.sem
	}
}

// Active returns the number of counted connections for key (tests).
func (l *Listener) Active(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.counts[key]
}

type conn struct {
	net.Conn
	l    *Listener
	key  string
	once sync.Once
}

func (c *conn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		if c.l.perIP > 0 {
			c.l.mu.Lock()
			if c.l.counts[c.key]--; c.l.counts[c.key] <= 0 {
				delete(c.l.counts, c.key)
			}
			c.l.mu.Unlock()
		}
		c.l.releaseSlot()
	})
	return err
}
