//go:build testhooks

package deploy

import (
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

var inst = new(int) // any pointer: the object the fake locks belong to

func newChecker() *lockChecker {
	return &lockChecker{byG: map[uint64][]held{}, owner: map[held]uint64{}}
}

// The checker itself: the right order is silent, every reversal is recorded.
func TestLockOrderCheckerCatchesReversals(t *testing.T) {
	ok := newChecker()
	ok.taken(lockService, inst, 1)
	ok.taken(lockAlloc, inst, 0)
	ok.taken(lockRoute, inst, 0)
	ok.released(lockRoute, inst, 0)
	ok.released(lockAlloc, inst, 0)
	ok.taken(lockRoute, inst, 0) // after releasing the allocation lock, the route lock alone is fine
	ok.released(lockRoute, inst, 0)
	ok.released(lockService, inst, 1)
	if len(ok.violations) != 0 {
		t.Fatalf("the right order was reported: %v", ok.violations)
	}
	for name, steps := range map[string]func(c *lockChecker){
		"service after route":      func(c *lockChecker) { c.taken(lockRoute, inst, 0); c.taken(lockService, inst, 1) },
		"service after allocation": func(c *lockChecker) { c.taken(lockAlloc, inst, 0); c.taken(lockService, inst, 1) },
		"allocation after route":   func(c *lockChecker) { c.taken(lockRoute, inst, 0); c.taken(lockAlloc, inst, 0) },
		"two services":             func(c *lockChecker) { c.taken(lockService, inst, 1); c.taken(lockService, inst, 2) },
		"route twice":              func(c *lockChecker) { c.taken(lockRoute, inst, 0); c.taken(lockRoute, inst, 0) },
	} {
		c := newChecker()
		steps(c)
		if len(c.violations) != 1 {
			t.Errorf("%s: %v", name, c.violations)
		}
	}
	// Another goroutine's locks do not count against this one.
	c := newChecker()
	c.taken(lockRoute, inst, 0)
	done := make(chan struct{})
	go func() { c.taken(lockService, inst, 1); close(done) }()
	<-done
	if len(c.violations) != 0 {
		t.Errorf("a lock of another goroutine was counted: %v", c.violations)
	}
}

// The real code, under load from every side at once (a deploy, events, a reconnect
// and per-service resyncs): nothing is taken out of order. The package's TestMain
// fails the run for any violation in any test; this one asserts it for its own work.
func TestLockOrderIsNeverReversed(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	g.live(g.svc, 1, "10.250.0.2")
	g.start(false)
	g.run()
	g.waitUp()
	_, job := g.deploy(g.svc)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			g.rec.ResyncService(g.svc)
			g.routes.State().snapshot()
			time.Sleep(time.Millisecond)
		}
	}()
	for i := 0; i < 3; i++ {
		g.rops.drop(nil)
		time.Sleep(20 * time.Millisecond)
	}
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("job %s", s)
	}
	close(stop)
	<-done
	if v := lockOrderReport(); v != "" {
		t.Fatalf("lock order violations:\n%s", v)
	}
	if !strings.Contains(strings.Join(g.ops.names(), ","), "create") {
		t.Error("the deploy did not run")
	}
}
