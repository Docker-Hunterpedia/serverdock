package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Hasher runs argon2id behind a semaphore: one hash at a time, at most
// MaxWaiters callers waiting for it, each waiting at most Wait. A caller that
// would exceed either gets ErrBusy at once, which the HTTP layer answers with a
// fast 503. A hash holds about 19 MiB, so unbounded concurrency would let a
// handful of requests exhaust a 1 GB server; this bounds it to one.
//
// Back-off and every other cheap check must run before Hash or Verify: only a
// request that has passed them may take the slot.
type Hasher struct {
	slot       chan struct{}
	maxWaiters int
	wait       time.Duration

	mu      sync.Mutex
	waiting int

	// derive and freeMem are replaced in tests.
	derive  func(password, salt []byte, memKiB, t uint32, threads uint8, keyLen uint32) []byte
	freeMem func()
}

// Defaults of the semaphore.
const (
	MaxWaiters = 4
	MaxWait    = 2 * time.Second
)

// ErrBusy means the hasher is saturated: retry shortly (HTTP 503, Retry-After 2).
var ErrBusy = errors.New("auth: too many password checks in progress")

// NewHasher returns a Hasher with the default limits.
func NewHasher() *Hasher {
	return &Hasher{
		slot:       make(chan struct{}, 1),
		maxWaiters: MaxWaiters,
		wait:       MaxWait,
		derive: func(pw, salt []byte, memKiB, t uint32, threads uint8, keyLen uint32) []byte {
			return argon2.IDKey(pw, salt, t, memKiB, threads, keyLen)
		},
		freeMem: debug.FreeOSMemory,
	}
}

// acquire takes the slot, waiting if needed within the limits.
func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.slot <- struct{}{}:
		return nil
	default:
	}
	h.mu.Lock()
	if h.waiting >= h.maxWaiters {
		h.mu.Unlock()
		return ErrBusy
	}
	h.waiting++
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.waiting--; h.mu.Unlock() }()
	t := time.NewTimer(h.wait)
	defer t.Stop()
	select {
	case h.slot <- struct{}{}:
		return nil
	case <-t.C:
		return ErrBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release frees the slot. When nobody is queued, the heap the hash just used is
// returned to the OS from a new goroutine, so the response is never delayed by
// it (SYS18: argon2's 19 MiB would otherwise stay in RSS until the next GC cycle).
func (h *Hasher) release() {
	// Decide before freeing the slot: once it is free a queued caller may take it
	// at once, and a decision taken afterwards would depend on that race.
	h.mu.Lock()
	idle := h.waiting == 0
	h.mu.Unlock()
	<-h.slot
	if idle {
		go h.freeMem()
	}
}

// Hash returns the PHC string for password with a fresh random salt.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	key := h.derive([]byte(password), salt, argonMemoryKiB, argonTime, argonThreads, argonKeyLen)
	return encodePHC(phcParams{memory: argonMemoryKiB, time: argonTime, threads: argonThreads, salt: salt, key: key}), nil
}

// Verify reports whether password matches the PHC string. A malformed string is
// an error, not a mismatch.
func (h *Hasher) Verify(ctx context.Context, phc, password string) (bool, error) {
	p, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()
	got := h.derive([]byte(password), p.salt, p.memory, p.time, p.threads, uint32(len(p.key)))
	return equalKeys(got, p.key), nil
}

// dummyPHC is verified against when the user does not exist (or was reset), so
// the time and memory of the attempt match a real wrong password. It is a
// constant compiled into the binary, hashed with the current parameters; no
// password matches it that anyone knows.
const dummyPHC = "$argon2id$v=19$m=19456,t=2,p=1$ZHVtbXktc2FsdC0wMDAwMA$DUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUM"

// VerifyDummy spends the cost of one Verify against a hash nobody can match and
// always returns false (unless the hasher is busy: then ErrBusy, like Verify).
func (h *Hasher) VerifyDummy(ctx context.Context, password string) error {
	_, err := h.Verify(ctx, dummyPHC, password)
	return err
}
