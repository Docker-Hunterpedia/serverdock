package limits

import (
	"fmt"
	"net"
	"testing"
	"time"
)

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

func TestBackoffDoublingAndCap(t *testing.T) {
	c := newClock()
	b := NewBackoff(5, time.Second, 15*time.Minute, c.Now)
	// Five free failures: never blocked.
	for i := 0; i < 5; i++ {
		if w := b.Check("k", false); w != 0 {
			t.Fatalf("blocked before the free failures were used (%d): %v", i, w)
		}
		if w, ok := b.Fail("k", false); w != 0 || !ok {
			t.Fatalf("failure %d: wait %v", i+1, w)
		}
	}
	// Then 1 s, doubling, capped at 15 minutes.
	want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 900, 900, 900}
	for i, w := range want {
		w *= time.Second
		if got := b.Check("k", false); got != 0 {
			t.Fatalf("step %d: still blocked: %v", i, got)
		}
		got, _ := b.Fail("k", false)
		if got != w {
			t.Fatalf("failure %d after the free ones: wait %v, want %v", i+1, got, w)
		}
		if left := b.Check("k", false); left != w {
			t.Fatalf("Check right after = %v, want %v", left, w)
		}
		c.Add(w) // the caller waits it out
	}
	// A success forgets the key.
	b.Reset("k")
	if b.Len() != 0 || b.Check("k", false) != 0 {
		t.Fatal("Reset did not forget the key")
	}
}

func TestBackoffCountsDownWhileWaiting(t *testing.T) {
	c := newClock()
	b := NewBackoff(0, 10*time.Second, time.Hour, c.Now)
	b.Fail("k", false)
	c.Add(4 * time.Second)
	if w := b.Check("k", false); w != 6*time.Second {
		t.Fatalf("%v", w)
	}
}

func TestBackoffNeverEvictsBlocked(t *testing.T) {
	c := newClock()
	b := NewBackoff(0, time.Hour, time.Hour, c.Now)
	b.maxKey = 10
	for i := 0; i < 10; i++ {
		b.Fail(fmt.Sprint("blocked", i), false)
		c.Add(time.Millisecond)
	}
	// A flood of new keys cannot push any blocked key out.
	for i := 0; i < 100; i++ {
		b.Fail(fmt.Sprint("flood", i), false)
	}
	if b.Len() != 10 {
		t.Fatalf("len %d", b.Len())
	}
	for i := 0; i < 10; i++ {
		if w := b.Check(fmt.Sprint("blocked", i), false); w <= 0 {
			t.Fatalf("blocked%d lost its block", i)
		}
	}
}

func TestBackoffEvictsUnblockedOldestFirst(t *testing.T) {
	c := newClock()
	b := NewBackoff(5, time.Second, time.Minute, c.Now)
	b.maxKey = 4
	for _, k := range []string{"a", "b", "c", "d"} { // one free failure each: tracked, not blocked
		b.Fail(k, false)
		c.Add(time.Second)
	}
	b.Fail("a", false) // a is now the most recently failed
	c.Add(time.Second)
	b.Fail("e", false) // full: evicts the unblocked key with the oldest last failure: b
	if b.Len() != 4 {
		t.Fatalf("len %d", b.Len())
	}
	for _, k := range []string{"a", "c", "d", "e"} {
		if _, ok := b.m[k]; !ok {
			t.Errorf("%s was evicted", k)
		}
	}
	if _, ok := b.m["b"]; ok {
		t.Error("b (oldest, unblocked) was not evicted")
	}
	// A blocked key is skipped even when it is the oldest.
	b2 := NewBackoff(0, time.Hour, time.Hour, c.Now)
	b2.maxKey = 3
	b2.Fail("old-blocked", false)
	c.Add(time.Second)
	b2.free = 100 // later keys are never blocked
	b2.Fail("x", false)
	c.Add(time.Second)
	b2.Fail("y", false)
	c.Add(time.Second)
	b2.Fail("z", false)
	if _, ok := b2.m["old-blocked"]; !ok {
		t.Error("the oldest key was evicted although it is blocked")
	}
	if _, ok := b2.m["x"]; ok {
		t.Error("x (oldest unblocked) was not evicted")
	}
}

