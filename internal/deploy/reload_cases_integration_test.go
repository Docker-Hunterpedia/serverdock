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
//	  go test -tags integration -run TestIntegrationReloadCasesWith100Routes -v -count=1 ./internal/deploy
//
// (SD_TEST_RELOAD_N sets the repeats per case, default 3, 1 to 5.) Each case
// prints one parseable line on stdout, "RESULT case=... key=value ...". What it
// asserts is correctness only: no non-2xx response and no request that fails
// even after one client retry. The numbers are reported, never gated.
//
// Honest equivalent of 100 upstream routes: ONE real app container (the test
// image, started through Docker) is the upstream of all 100 routes. The 100
// services, their deploys and their state-map entries are rows and fixtures
// that all point at that container's address; the Caddy side (100 listeners, a
// 100-entry table, a reverse_proxy per request) and the Routes side (build,
// hash, lock, apply with bind isolation) are the real ones, over the real agent
// ops and the real embedded Caddy. Only the first-Sync case replaces Docker's
// container list with a fixture of the same 100 containers (the real list would
// hold one). VmHWM is the high-water mark of the TEST PROCESS (Caddy, the
// Routes code, the store and the load generator together), reset at the start
// of each case through /proc/self/clear_refs, so it is an upper bound of the
// server's own, not its exact value.

type rcClass struct {
	name                              string
	total, drops, goRetries, failures atomic.Int64
}

type rcSnap struct{ total, drops, goRetries, failures [2]int64 }

// rcLoad is the load generator of TestIntegrationTwentyReloadsUnderLoadCauseNoNon2xx
// (keep-alive and new-connection classes, per target), with POST requests
// alternating with GET.
type rcLoad struct {
	cls       [2]*rcClass // keep-alive, new-connection
	badStatus atomic.Int64
	firstBad  atomic.Value
	stop      chan struct{}
	wg        sync.WaitGroup
}

func newRCLoad() *rcLoad {
	return &rcLoad{cls: [2]*rcClass{{name: "keep-alive"}, {name: "new-connection"}}, stop: make(chan struct{})}
}

func (l *rcLoad) hit(c *http.Client, method, url string, k *rcClass) {
	k.total.Add(1)
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var conns atomic.Int64
		var body io.Reader
		if method == "POST" {
			body = strings.NewReader("payload=1")
		}
		req, _ := http.NewRequest(method, url, body)
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
			// the proxy. The client retries on a new connection.
			c.CloseIdleConnections()
			// ... after a short pause: an immediate retry can land in the same few
			// milliseconds of the same reload (measured: 2 of 6 runs without it, always a
			// second EOF on a POST; the 68,000-request control run with no reload had none).
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err != nil {
		k.failures.Add(1)
		l.firstBad.CompareAndSwap(nil, fmt.Sprintf("%s %s (%s): %v", method, url, k.name, err))
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		l.badStatus.Add(1)
		l.firstBad.CompareAndSwap(nil, fmt.Sprintf("%s %s: %s", method, url, resp.Status))
	}
}

