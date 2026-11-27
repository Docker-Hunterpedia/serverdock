//go:build integration

package deploy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// The remaining reload cases of plan 0004 Revision 11, measured with 100 routes
// (step 15). MEASUREMENT, not a CI test: it is slow, so it runs only with
//
//	SD_TEST_RELOAD_CASES=1 SD_TEST_APP_IMAGE=127.0.0.1:5000/sd-testapp:1 \
//	  go test -tags integration -run TestIntegrationReloadCasesWith100Routes -v -count=1 ./internal/deploy | grep -E '^(RESULT|NOTE)'
//
// SD_TEST_RELOAD_N sets the repeats per case (1 to 5, default 3);
// SD_TEST_RELOAD_CONTROL_MS the length of one "no operation" window (default 250).
// Each case prints one parseable line on stdout, "RESULT case=... key=value ...". The test
// asserts correctness only: no non-2xx response and no request that fails even after one
// retry on a new connection after 50 ms. The numbers are reported, never gated.
//
// What the numbers are:
//   - loads_ok / loads_failed: Caddy config loads during the operation, counted without any
//     product hook (see loadCounter). drops_per_op divides by operations, drops_per_load by
//     all load attempts. A case that reloads nothing (a table swap) must show 0 loads.
//   - drops: requests that got no response (never read, so never processed, safe to retry),
//     per target (panel, app) and class (keep-alive, new-connection), with Go's own
//     transparent retries of reused connections next to them.
//   - failed_after_one_retry_on_new_conn_50ms: the client retries once, on a new connection
//     (idle ones closed first) after 50 ms. An IMMEDIATE retry on the same pool failed about
//     2 runs in 6 in the first version of this test (always a second EOF on a POST, never in a
//     68,000-request run with no reload), so this column is the honest "a client that waits
//     a moment is not failed" figure, not "a reload never fails a request".
//   - reload_ms is the time of the apply (Routes call). cpu_gross is the process CPU (user plus
//     system, getrusage) during it, cpu_net subtracts what the load generator alone burns in
//     the same time (measured by the no-operation case): both are of the TEST PROCESS.
//   - rss and hwm are of the test process too (Caddy, Routes, the store and the load
//     generator together). Every operation resets the high-water mark first (clear_refs), so
//     hwm_delta = VmHWM after minus VmRSS before is the growth across that reload; the
//     absolute values only bound the server's from above.
//
// 100 upstream routes, honestly: ONE real app container (the test image, started through
// Docker) is the upstream of all 100 routes. The 100 services, deploys and state-map entries
// are rows and fixtures that all point at its address; the Caddy side (100 listeners and a
// 100-entry table) and the Routes side (build, hash, lock, bind isolation) are the real ones,
// over the real agent ops and the real embedded Caddy. The boot cases go through
// Routes.Load, the real boot path (Reconciler.fullResync calls it with first = true), after a
// real container_list and 100 real container_inspect calls on that one container with the
// labels rewritten, timed apart from the apply.

type rcCounters struct{ total, drops, goRetries, failures atomic.Int64 }

const (
	tgtPanel = 0
	tgtApp   = 1
)

// rcCounts is a snapshot: [target][class], class 0 keep-alive, 1 new-connection.
type rcCounts struct{ total, drops, goRetries, failures [2][2]int64 }

func (a rcCounts) sub(b rcCounts) (d rcCounts) {
	for t := 0; t < 2; t++ {
		for c := 0; c < 2; c++ {
			d.total[t][c], d.drops[t][c] = a.total[t][c]-b.total[t][c], a.drops[t][c]-b.drops[t][c]
			d.goRetries[t][c], d.failures[t][c] = a.goRetries[t][c]-b.goRetries[t][c], a.failures[t][c]-b.failures[t][c]
		}
	}
	return d
}

func (a *rcCounts) add(b rcCounts) {
	for t := 0; t < 2; t++ {
		for c := 0; c < 2; c++ {
			a.total[t][c] += b.total[t][c]
			a.drops[t][c] += b.drops[t][c]
			a.goRetries[t][c] += b.goRetries[t][c]
			a.failures[t][c] += b.failures[t][c]
		}
	}
}