func TestBackoffForgetsAfter24h(t *testing.T) {
	c := newClock()
	b := NewBackoff(0, time.Second, time.Minute, c.Now)
	b.Fail("k", false)
	b.Fail("k", false)
	c.Add(ForgetAfter - time.Second)
	b.Fail("other", false)
	if b.m["k"] == nil || b.m["k"].fails != 2 {
		t.Fatal("forgotten too early")
	}
	c.Add(time.Second)
	if w := b.Check("k", false); w != 0 {
		t.Fatalf("still blocked after 24 h: %v", w)
	}
	if _, ok := b.Fail("k", false); !ok || b.m["k"].fails != 1 {
		t.Fatalf("not restarted from zero: %+v", b.m["k"])
	}
	// Entries past 24 h go first when room is needed, even blocked-looking ones.
	b3 := NewBackoff(0, time.Hour, time.Hour*12, c.Now)
	b3.maxKey = 2
	b3.Fail("stale", false)
	b3.Fail("fresh", false)
	c.Add(ForgetAfter)
	b3.Fail("fresh", false)
	if _, ok := b3.Fail("new", false); !ok {
		t.Fatal("no room made from the stale entry")
	}
	if _, ok := b3.m["stale"]; ok {
		t.Error("stale entry kept")
	}
}

func TestBackoffFullOfBlockedRefusesNewKey(t *testing.T) {
	c := newClock()
	b := NewBackoff(0, time.Hour, time.Hour, c.Now)
	b.maxKey = 5
	for i := 0; i < 5; i++ {
		b.Fail(fmt.Sprint("k", i), false)
	}
	// A new key is treated as blocked (fail closed), both asked and failed.
	if w := b.Check("stranger", false); w < time.Second {
		t.Fatalf("a new key was admitted while the map is full of blocked keys: %v", w)
	}
	if w, ok := b.Fail("stranger", false); ok || w <= 0 {
		t.Fatalf("a new key was tracked: %v %v", w, ok)
	}
	if b.Len() != 5 {
		t.Fatalf("len %d", b.Len())
	}
	// When a block expires there is room again.
	c.Add(time.Hour + time.Second)
	if w := b.Check("stranger", false); w != 0 {
		t.Fatalf("still refused after the blocks expired: %v", w)
	}
}

// A known key (bypassFull) is admitted when the map is full of blocked keys,
// and its own entry still counts.
func TestBackoffBypassFullAdmitsAKnownKey(t *testing.T) {
	c := newClock()
	b := NewBackoff(1, time.Hour, time.Hour, c.Now)
	b.maxKey = 3
	for i := 0; i < 3; i++ {
		b.Fail(fmt.Sprint("k", i), false)
		b.Fail(fmt.Sprint("k", i), false) // past the one free failure: blocked
	}
	if w := b.Check("known", true); w != 0 {
		t.Fatalf("known key refused: %v", w)
	}
	if _, ok := b.Fail("known", true); !ok { // free failure
		t.Fatal("known key not tracked")
	}
	if w, _ := b.Fail("known", true); w <= 0 { // its own entry counts: now blocked
		t.Fatalf("known key never blocked: %v", w)
	}
	if w := b.Check("known", true); w <= 0 {
		t.Fatal("known key's own block ignored")
	}
	if b.Len() != 4 {
		t.Fatalf("len %d: blocked keys were evicted", b.Len())
	}
	// The overflow is bounded.
	for i := 0; i < bypassOverflow+10; i++ {
		b.Fail(fmt.Sprint("kk", i), true)
	}
	if b.Len() > b.maxKey+bypassOverflow {
		t.Fatalf("len %d exceeds the overflow bound", b.Len())
	}
}