func (l *rcLoad) start(urls []string) {
	for _, u := range urls {
		for i, k := range l.cls {
			tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: i == 1, MaxIdleConnsPerHost: 4}
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
						l.hit(c, m, u, k)
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

func (l *rcLoad) snap() (s rcSnap) {
	for i, k := range l.cls {
		s.total[i], s.drops[i], s.goRetries[i], s.failures[i] = k.total.Load(), k.drops.Load(), k.goRetries.Load(), k.failures.Load()
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
	ms                    []float64
	total, drops, retries [2]int64 // keep-alive, new-connection
	failures              [2]int64
	routes                int
	rssStart, hwmStart    float64
	rssEnd, hwmEnd        float64
	hwmReset              bool
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
	g, ref := newSwitchRig(t)
	ctx := g.ctx
	subnet := netip.MustParsePrefix(g.subnet).Addr().As4()
	subnet[3] = 2
	ip := netip.AddrFrom4(subnet).String()
	g.startApp(t, ref, "R", ip)

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
	r := NewRoutes(st, g.cl, nil)
	if err := r.ApplyRoutes(ctx, func() error { r.state.replace(fixture()); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := len(g.applier.Applied()); got != nStable+n {
		t.Fatalf("applied %d routes, want %d", got, nStable+n)
	}

	panel := "https://127.0.0.1:" + strconv.Itoa(g.panelPort) + "/"
	appURL := func(s svc) string { return "http://127.0.0.1:" + strconv.Itoa(s.port) + "/cgi-bin/id" }
	load := newRCLoad()
	load.start([]string{panel, appURL(stable[0]), appURL(stable[1])})
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
	begin := func(name string) *rcCase {
		c := &rcCase{name: name}
		c.hwmReset = os.WriteFile("/proc/self/clear_refs", []byte("5"), 0) == nil
		c.rssStart, c.hwmStart = rcProc(t)
		results = append(results, c)
		return c
	}
	// op runs one reload, then lets trailing drops land before reading the counters.
	op := func(c *rcCase, l *rcLoad, f func() error) {
		before := l.snap()
		t0 := time.Now()
		if err := f(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		c.ms = append(c.ms, float64(time.Since(t0).Microseconds())/1000)
		time.Sleep(250 * time.Millisecond)
		after := l.snap()
		for i := 0; i < 2; i++ {
			c.total[i] += after.total[i] - before.total[i]
			c.drops[i] += after.drops[i] - before.drops[i]
			c.retries[i] += after.goRetries[i] - before.goRetries[i]
			c.failures[i] += after.failures[i] - before.failures[i]
		}
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

	{ // control: no reload at all, for the baseline of drops and failures of the load itself
		ctl := begin("control_no_reload")
		for i := 0; i < n; i++ {
			op(ctl, load, func() error { time.Sleep(time.Duration(ctlMS) * time.Millisecond); return nil })
		}
		ctl.finishHWM(t)
	}
	// 1-3: a port that another program holds (bind isolation), "Try again" while it
	// is still held, and "Try again" once it is free.
	bind, again, freed := begin("bind_isolation"), &rcCase{name: "try_again_port_still_taken"}, &rcCase{name: "try_again_port_freed"}
	results = append(results, again, freed)
	for _, c := range []*rcCase{again, freed} {
		c.hwmReset, c.rssStart, c.hwmStart = bind.hwmReset, bind.rssStart, bind.hwmStart
	}
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
	for _, c := range []*rcCase{bind, again, freed} {
		c.finishHWM(t)
	}

	// 4-6: add, change, clear a public port.
	add, chg, clr := begin("add_public_port"), (*rcCase)(nil), (*rcCase)(nil)
	p1, p2 := port(), port()
	for i := 0; i < n; i++ {
		op(add, load, setPort(tog.id, p1))
		wantApplied(100, "add")
		if i == 0 {
			chg = &rcCase{name: "change_public_port"}
			results = append(results, chg)
			clr = &rcCase{name: "clear_public_port"}
			results = append(results, clr)
			for _, c := range []*rcCase{chg, clr} {
				c.hwmReset = add.hwmReset
				c.rssStart, c.hwmStart = add.rssStart, add.hwmStart
			}
		}
		op(chg, load, setPort(tog.id, p2))
		wantApplied(100, "change")
		op(clr, load, setPort(tog.id, nil))
		wantApplied(99, "clear")
		p1, p2 = p2, p1
	}
	for _, c := range []*rcCase{add, chg, clr} {
		c.finishHWM(t)
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
	del.finishHWM(t)

	// 8: the first Sync at boot: a fresh Caddy with the panel alone, a fresh
	// Routes (state map not filled), the panel under load, then Sync of the 100
	// routes. The restart itself is an outage by design and is
	// not measured here (plan 0004 step 10 measures it).
	load.finish()
	// Back to 100 exposed services for the boot case: T and n new ones.
	mustExec(t, st, `UPDATE services SET public_port = ? WHERE id = ?`, port(), tog.id)
	for i := 0; i < n; i++ {
		mk("w"+strconv.Itoa(i), true)
	}
	boot := begin("first_sync_at_boot")
	for i := 0; i < n && i < 3; i++ {
		if err := g.applier.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := g.applier.Start(); err != nil {
			t.Fatal(err)
		}
		r2 := NewRoutes(st, g.cl, nil)
		bl := newRCLoad()
		loads = append(loads, bl)
		bl.start([]string{panel})
		time.Sleep(500 * time.Millisecond)
		want := 100
		op(boot, bl, func() error { return r2.Sync(ctx, &rcBoot{st: st, ip: ip}) })
		wantApplied(want, "first sync")
		bl.finish()
		load.badStatus.Add(bl.badStatus.Load())
		if v := bl.firstBad.Load(); v != nil {
			load.firstBad.CompareAndSwap(nil, v)
		}
	}
	boot.finishHWM(t)

	// Report.
	var bad int64 = load.badStatus.Load()
	for _, c := range results {
		var sum, mx float64
		for _, v := range c.ms {
			sum += v
			if v > mx {
				mx = v
			}
		}
		mean := sum / float64(len(c.ms))
		per := func(v int64) float64 { return float64(v) / float64(c.ops) }
		fmt.Printf("RESULT case=%s routes=%d ops=%d reload_ms_mean=%.1f reload_ms_max=%.1f "+
			"keepalive_requests=%d keepalive_drops=%d keepalive_drops_per_reload=%.2f keepalive_go_retries=%d keepalive_failed_after_retry=%d "+
			"newconn_requests=%d newconn_drops=%d newconn_drops_per_reload=%.2f newconn_go_retries=%d newconn_failed_after_retry=%d "+
			"vmrss_start_mb=%.1f vmrss_end_mb=%.1f vmhwm_start_mb=%.1f vmhwm_end_mb=%.1f vmhwm_delta_mb=%.1f hwm_reset=%v\n",
			c.name, c.routes, c.ops, mean, mx,
			c.total[0], c.drops[0], per(c.drops[0]), c.retries[0], c.failures[0],
			c.total[1], c.drops[1], per(c.drops[1]), c.retries[1], c.failures[1],
			c.rssStart/1e6, c.rssEnd/1e6, c.hwmStart/1e6, c.hwmEnd/1e6, (c.hwmEnd-c.hwmStart)/1e6, c.hwmReset)
		for i := 0; i < 2; i++ {
			if c.failures[i] != 0 {
				t.Errorf("%s: %d requests failed even after one client retry", c.name, c.failures[i])
			}
			if c.total[i] < 20 {
				t.Errorf("%s: only %d requests in the %s class; the load did not run", c.name, c.total[i], load.cls[i].name)
			}
		}
	}
	if bad != 0 {
		t.Errorf("%d non-2xx responses (first: %v)", bad, load.firstBad.Load())
	}
	if v := load.firstBad.Load(); v != nil && bad == 0 {
		t.Logf("first failed request: %v", v)
	}
}

// finishHWM records the end values of a case's memory reading.
func (c *rcCase) finishHWM(t *testing.T) {
	t.Helper()
	c.rssEnd, c.hwmEnd = rcProc(t)
}

// rcBoot is the BootSource of the first-Sync case: Docker's list replaced by
// the fixture of the services that have a public port and an active deploy,
// each as a running container at the one real app's address.
type rcBoot struct {
	st *store.Store
	ip string
}

func (b *rcBoot) rows() (ids, svcs, deps []string) {
	rs, err := b.st.R.Query(`SELECT id, active_deploy_id FROM services WHERE active_deploy_id IS NOT NULL AND public_port IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, nil, nil
	}
	defer rs.Close()
	for rs.Next() {
		var s, d int64
		if rs.Scan(&s, &d) == nil {
			ids = append(ids, fmt.Sprintf("c%d", s))
			svcs = append(svcs, strconv.FormatInt(s, 10))
			deps = append(deps, strconv.FormatInt(d, 10))
		}
	}
	return ids, svcs, deps
}

func (b *rcBoot) ContainerList(context.Context) (agentops.ListResult, error) {
	ids, svcs, deps := b.rows()
	var out agentops.ListResult
	for i := range ids {
		out.Containers = append(out.Containers, agentops.ListedContainer{ID: ids[i], State: "running", IP: b.ip, ConfiguredIP: b.ip,
			Labels: agentops.ListedLabels{Project: "1", Service: svcs[i], Deploy: deps[i], Attempt: "1"}})
	}
	return out, nil
}

func (b *rcBoot) ContainerInspect(_ context.Context, id string) (agentops.ContainerState, error) {
	ids, svcs, deps := b.rows()
	for i := range ids {
		if ids[i] == id {
			return agentops.ContainerState{ID: id, State: "running", Running: true, IP: b.ip, ConfiguredIP: b.ip, Owned: true,
				Labels: map[string]string{agentops.LabelService: svcs[i], agentops.LabelDeploy: deps[i]}}, nil
		}
	}
	return agentops.ContainerState{}, fmt.Errorf("no container %s", id)
}