var rcClassNames = [2]string{"keepalive", "newconn"}
var rcTargetNames = [2]string{"panel", "app"}

type rcURL struct {
	url    string
	target int
}

// rcLoad is the load generator of TestIntegrationTwentyReloadsUnderLoadCauseNoNon2xx
// (keep-alive and new-connection classes, per target), with POST requests
// alternating with GET.
type rcLoad struct {
	cnt       [2][2]rcCounters
	badStatus atomic.Int64
	firstBad  atomic.Value
	stop      chan struct{}
	wg        sync.WaitGroup
}

func newRCLoad() *rcLoad { return &rcLoad{stop: make(chan struct{})} }

func (l *rcLoad) hit(c *http.Client, method string, u rcURL, class int) {
	k := &l.cnt[u.target][class]
	k.total.Add(1)
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var conns atomic.Int64
		var body io.Reader
		if method == "POST" {
			body = strings.NewReader("payload=1")
		}
		req, _ := http.NewRequest(method, u.url, body)
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { conns.Add(1) }}))
		resp, err = c.Do(req)
		if n := conns.Load(); n > 1 {
			k.goRetries.Add(n - 1)
		}
		if err == nil {
			break
		}
		if attempt == 0 {
			k.drops.Add(1) // no response; never read by the server, so safe to retry
			// A reload closes every idle keep-alive connection at once, and Go never
			// retries a POST by itself: without this the retry could pick a second
			// pooled connection that the same reload closed, which says nothing about
			// the proxy. The client retries on a new connection ...
			c.CloseIdleConnections()
			// ... after a short pause: an immediate retry can land in the same few
			// milliseconds of the same reload (measured: 2 of 6 runs without it, always a
			// second EOF on a POST; the 68,000-request control run with no reload had none).
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err != nil {
		k.failures.Add(1)
		l.firstBad.CompareAndSwap(nil, fmt.Sprintf("%s %s (%s): %v", method, u.url, rcClassNames[class], err))
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		l.badStatus.Add(1)
		l.firstBad.CompareAndSwap(nil, fmt.Sprintf("%s %s: %s", method, u.url, resp.Status))
	}
}

func (l *rcLoad) start(urls []rcURL) {
	for _, u := range urls {
		for class := 0; class < 2; class++ {
			tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: class == 1, MaxIdleConnsPerHost: 4}
			c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
			l.wg.Go(func() {
				defer tr.CloseIdleConnections()
				for n := 0; ; n++ {
					select {
					case <-l.stop:
						return
					default:
						m := "GET"
						if n%2 == 1 {
							m = "POST"
						}
						l.hit(c, m, u, class)
						time.Sleep(2 * time.Millisecond)
					}
				}
			})
		}
	}
}

func (l *rcLoad) finish() {
	close(l.stop)
	l.wg.Wait()
}

func (l *rcLoad) snap() (s rcCounts) {
	for t := 0; t < 2; t++ {
		for c := 0; c < 2; c++ {
			k := &l.cnt[t][c]
			s.total[t][c], s.drops[t][c], s.goRetries[t][c], s.failures[t][c] = k.total.Load(), k.drops.Load(), k.goRetries.Load(), k.failures.Load()
		}
	}
	return s
}

// rcProc reads VmRSS and VmHWM (bytes) of this process.
func rcProc(t *testing.T) (rss, hwm float64) {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("no /proc/self/status: %v", err)
	}
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) >= 2 && (f[0] == "VmRSS:" || f[0] == "VmHWM:") {
			kb, _ := strconv.ParseFloat(f[1], 64)
			if f[0] == "VmRSS:" {
				rss = kb * 1024
			} else {
				hwm = kb * 1024
			}
		}
	}
	return rss, hwm
}

// rcCase accumulates the operations of one case.
type rcCase struct {
	name                  string
	ops                   int
	ms, listMS            []float64
	cpuGross, cpuNet      time.Duration
	loadsOK, loadsFailed  int64
	cnt                   rcCounts
	routes                int
	rssStart, rssEnd      float64 // sums over the operations, divided by ops when printed
	hwmDeltaSum, hwmDelta float64 // the sum and the largest growth of the high-water mark
	hwmReset              bool
	extra                 string // more key=value pairs for the RESULT line
}