func TestBackoffInstancesIndependent(t *testing.T) {
	c := newClock()
	ip64 := NewBackoff(5, time.Second, 15*time.Minute, c.Now)
	acct := NewBackoff(10, time.Second, 15*time.Minute, c.Now)
	twofa := NewBackoff(5, 2*time.Second, time.Hour, c.Now)
	for i := 0; i < 8; i++ {
		ip64.Fail("1.2.3.4", false)
	}
	if ip64.Check("1.2.3.4", false) == 0 {
		t.Fatal("ip64 should block")
	}
	if acct.Check("1.2.3.4", false) != 0 || twofa.Check("1.2.3.4", false) != 0 || acct.Len() != 0 {
		t.Fatal("the instances share state")
	}
	// The 2FA instance has its own base and cap: 6th failure waits 2 s; the cap is 1 h.
	for i := 0; i < 5; i++ {
		twofa.Fail("u1", false)
	}
	if w, _ := twofa.Fail("u1", false); w != 2*time.Second {
		t.Fatalf("2FA first wait %v", w)
	}
	for i := 0; i < 30; i++ {
		twofa.Fail("u1", false)
	}
	if w := twofa.Check("u1", false); w != time.Hour {
		t.Fatalf("2FA cap %v, want 1h", w)
	}
}

func TestUnknownUsernamesShareOneBucket(t *testing.T) {
	// The caller maps every non-existent name to one key; the limiter then treats
	// a thousand different typed names as a single account.
	c := newClock()
	b := NewBackoff(10, time.Second, 15*time.Minute, c.Now)
	key := func(name string, exists bool) string {
		if exists {
			return "user:" + name
		}
		return "<unknown>"
	}
	for i := 0; i < 1000; i++ {
		b.Fail(key(fmt.Sprint("nobody", i), false), false)
	}
	if b.Len() != 1 {
		t.Fatalf("%d buckets for non-existent names, want 1", b.Len())
	}
	if b.Check(key("alice", true), false) != 0 {
		t.Fatal("a real user is blocked by the shared bucket")
	}
	if b.Check(key("anyone-else", false), false) == 0 {
		t.Fatal("the shared bucket does not block")
	}
}

func TestIPv6Slash48Bucket(t *testing.T) {
	a := net.ParseIP("2001:db8:1:1::1")
	b := net.ParseIP("2001:db8:1:ffff::9") // another /64 of the same /48
	other := net.ParseIP("2001:db8:2:1::1")
	if Key(a) == Key(b) {
		t.Fatal("different /64s share a /64 key")
	}
	if Key48(a) != Key48(b) || Key48(a) == Key48(other) {
		t.Fatalf("/48 keys: %s %s %s", Key48(a), Key48(b), Key48(other))
	}
	if Key48(net.ParseIP("203.0.113.5")) != "203.0.113.5" || Key48(net.ParseIP("::ffff:203.0.113.5")) != "203.0.113.5" {
		t.Error("IPv4 keys")
	}
	// An attacker rotating /64s is stopped by the /48 instance.
	c := newClock()
	ip48 := NewBackoff(20, time.Second, 15*time.Minute, c.Now)
	for i := 0; i < 25; i++ {
		ip := net.ParseIP(fmt.Sprintf("2001:db8:1:%x::1", i))
		ip48.Fail(Key48(ip), false)
	}
	if ip48.Check(Key48(net.ParseIP("2001:db8:1:ffff::1")), false) == 0 {
		t.Error("rotating /64s inside one /48 escaped the /48 limiter")
	}
}

func TestBackoffCapMustNotExceedForgetAfter(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a cap over ForgetAfter was accepted")
		}
	}()
	NewBackoff(1, time.Second, ForgetAfter+time.Second, nil)
}
