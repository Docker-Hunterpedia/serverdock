package deploy

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
)

func TestLoadCounterCountsAdminLinesAndForwardsTheRest(t *testing.T) {
	var c loadCounter
	var out bytes.Buffer
	in := strings.Join([]string{
		`{"level":"warn","logger":"admin","msg":"admin endpoint disabled"}`,
		`{"level":"warn","logger":"http","msg":"HTTP/2 skipped because it requires TLS"}`,
		`{"level":"error","msg":"something real"}`,
		`{"level":"warn","logger":"admin","msg":"admin endpoint disabled"}`,
	}, "\n")
	c.countLines(strings.NewReader(in), &out)
	if c.attempts.Load() != 2 {
		t.Errorf("attempts %d", c.attempts.Load())
	}
	if got := out.String(); !strings.Contains(got, "something real") || strings.Contains(got, "skipped") || strings.Contains(got, "admin endpoint") {
		t.Errorf("forwarded %q", got)
	}
	c.ok.Store(1)
	if ok, failed := c.snap(); ok != 1 || failed != 1 {
		t.Errorf("snap %d %d", ok, failed)
	}
	c.ok.Store(5) // more successes than attempts (a late log line): never negative
	if _, failed := c.snap(); failed != 0 {
		t.Errorf("failed %d", failed)
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
	p1, p2 := free(), free()
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
	failedPorts, err := a.Apply(context.Background(), []proxy.AppRoute{maint, page(p2)}, false)
	if err != nil || len(failedPorts) != 1 || failedPorts[0] != p2 {
		t.Fatalf("%v %v", failedPorts, err)
	}
	settle()
	if ok, failed := c.snap(); ok != 2 || failed != 1 {
		t.Fatalf("a taken port: %d ok, %d failed (want 2 and 1: the retry without the port is the served config, so it is skipped)", ok, failed)
	}
}