func (c *rcCase) print() {
	n := float64(c.ops)
	var sum, mx float64
	for _, v := range c.ms {
		sum += v
		mx = max(mx, v)
	}
	loads := c.loadsOK + c.loadsFailed
	var b strings.Builder
	fmt.Fprintf(&b, "RESULT case=%s routes=%d ops=%d loads_ok=%d loads_failed=%d loads_ok_per_op=%.2f loads_failed_per_op=%.2f "+
		"reload_ms_mean=%.1f reload_ms_max=%.1f cpu_gross_ms_mean=%.2f cpu_net_ms_mean=%.2f "+
		"rss_start_mb=%.1f rss_end_mb=%.1f rss_delta_mb=%.1f hwm_delta_mb_mean=%.1f hwm_delta_mb_max=%.1f hwm_reset=%v",
		c.name, c.routes, c.ops, c.loadsOK, c.loadsFailed, float64(c.loadsOK)/n, float64(c.loadsFailed)/n,
		sum/n, mx, float64(c.cpuGross.Microseconds())/1000/n, float64(c.cpuNet.Microseconds())/1000/n,
		c.rssStart/n/1e6, c.rssEnd/n/1e6, (c.rssEnd-c.rssStart)/n/1e6, c.hwmDeltaSum/n/1e6, c.hwmDelta/1e6, c.hwmReset)
	for t := 0; t < 2; t++ {
		for k := 0; k < 2; k++ {
			p := rcTargetNames[t] + "_" + rcClassNames[k]
			fmt.Fprintf(&b, " %s_requests=%d %s_drops=%d %s_drops_per_op=%.2f %s_drops_per_load=%.2f %s_go_retries=%d %s_failed_after_one_retry_on_new_conn_50ms=%d",
				p, c.cnt.total[t][k], p, c.cnt.drops[t][k], p, float64(c.cnt.drops[t][k])/n, p, ratio(c.cnt.drops[t][k], loads), p, c.cnt.goRetries[t][k], p, c.cnt.failures[t][k])
		}
	}
	if len(c.listMS) > 0 {
		var s float64
		for _, v := range c.listMS {
			s += v
		}
		fmt.Fprintf(&b, " list_inspect_ms_mean=%.1f", s/float64(len(c.listMS)))
	}
	if c.extra != "" {
		b.WriteString(" " + c.extra)
	}
	fmt.Println(b.String())
}

