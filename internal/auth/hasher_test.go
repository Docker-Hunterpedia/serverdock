package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var bg = context.Background()

func TestPHCRoundTripAndParsing(t *testing.T) {
	h := NewHasher()
	phc, err := h.Hash(bg, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("PHC %q", phc)
	}
	p, err := parsePHC(phc)
	if err != nil || len(p.salt) != 16 || len(p.key) != 32 || encodePHC(p) != phc {
		t.Fatalf("%+v %v", p, err)
	}
	if ok, err := h.Verify(bg, phc, "correct horse battery staple"); !ok || err != nil {
		t.Fatalf("right password: %v %v", ok, err)
	}
	for _, wrong := range []string{"", "correct horse battery stapl", "Correct horse battery staple"} {
		if ok, err := h.Verify(bg, phc, wrong); ok || err != nil {
			t.Errorf("%q: %v %v", wrong, ok, err)
		}
	}
	// Two hashes of one password differ (random salt).
	if phc2, _ := h.Hash(bg, "correct horse battery staple"); phc2 == phc {
		t.Error("same salt twice")
	}
	if _, err := parsePHC(dummyPHC); err != nil {
		t.Errorf("the dummy hash does not parse: %v", err)
	}
}

func TestPHCRejectsMalformedAndHostile(t *testing.T) {
	good := "$argon2id$v=19$m=19456,t=2,p=1$ZHVtbXktc2FsdC0wMDAwMA$DUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUM"
	if _, err := parsePHC(good); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"", "plain", "$argon2i$v=19$m=19456,t=2,p=1$ZHVtbXktc2FsdC0wMDAwMA$DUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUM",
		strings.Replace(good, "v=19", "v=16", 1),
		strings.Replace(good, "m=19456", "m=4294967295", 1), // would allocate 4 TiB
		strings.Replace(good, "m=19456", "m=999999999", 1),  // over our bound
		strings.Replace(good, "m=19456", "m=0", 1),
		strings.Replace(good, "t=2", "t=0", 1),
		strings.Replace(good, "t=2", "t=999999", 1),
		strings.Replace(good, "p=1", "p=0", 1),
		strings.Replace(good, "p=1", "p=200", 1),
		strings.Replace(good, "m=19456,t=2,p=1", "t=2,m=19456,p=1", 1),
		strings.Replace(good, "m=19456", "m=019456", 1),
		strings.Replace(good, "m=19456", "m=-1", 1),
		good + "$extra",
		strings.Replace(good, "$ZHVtbXktc2FsdC0wMDAwMA$", "$$", 1),   // empty salt
		strings.Replace(good, "$ZHVtbXktc2FsdC0wMDAwMA$", "$QQ$", 1), // 1-byte salt
		strings.Replace(good, "$ZHVtbXktc2FsdC0wMDAwMA$", "$!!!!$", 1),
		strings.Replace(good, "$DUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUM", "$QUJD", 1), // 3-byte key
	}
	for _, s := range bad {
		if _, err := parsePHC(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	h := NewHasher()
	if _, err := h.Verify(bg, "$argon2id$v=19$m=4294967295,t=2,p=1$ZHVtbXktc2FsdC0wMDAwMA$DUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUM", "x"); err == nil {
		t.Error("Verify tried a hostile hash")
	}
}

func FuzzParsePHC(f *testing.F) {
	f.Add("$argon2id$v=19$m=19456,t=2,p=1$ZHVtbXktc2FsdC0wMDAwMA$DUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUMMYKEYDUM")
	f.Add("$argon2id$v=19$m=1,t=1,p=1$$")
	f.Add("$$$$$$")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		p, err := parsePHC(s)
		if err != nil {
			return
		}
		if p.memory > maxArgonMemoryKiB || p.time > maxArgonTime || p.threads > maxArgonThreads || len(p.salt) < 8 || len(p.key) < 16 {
			t.Fatalf("accepted out-of-bounds %q: %+v", s, p)
		}
	})
}

