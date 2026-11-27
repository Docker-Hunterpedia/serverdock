package deploy

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
)

func TestLoadCounterSnapAndConsistency(t *testing.T) {
	var n int64
	c := &loadCounter{attemptsFn: func() int64 { return n }}
	n = 2
	c.ok.Store(1)
	if ok, failed := c.snap(); ok != 1 || failed != 1 || !c.consistent() {
		t.Errorf("snap %d %d", ok, failed)
	}
	c.ok.Store(5) // more successes than attempts: never negative, and flagged
	if _, failed := c.snap(); failed != 0 || c.consistent() {
		t.Errorf("failed %d", failed)
	}
	// Relative to the start: loads before the counter existed are not counted.
	n = 10
	c2 := &loadCounter{attemptsFn: func() int64 { return n }}
	c2.base.attempts = n
	n = 12
	if c2.attempts() != 2 {
		t.Errorf("attempts %d", c2.attempts())
	}
}

func TestNetCPUAndRatio(t *testing.T) {
	// 10 ms of CPU in a 20 ms operation, with a load generator that burns 0.25 CPU-seconds
	// per second: 5 ms of it is the generator's.
	if got := netCPU(10*time.Millisecond, 20*time.Millisecond, 0.25); got != 5*time.Millisecond {
		t.Errorf("%v", got)
	}
	if got := netCPU(time.Millisecond, 20*time.Millisecond, 0.25); got != 0 {
		t.Errorf("clamped: %v", got)
	}
	if ratio(3, 0) != 0 || ratio(3, 2) != 1.5 {
		t.Error("ratio")
	}
	if c, err := cpuTime(); err != nil || c <= 0 {
		t.Errorf("cpuTime %v %v", c, err)
	}
}

// The counter against the real embedded Caddy (no Docker): a load that works, a table swap
// (no load), and a load that fails on a taken port and is retried without it.
func TestLoadCounterWithTheRealApplier(t *testing.T) {
	dir := t.TempDir()
	leaf, err := pki.EnsurePanelLeaf(pki.PanelOptions{Dir: filepath.Join(dir, "pki")})
	if err != nil {
		t.Fatal(err)
	}
	free := func() int {
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		return ln.Addr().(*net.TCPAddr).Port
	}
	a := proxy.NewApplier(proxy.Config{DataDir: dir, Listen: "127.0.0.1:" + strconv.Itoa(free()), Upstream: "127.0.0.1:9",
		Panel: &proxy.Panel{Listen: "127.0.0.1:" + strconv.Itoa(free()), CertFile: leaf.CertFile}})
	c, err := startLoadCounter()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { // the Applier stops first (it logs), then the counter
		_ = a.Stop()
		c.Close()
	}()
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	settle := func() { time.Sleep(300 * time.Millisecond) }
	settle()
	if ok, failed := c.snap(); ok != 1 || failed != 0 {
		t.Fatalf("start: %d ok, %d failed", ok, failed)
	}
	p1, p2, p3 := free(), free(), free()
	page := func(p int) proxy.AppRoute { return proxy.AppRoute{Port: p, Static: proxy.NotRunning()} }
	if _, err := a.Apply(context.Background(), []proxy.AppRoute{page(p1)}, false); err != nil {
		t.Fatal(err)
	}
	settle()
	if ok, failed := c.snap(); ok != 2 || failed != 0 {
		t.Fatalf("one new port: %d ok, %d failed", ok, failed)
	}
	maint := page(p1)
	maint.Static = proxy.Maintenance()
	if _, err := a.Apply(context.Background(), []proxy.AppRoute{maint}, false); err != nil {
		t.Fatal(err)
	}
	settle()
	if ok, failed := c.snap(); ok != 2 || failed != 0 {
		t.Fatalf("a table swap must not load: %d ok, %d failed", ok, failed)
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(p2))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ln3, err := net.Listen("tcp", ":"+strconv.Itoa(p3))
	if err != nil {
		t.Fatal(err)
	}
	defer ln3.Close()
	// Two taken ports: each failed load drops the port it names, and what is left is
	// the served config, so it is skipped: 2 failed loads, no ok one.
	failedPorts, err := a.Apply(context.Background(), []proxy.AppRoute{maint, page(p2), page(p3)}, false)
	if err != nil || len(failedPorts) != 2 {
		t.Fatalf("%v %v", failedPorts, err)
	}
	settle()
	if ok, failed := c.snap(); ok != 2 || failed != 2 {
		t.Fatalf("two taken ports: %d ok, %d failed (want 2 and 2: k taken ports cost k failed loads, the last remainder is the served config)", ok, failed)
	}
}