func TestIntegrationReloadCasesWith100Routes(t *testing.T) {
	if os.Getenv("SD_TEST_RELOAD_CASES") != "1" {
		t.Skip("a measurement, not a CI test: set SD_TEST_RELOAD_CASES=1 (and SD_TEST_APP_IMAGE)")
	}
	n := 3
	if v := os.Getenv("SD_TEST_RELOAD_N"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 || n > 5 {
			t.Fatalf("SD_TEST_RELOAD_N=%q: want 1 to 5", v)
		}
	}
	ctlMS := 250
	if v, err := strconv.Atoi(os.Getenv("SD_TEST_RELOAD_CONTROL_MS")); err == nil && v > 0 {
		ctlMS = v
	}
	// The counter counts proxy.Load calls (not Caddy log lines), so it can start at any time.
	counter, err := startLoadCounter()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(counter.Close)
	g, ref := newSwitchRig(t)
	ctx := g.ctx
	subnet := netip.MustParsePrefix(g.subnet).Addr().As4()
	subnet[3] = 2
	ip := netip.AddrFrom4(subnet).String()
	g.startApp(t, ref, "R", ip)
	realID := itDocker(t, "inspect", "-f", "{{.Id}}", "sd-it-"+g.inst[:8]+"-r")

	// 100 exposed services in the end: stable + n victims (deleted in the
	// delete case) + T (the toggled one, unexposed to begin with). B is a spare
	// without a port for the bind cases.
	st := openStore(t)
	used := map[int]bool{}
	port := func() int {
		for {
			p := itFreePort(t)
			if !used[p] {
				used[p] = true
				return p
			}
		}
	}
	nStable := 99 - n
	type svc struct {
		id, deploy int64
		port       int
	}
	mk := func(name string, exposed bool) svc {
		s := svc{id: addService(t, st, name)}
		s.deploy = addDeploy(t, st, s.id, 1, "succeeded", ip)
		if exposed {
			s.port = port()
			mustExec(t, st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, s.port, s.deploy, s.id)
		} else {
			mustExec(t, st, `UPDATE services SET container_port = 8080, active_deploy_id = ? WHERE id = ?`, s.deploy, s.id)
		}
		return s
	}
	var stable, victims []svc
	for i := 0; i < nStable; i++ {
		stable = append(stable, mk("s"+strconv.Itoa(i), true))
	}
	for i := 0; i < n; i++ {
		victims = append(victims, mk("v"+strconv.Itoa(i), true))
	}
	tog := mk("toggle", false)
	spare := addService(t, st, "spare")
	all := append(append([]svc{}, stable...), victims...)
	all = append(all, tog)
	fixture := func() []Container {
		var cs []Container
		for _, s := range all {
			cs = append(cs, Container{ID: fmt.Sprintf("c%d", s.id), Service: s.id, Deploy: s.deploy, State: StateRunning, IP: ip})
		}
		return cs
	}

	// Idle RSS with the routes (plan 0004 Revision 9): the process before and after the
	// first apply of ~100 routes, settled, with no load running.
	settle := func(t *testing.T) float64 {
		t.Helper()
		time.Sleep(500 * time.Millisecond)
		runtime.GC()
		debug.FreeOSMemory()
		time.Sleep(300 * time.Millisecond)
		rss, _ := rcProc(t)
		return rss
	}
	rssNoRoutes := settle(t)
	r := NewRoutes(st, g.cl, nil)
	if err := r.ApplyRoutes(ctx, func() error { r.state.replace(fixture()); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := len(g.applier.Applied()); got != nStable+n {
		t.Fatalf("applied %d routes, want %d", got, nStable+n)
	}
	rssRoutes := settle(t)
	fmt.Printf("RESULT case=idle_rss_with_routes routes=%d rss_no_routes_mb=%.1f rss_with_routes_mb=%.1f delta_mb=%.1f note=test_process_idle_after_gc\n",
		len(g.applier.Applied()), rssNoRoutes/1e6, rssRoutes/1e6, (rssRoutes-rssNoRoutes)/1e6)
	fmt.Println("NOTE load attempts are counted by proxy.LoadAttempts (one atomic add per load, test use only), successful loads by watching the active Caddy context; drops are requests with no response, retried once on a new connection after 50 ms; rss/hwm/cpu are of the whole test process")

	panel := "https://127.0.0.1:" + strconv.Itoa(g.panelPort) + "/"
	appURL := func(s svc) rcURL { return rcURL{"http://127.0.0.1:" + strconv.Itoa(s.port) + "/cgi-bin/id", tgtApp} }
	load := newRCLoad()
	load.start([]rcURL{{panel, tgtPanel}, appURL(stable[0]), appURL(stable[1])})
	loads := []*rcLoad{load}
	t.Cleanup(func() {
		for _, l := range loads {
			select {
			case <-l.stop:
			default:
				l.finish()
			}
		}
	})
	time.Sleep(500 * time.Millisecond)

	var results []*rcCase
	hwmReset := os.WriteFile("/proc/self/clear_refs", []byte("5"), 0) == nil
	begin := func(name string) *rcCase {
		c := &rcCase{name: name, hwmReset: hwmReset}
		results = append(results, c)
		return c
	}
	var cpuRate float64 // CPU seconds the load generator burns per wall second, from the no-operation case
	// op runs one operation, then lets trailing drops land before reading the counters.
	op := func(c *rcCase, l *rcLoad, f func() error) {
		_ = os.WriteFile("/proc/self/clear_refs", []byte("5"), 0)
		rss0, _ := rcProc(t)
		before := l.snap()
		ok0, fail0 := counter.snap()
		cpu0, _ := cpuTime()
		t0 := time.Now()
		if err := f(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		dur := time.Since(t0)
		cpu1, _ := cpuTime()
		time.Sleep(250 * time.Millisecond)
		ok1, fail1 := counter.snap()
		rss1, hwm1 := rcProc(t)
		c.cnt.add(l.snap().sub(before))
		c.ms = append(c.ms, float64(dur.Microseconds())/1000)
		c.cpuGross += cpu1 - cpu0
		c.cpuNet += netCPU(cpu1-cpu0, dur, cpuRate)
		c.loadsOK += ok1 - ok0
		c.loadsFailed += fail1 - fail0
		c.rssStart += rss0
		c.rssEnd += rss1
		d := max(hwm1-rss0, 0)
		c.hwmDeltaSum += d
		c.hwmDelta = max(c.hwmDelta, d)
		c.ops++
		c.routes = len(g.applier.Applied())
	}
	setPort := func(id int64, p any) func() error {
		return func() error {
			return r.ApplyRoutes(ctx, func() error {
				mustExec(t, st, `UPDATE services SET public_port = ? WHERE id = ?`, p, id)
				return nil
			})
		}
	}
	wantApplied := func(want int, what string) {
		t.Helper()
		if got := len(g.applier.Applied()); got != want {
			t.Fatalf("%s: %d routes applied, want %d", what, got, want)
		}
	}

	// 0: no operation: the load generator alone (its drops, failures and CPU rate).
	{
		ctl := begin("no_operation_load_generator_baseline")
		for i := 0; i < n; i++ {
			op(ctl, load, func() error { time.Sleep(time.Duration(ctlMS) * time.Millisecond); return nil })
		}
		cpuRate = ctl.cpuGross.Seconds() / (sumF(ctl.ms) / 1000)
		ctl.cpuNet = 0 // not defined for the baseline itself
	}

	// 0b: a real table swap (a switch that keeps the port set): no load at all.
	{
		sw := begin("table_swap_switch_no_reload")
		for i := 0; i < n; i++ {
			op(sw, load, func() error { return r.SetOverride(ctx, stable[5].id, Override{Kind: OverrideMaintenance}) })
			op(sw, load, func() error { return r.ClearOverride(ctx, stable[5].id) })
		}
	}

	// 1-3: a port that another program holds (bind isolation), "Try again" while it
	// is still held, and "Try again" once it is free. ONE new port, taken: the fast path
	// (the agent names the port in the bind error and retries without it).
	bind, again, freed := begin("one_new_port_taken_fast_path"), begin("try_again_port_still_taken"), begin("try_again_port_freed")
	base := len(g.applier.Applied())
	for i := 0; i < n; i++ {
		x := port()
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(x))
		if err != nil {
			t.Fatal(err)
		}
		op(bind, load, setPort(spare, x))
		if r.BindStatus(x) == "" {
			t.Fatalf("bind isolation: port %d is not reported as taken (applied %v)", x, g.applier.Applied())
		}
		wantApplied(base, "bind isolation: the other routes stay")
		op(again, load, func() error { return r.Refresh(ctx) })
		if r.BindStatus(x) == "" {
			t.Fatalf("try again with the port still held: no longer reported as taken")
		}
		ln.Close()
		op(freed, load, func() error { return r.Refresh(ctx) })
		if r.BindStatus(x) != "" {
			t.Fatalf("try again with the port free: still reported as taken")
		}
		wantApplied(base+1, "try again, port freed")
		if err := setPort(spare, nil)(); err != nil { // not measured: back to the base set
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond)
		wantApplied(base, "back to base")
	}

	// 4-6: add, change, clear a public port.
	add, chg, clr := begin("add_public_port"), begin("change_public_port"), begin("clear_public_port")
	p1, p2 := port(), port()
	for i := 0; i < n; i++ {
		op(add, load, setPort(tog.id, p1))
		wantApplied(100, "add")
		op(chg, load, setPort(tog.id, p2))
		wantApplied(100, "change")
		op(clr, load, setPort(tog.id, nil))
		wantApplied(99, "clear")
		p1, p2 = p2, p1
	}

	// 7: delete an exposed service.
	del := begin("delete_exposed_service")
	for i, v := range victims {
		op(del, load, func() error {
			return r.DeleteRoutes(ctx, v.id, func() error {
				mustExec(t, st, `DELETE FROM services WHERE id = ?`, v.id)
				return nil
			})
		})
		wantApplied(nStable+n-1-i, "delete")
	}

	// 8-10: the first load at boot: a fresh Caddy with the panel alone, a fresh Routes (state
	// map not filled), the panel under load, then what the reconciler's first resync does:
	// container_list, a container_inspect per container, ContainerFromInspect, and
	// Routes.Load(first = true). The restart itself is an outage by design and is not
	// measured here (plan 0004 step 10 measures it). The list and the inspects are timed
	// apart from the apply. 0, 1 and 2 of the 100 new ports are held by another listener.
	load.finish()
	// Back to 100 exposed services: T and n new ones.
	mustExec(t, st, `UPDATE services SET public_port = ? WHERE id = ?`, port(), tog.id)
	for i := 0; i < n; i++ {
		mk("w"+strconv.Itoa(i), true)
	}
	// The routes' ports, in the order of the rows: the ones held by another program.
	rs, err := st.R.Query(`SELECT public_port FROM services WHERE public_port IS NOT NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var ports []int
	for rs.Next() {
		var p int
		if err := rs.Scan(&p); err != nil {
			t.Fatal(err)
		}
		ports = append(ports, p)
	}
	rs.Close()
	if len(ports) != 100 {
		t.Fatalf("%d exposed services, want 100", len(ports))
	}
	bootSrc := &rcBoot{st: st, cl: g.cl, realID: realID, ip: ip}
	bootTaken := map[string]int{}
	boot := func(name string, taken, reps int) {
		c := begin(name)
		for i := 0; i < reps; i++ {
			if err := g.applier.Stop(); err != nil {
				t.Fatal(err)
			}
			if err := g.applier.Start(); err != nil {
				t.Fatal(err)
			}
			var held []net.Listener
			for k := 0; k < taken; k++ {
				ln, err := net.Listen("tcp", ":"+strconv.Itoa(ports[10+k*7]))
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, ln)
			}
			r2 := NewRoutes(st, g.cl, nil)
			bl := newRCLoad()
			loads = append(loads, bl)
			bl.start([]rcURL{{panel, tgtPanel}})
			time.Sleep(500 * time.Millisecond)
			t0 := time.Now()
			all, err := bootSrc.listInspect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c.listMS = append(c.listMS, float64(time.Since(t0).Microseconds())/1000)
			if len(all) != 100 {
				t.Fatalf("the boot list holds %d containers, want 100", len(all))
			}
			// As the reconciler runs it: a context with a cancel and no deadline.
			// The boot Load runs without a deadline: Reconciler.session (reconciler.go) derives its
			// context from Run's server context with WithCancel only, and neither Routes.apply
			// (detach false) nor Applier.Apply adds one. The rig's own context has a 4-minute
			// deadline, so this one does not derive from it.
			bctx, cancel := context.WithCancel(context.Background())
			op(c, bl, func() error { return r2.Load(bctx, all, true, nil) })
			cancel()
			for k := 0; k < taken; k++ {
				if r2.BindStatus(ports[10+k*7]) == "" {
					t.Fatalf("%s: port %d is not reported as taken", name, ports[10+k*7])
				}
			}
			wantApplied(100-taken, name)
			for _, ln := range held {
				ln.Close()
			}
			bl.finish()
			load.badStatus.Add(bl.badStatus.Load())
			if v := bl.firstBad.Load(); v != nil {
				load.firstBad.CompareAndSwap(nil, v)
			}
		}
		// k taken ports among the new ones cost k failed loads (each drops the port it names)
		// and one that works: k+1 (0 taken: 1).
		want := taken + 1
		bootTaken[name] = taken
		c.extra = fmt.Sprintf("containers=100 new_ports=%d taken_ports=%d expected_loads_about=%d", len(ports), taken, want)
	}
	boot("first_sync_at_boot", 0, min(n, 3))
	boot("first_sync_at_boot_with_taken_ports_1", 1, min(n, 2))
	boot("first_sync_at_boot_with_taken_ports_2", 2, min(n, 2))

	// Report.
	bad := load.badStatus.Load()
	for _, c := range results {
		c.print()
		for k := 0; k < 2; k++ {
			var tot, fail int64
			for tg := 0; tg < 2; tg++ {
				tot += c.cnt.total[tg][k]
				fail += c.cnt.failures[tg][k]
			}
			if fail != 0 {
				t.Errorf("%s: %d %s requests failed even after one retry on a new connection (first: %v)", c.name, fail, rcClassNames[k], load.firstBad.Load())
			}
			if tot < 20 {
				t.Errorf("%s: only %d requests in the %s class; the load did not run", c.name, tot, rcClassNames[k])
			}
		}
	}
	// In-run sanity checks of the load counter and of the cases' shape.
	if !counter.consistent() {
		t.Errorf("the load counter saw more successful loads than attempts")
	}
	for _, c := range results {
		if taken, ok := bootTaken[c.name]; ok && c.ops > 0 {
			// Per operation: loads attempted <= taken + 1 (bind isolation drops each taken port as named).
			if got, max := c.loadsOK+c.loadsFailed, int64(taken+1)*int64(c.ops); got > max {
				t.Errorf("%s: %d loads in %d operations, want at most %d (taken ports + 1 each)", c.name, got, c.ops, max)
			}
		}
		switch c.name {
		case "table_swap_switch_no_reload":
			if c.loadsOK != 0 || c.loadsFailed != 0 {
				t.Errorf("%s: %d ok and %d failed loads, want none", c.name, c.loadsOK, c.loadsFailed)
			}
		case "try_again_port_freed":
			if c.loadsOK < 1 {
				t.Errorf("%s: no successful load", c.name)
			}
		case "one_new_port_taken_fast_path":
			if c.loadsFailed < 1 || c.loadsOK != 0 {
				t.Errorf("%s: %d ok and %d failed loads, want at least one failed and no ok", c.name, c.loadsOK, c.loadsFailed)
			}
		}
	}
	fmt.Printf("NOTE load_generator_cpu_seconds_per_wall_second=%.3f\n", cpuRate)
	if bad != 0 {
		t.Errorf("%d non-2xx responses (first: %v)", bad, load.firstBad.Load())
	}
}

func sumF(v []float64) (s float64) {
	for _, x := range v {
		s += x
	}
	return s
}

// rcBoot is the list and inspect of the first resync: container_list through the real
// agent op, and a real container_inspect on the one real container for each of the 100
// fixture containers, its labels and address rewritten to the service's (one real
// container cannot carry 100 identities).
type rcBoot struct {
	st     *store.Store
	cl     *agentops.Client
	realID string
	ip     string
}

func (b *rcBoot) listInspect(ctx context.Context) ([]Container, error) {
	if _, err := b.cl.ContainerList(ctx); err != nil { // the real list (one container of ours)
		return nil, err
	}
	rs, err := b.st.R.QueryContext(ctx, `SELECT id, active_deploy_id FROM services WHERE active_deploy_id IS NOT NULL AND public_port IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var all []Container
	for rs.Next() {
		var svc, dep int64
		if err := rs.Scan(&svc, &dep); err != nil {
			return nil, err
		}
		st, err := b.cl.ContainerInspect(ctx, b.realID)
		if err != nil {
			return nil, err
		}
		st.ID = fmt.Sprintf("c%d", svc)
		st.Owned, st.State, st.Running, st.Restarting = true, "running", true, false
		st.IP, st.ConfiguredIP = b.ip, b.ip
		st.Labels = map[string]string{agentops.LabelService: strconv.FormatInt(svc, 10), agentops.LabelDeploy: strconv.FormatInt(dep, 10)}
		if c, ok := ContainerFromInspect(st); ok {
			all = append(all, c)
		}
	}
	return all, rs.Err()
}