// slowHasher returns a Hasher whose derive blocks until release is closed.
func slowHasher(release chan struct{}) (*Hasher, *atomic.Int32) {
	h := NewHasher()
	var running atomic.Int32
	h.derive = func(pw, salt []byte, m, tt uint32, th uint8, kl uint32) []byte {
		running.Add(1)
		<-release
		return make([]byte, kl)
	}
	h.freeMem = func() {}
	return h, &running
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// One hash runs and four callers wait; the next caller is refused immediately.
func TestHasherFast503(t *testing.T) {
	release := make(chan struct{})
	h, running := slowHasher(release)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	call := func() {
		defer wg.Done()
		_, err := h.Hash(bg, "pw")
		errs <- err
	}
	wg.Add(1)
	go call()
	waitFor(t, "the first hash to start", func() bool { return running.Load() == 1 })
	for i := 0; i < MaxWaiters; i++ {
		wg.Add(1)
		go call()
	}
	waitFor(t, "four waiters", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.waiting == MaxWaiters })

	t0 := time.Now()
	_, err := h.Hash(bg, "pw")
	if !errors.Is(err, ErrBusy) || time.Since(t0) > 50*time.Millisecond {
		t.Fatalf("over the limit: %v after %v, want ErrBusy at once", err, time.Since(t0))
	}
	if _, err := h.Verify(bg, dummyPHC, "pw"); !errors.Is(err, ErrBusy) {
		t.Errorf("Verify over the limit: %v", err)
	}
	if err := h.VerifyDummy(bg, "pw"); !errors.Is(err, ErrBusy) {
		t.Errorf("VerifyDummy over the limit: %v", err)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("an admitted caller failed: %v", err)
		}
	}
}

// A waiter that does not get the slot within 2 s is refused like an over-limit caller.
func TestHasherWait503(t *testing.T) {
	release := make(chan struct{})
	h, running := slowHasher(release)
	defer close(release)
	go h.Hash(bg, "pw")
	waitFor(t, "the first hash to start", func() bool { return running.Load() == 1 })
	t0 := time.Now()
	_, err := h.Hash(bg, "pw")
	d := time.Since(t0)
	if !errors.Is(err, ErrBusy) || d < 1900*time.Millisecond || d > 2600*time.Millisecond {
		t.Fatalf("%v after %v, want ErrBusy after about 2 s", err, d)
	}
	h.mu.Lock()
	w := h.waiting
	h.mu.Unlock()
	if w != 0 {
		t.Errorf("waiting = %d after the timeout", w)
	}
}

func TestHasherWaiterCanCancel(t *testing.T) {
	release := make(chan struct{})
	h, running := slowHasher(release)
	defer close(release)
	go h.Hash(bg, "pw")
	waitFor(t, "the first hash to start", func() bool { return running.Load() == 1 })
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "pw"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

// FreeOSMemory runs after the slot is released, from a new goroutine: Hash
// returns while it is still blocked.
func TestFreeOSMemoryNotOnTheResponsePath(t *testing.T) {
	h := NewHasher()
	h.derive = func(pw, salt []byte, m, tt uint32, th uint8, kl uint32) []byte { return make([]byte, kl) }
	started, unblock := make(chan struct{}), make(chan struct{})
	h.freeMem = func() { close(started); <-unblock }
	done := make(chan struct{})
	go func() { h.Hash(bg, "pw"); close(done) }()
	select {
	case <-done: // returned although freeMem is blocked
	case <-time.After(2 * time.Second):
		t.Fatal("Hash waited for FreeOSMemory")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("FreeOSMemory was never started")
	}
	close(unblock)

	// Not started while another caller is queued (the next hash reuses the heap).
	release := make(chan struct{})
	h2, running := slowHasher(release)
	var freed atomic.Int32
	h2.freeMem = func() { freed.Add(1) }
	go h2.Hash(bg, "a")
	waitFor(t, "first hash", func() bool { return running.Load() == 1 })
	go h2.Hash(bg, "b")
	waitFor(t, "a waiter", func() bool { h2.mu.Lock(); defer h2.mu.Unlock(); return h2.waiting == 1 })
	release <- struct{}{} // finish the first; the waiter takes the slot
	time.Sleep(50 * time.Millisecond)
	if freed.Load() != 0 {
		t.Error("FreeOSMemory ran with a waiter queued")
	}
	close(release)
	waitFor(t, "the last release to free memory", func() bool { return freed.Load() == 1 })
}

// The dummy verification costs the same as a real one (within 20%), so a missing
// user cannot be told from a wrong password by timing. Best of several runs.
func TestDummyHashWithin20PercentOfARealOne(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	h := NewHasher()
	phc, _ := h.Hash(bg, "some real password")
	best := func(f func()) time.Duration {
		m := time.Hour
		for i := 0; i < 7; i++ {
			t0 := time.Now()
			f()
			m = min(m, time.Since(t0))
		}
		return m
	}
	real := best(func() { h.Verify(bg, phc, "wrong password") })
	dummy := best(func() { h.VerifyDummy(bg, "wrong password") })
	ratio := float64(dummy) / float64(real)
	t.Logf("real %v dummy %v ratio %.2f", real, dummy, ratio)
	if ratio < 0.8 || ratio > 1.2 {
		t.Errorf("dummy/real = %.2f, want within 20%%", ratio)
	}
	if err := h.VerifyDummy(bg, ""); err != nil {
		t.Fatal(err)
	}
}
