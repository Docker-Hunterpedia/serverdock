//go:build integration

package deploy

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// itCounter counts the requests of one target over one connection class.
type itCounter struct {
	target, class                string
	total, drops, retries, fails atomic.Int64
	bad                          atomic.Int64
	bodies                       sync.Map // body -> *atomic.Int64
}

func (c *itCounter) body(b string) {
	v, _ := c.bodies.LoadOrStore(b, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

func (c *itCounter) seen() map[string]int64 {
	out := map[string]int64{}
	c.bodies.Range(func(k, v any) bool { out[k.(string)] = v.(*atomic.Int64).Load(); return true })
	return out
}

// itTarget is one URL under constant POST load. check returns "" for an
// acceptable response, else what was wrong with it.
type itTarget struct {
	name  string
	url   string
	check func(status int, body string) string
	// checkResp, when set, is used instead of check and also sees the headers (the
	// maintenance page's Retry-After).
	checkResp func(status int, h http.Header, body string) string
}

// itLoad runs POST load on every target, over keep-alive and over new connections
// (one goroutine per target and class). A request that fails to get a response is
// counted as a drop and sent once more (that retry is the "failed after a client
// retry" count if it fails too); the Go transport's own transparent retries are
// counted with httptrace (a second GotConn for one request).
type itLoad struct {
	stop     chan struct{}
	wg       sync.WaitGroup
	counters []*itCounter
	firstBad atomic.Value
	mu       sync.Mutex
	anoms    []string // every unexpected response, with its time
}

func (l *itLoad) anomaly(msg string) {
	l.mu.Lock()
	l.anoms = append(l.anoms, time.Now().Format("05.000")+" "+msg)
	l.mu.Unlock()
}

func startLoad(targets []itTarget) *itLoad {
	l := &itLoad{stop: make(chan struct{})}
	for _, tg := range targets {
		for _, class := range []string{"keep-alive", "new-connection"} {
			c := &itCounter{target: tg.name, class: class}
			l.counters = append(l.counters, c)
			tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: class == "new-connection", MaxIdleConnsPerHost: 4}
			cli := &http.Client{Transport: tr, Timeout: 15 * time.Second}
			l.wg.Go(func() {
				defer tr.CloseIdleConnections()
				for {
					select {
					case <-l.stop:
						return
					default:
						l.hit(cli, tg, c)
						time.Sleep(2 * time.Millisecond)
					}
				}
			})
		}
	}
	return l
}

func (l *itLoad) hit(cli *http.Client, tg itTarget, c *itCounter) {
	c.total.Add(1)
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var conns atomic.Int64
		req, _ := http.NewRequest("POST", tg.url, strings.NewReader("payload=1"))
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { conns.Add(1) }}))
		resp, err = cli.Do(req)
		if n := conns.Load(); n > 1 {
			c.retries.Add(n - 1)
		}
		if err == nil {
			break
		}
		if attempt == 0 {
			c.drops.Add(1)
			l.firstBad.CompareAndSwap(nil, fmt.Sprintf("%s (%s): %v", tg.name, c.class, err))
		}
	}
	if err != nil {
		c.fails.Add(1)
		return
	}
	b, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil { // a truncated response is not a good one, whatever its status
		c.bad.Add(1)
		l.anomaly(fmt.Sprintf("%s (%s): %d, reading the body: %v (got %q)", tg.name, c.class, resp.StatusCode, rerr, b))
		l.firstBad.CompareAndSwap(nil, fmt.Sprintf("%s (%s): %d, reading the body: %v (got %q)", tg.name, c.class, resp.StatusCode, rerr, b))
		return
	}
	c.body(strings.TrimSpace(string(b)))
	why := ""
	if tg.checkResp != nil {
		why = tg.checkResp(resp.StatusCode, resp.Header, string(b))
	} else {
		why = tg.check(resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if why != "" {
		c.bad.Add(1)
		l.anomaly(fmt.Sprintf("%s (%s): %s", tg.name, c.class, why))
		l.firstBad.CompareAndSwap(nil, fmt.Sprintf("%s (%s): %s", tg.name, c.class, why))
	}
}

// finish stops the load and returns the counters.
func (l *itLoad) finish() []*itCounter {
	close(l.stop)
	l.wg.Wait()
	return l.counters
}

// report logs the per-class totals and returns the sums (requests, non-2xx or
// unexpected responses, drops, Go-internal retries, failures after a client retry).
func (l *itLoad) report(t *testing.T) (total, bad, drops, retries, fails int64) {
	t.Helper()
	for _, class := range []string{"keep-alive", "new-connection"} {
		var tt, bb, dd, rr, ff int64
		for _, c := range l.counters {
			if c.class == class {
				tt, bb, dd, rr, ff = tt+c.total.Load(), bb+c.bad.Load(), dd+c.drops.Load(), rr+c.retries.Load(), ff+c.fails.Load()
			}
		}
		t.Logf("%s: %d requests; non-2xx or unexpected %d; dropped connections %d; Go-internal retries %d; failed after a client retry %d", class, tt, bb, dd, rr, ff)
		total, bad, drops, retries, fails = total+tt, bad+bb, drops+dd, retries+rr, fails+ff
	}
	return
}

// itOwnedLabels are the labels that make a fixture container a container of this
// install for a service and deploy (as the runner's create sets them).
func itOwnedLabels(project, service, deploy int64) []string {
	return []string{"--label", "serverdock.managed=1", "--label", "serverdock.project=" + strconv.FormatInt(project, 10),
		"--label", "serverdock.service=" + strconv.FormatInt(service, 10), "--label", "serverdock.deploy=" + strconv.FormatInt(deploy, 10),
		"--label", "serverdock.attempt=1"}
}

func (g *switchRig) containerID(t *testing.T, name string) string {
	t.Helper()
	return itDocker(t, "inspect", "-f", "{{.Id}}", "sd-it-"+g.inst[:8]+"-"+strings.ToLower(name))
}

// liveContainers lists the install's containers (by label) of a deploy.
func (x *itRunner) idsOfDeploy(t *testing.T, d int64) []string {
	t.Helper()
	list, err := x.cl.ContainerList(x.g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range list.Containers {
		if c.Labels.Deploy == strconv.FormatInt(d, 10) {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

func (x *itRunner) activeOf(t *testing.T, svc int64) int64 {
	t.Helper()
	var a *int64
	if err := x.st.R.QueryRow(`SELECT active_deploy_id FROM services WHERE id = ?`, svc).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if a == nil {
		return 0
	}
	return *a
}

// TestBlueGreenSwitchWithZeroNon2xx: a real blue-green deploy (the real runner,
// agent ops, Docker and embedded Caddy) of an app that serves under constant POST
// load, with a second app and the panel under load too, over keep-alive and new
// connections. STRICT (plan 0004 Revision 11): zero non-2xx, zero dropped
// connections, zero client or Go-internal retries, and zero Caddy loads during the
// deploy (the exact caddy.ActiveContext comparison); every response on the
// deploying app comes from the old container or from the new one, never from
// another service; the old container is removed after the drain, and exactly one
// proxy_apply (the switch) is sent.
func TestBlueGreenSwitchWithZeroNon2xx(t *testing.T) {
	g, ref := newSwitchRig(t)
	ctx := g.ctx
	x := newITRunner(t, g, nil)
	base := netip.MustParsePrefix(g.subnet).Addr().As4()
	ipN := func(n byte) string { return netip.AddrFrom4([4]byte{base[0], base[1], base[2], n}).String() }
	ipOld, ipB := ipN(2), ipN(4)

	svcB := addService(t, x.st, "second")
	oldDeploy := addDeploy(t, x.st, x.svc, 1, "succeeded", ipOld)
	dB := addDeploy(t, x.st, svcB, 1, "succeeded", ipB)
	g.startApp(t, ref, "OLD", ipOld, itOwnedLabels(1, x.svc, oldDeploy)...)
	g.startApp(t, ref, "B", ipB, itOwnedLabels(1, svcB, dB)...)
	portA, portB := itFreePort(t), itFreePort(t)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, portA, oldDeploy, x.svc)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, portB, dB, svcB)
	oldID, bID := g.containerID(t, "OLD"), g.containerID(t, "B")
	if err := x.rt.ApplyRoutes(ctx, func() error {
		x.rt.state.replace([]Container{
			{ID: oldID, Service: x.svc, Deploy: oldDeploy, State: StateRunning, IP: ipOld},
			{ID: bID, Service: svcB, Deploy: dB, State: StateRunning, IP: ipB},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := g.applier.Applied(); len(got) != 2 {
		t.Fatalf("applied %v", got)
	}

	only := func(want string) func(int, string) string {
		return func(s int, b string) string {
			if s/100 != 2 || b != want {
				return fmt.Sprintf("%d %q, want a 2xx %q", s, b, want)
			}
			return ""
		}
	}
	deploying := func(s int, b string) string {
		if s/100 != 2 || !strings.HasPrefix(b, "app-") || b == "app-B" {
			return fmt.Sprintf("%d %q", s, b)
		}
		return ""
	}
	load := startLoad([]itTarget{
		{"deploying app", "http://127.0.0.1:" + strconv.Itoa(portA) + "/cgi-bin/id", deploying, nil},
		{"second app", "http://127.0.0.1:" + strconv.Itoa(portB) + "/cgi-bin/id", only("app-B"), nil},
		{"panel", "https://127.0.0.1:" + strconv.Itoa(g.panelPort) + "/", only("panel"), nil},
	})
	time.Sleep(300 * time.Millisecond) // the load is flowing before the deploy starts

	ctxBefore := caddy.ActiveContext()
	applyBefore := x.cc.n("proxy_apply")
	snap := Snapshot{V: 1, ImageRef: ref, ContainerPort: 8080, Health: SnapHealth{Kind: "http", Path: "/cgi-bin/id", TimeoutS: 30, Passes: 2},
		Strategy: "blue_green", DrainS: 10, StopGraceS: 1}
	var phases []string
	var pmu sync.Mutex
	x.r.trace = func(ev string) {
		pmu.Lock()
		phases = append(phases, time.Now().Format("05.000")+" "+ev)
		pmu.Unlock()
	}
	t0 := time.Now()
	d, runErr := x.deploy(t, snap)
	took := time.Since(t0)
	time.Sleep(300 * time.Millisecond)
	loaded := caddyLoaded(ctxBefore)
	applyDuring := x.cc.n("proxy_apply") - applyBefore
	counters := load.finish()
	total, bad, drops, retries, fails := load.report(t)
	_ = counters

	pmu.Lock()
	t.Logf("phases: %v", phases)
	pmu.Unlock()
	for _, a := range load.anoms {
		t.Logf("anomaly at %s", a)
	}
	if runErr != nil {
		t.Fatalf("the deploy failed: %v", runErr)
	}
	status, code, reason, _ := x.row(t, d)
	if status != "succeeded" || code != "" {
		t.Fatalf("deploy: %s %s %q", status, code, reason)
	}
	newIDs := x.idsOfDeploy(t, d)
	if len(newIDs) != 1 {
		t.Fatalf("%d containers of the new deploy", len(newIDs))
	}
	newBody := "app-" + newIDs[0][:12]

	// Every response on the deploying app came from the old or the new container.
	seen := map[string]int64{}
	for _, c := range load.counters {
		if c.target == "deploying app" {
			for b, n := range c.seen() {
				seen[b] += n
			}
		}
	}
	var kinds []string
	for b, n := range seen {
		kinds = append(kinds, fmt.Sprintf("%s x%d", b, n))
		if b != "app-OLD" && b != newBody {
			t.Errorf("the deploying app answered %q: not the old (app-OLD) or the new (%s) container", b, newBody)
		}
	}
	sort.Strings(kinds)
	t.Logf("responses on the deploying app: %s; deploy took %v; proxy_apply calls during the deploy: %d; Caddy loaded: %v", strings.Join(kinds, ", "), took, applyDuring, loaded)
	if seen["app-OLD"] == 0 || seen[newBody] == 0 {
		t.Errorf("the load did not see both versions: %v", seen)
	}
	if total < 400 {
		t.Errorf("only %d requests in all", total)
	}
	if bad != 0 || drops != 0 || retries != 0 || fails != 0 {
		t.Errorf("non-2xx or unexpected %d, dropped %d, retries %d, failed %d (first: %v)", bad, drops, retries, fails, load.firstBad.Load())
	}
	if loaded {
		t.Error("Caddy loaded a config during the deploy")
	}
	if applyDuring != 1 {
		t.Errorf("%d proxy_apply calls during the deploy, want exactly 1 (the switch)", applyDuring)
	}
	if x.activeOf(t, x.svc) != d {
		t.Error("the new deploy is not the active one")
	}
	if out := itDocker(t, "ps", "-a", "-q", "--filter", "id="+oldID); out != "" {
		t.Errorf("the old container still exists: %s", out)
	}
	if out := itDocker(t, "ps", "-q", "--filter", "id="+newIDs[0]); out == "" {
		t.Error("the new container is not running")
	}
	if out := itDocker(t, "ps", "-q", "--filter", "id="+bID); out == "" {
		t.Error("the second app's container is not running")
	}
	for _, s := range []string{StageSwitch, StageCommit, StageDrain, StageStopOld} {
		var state string
		x.st.R.QueryRow(`SELECT state FROM deploy_stages WHERE deploy_id = ? AND stage = ?`, d, s).Scan(&state)
		if state != StageOK {
			t.Errorf("stage %s is %q", s, state)
		}
	}
}

// TestTwoServicesDeployingAtTheSameTimeBothEndLive: two services whose first
// deploys run at once (their own service locks, the one route lock and the one
// allocation lock between them) both end live, each at its own address, each port
// answering from its own container, and no Caddy load happens.
func TestTwoServicesDeployingAtTheSameTimeBothEndLive(t *testing.T) {
	g, ref := newSwitchRig(t)
	ctx := g.ctx
	x := newITRunner(t, g, nil)
	svcB := addService(t, x.st, "second")
	portA, portB := itFreePort(t), itFreePort(t)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080 WHERE id = ?`, portA, x.svc)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080 WHERE id = ?`, portB, svcB)
	if err := x.rt.ApplyRoutes(ctx, func() error { x.rt.state.replace(nil); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := g.applier.Applied(); len(got) != 2 {
		t.Fatalf("applied %v", got)
	}

	snap := Snapshot{V: 1, ImageRef: ref, ContainerPort: 8080, Health: SnapHealth{Kind: "http", Path: "/cgi-bin/id", TimeoutS: 30, Passes: 2},
		Strategy: "blue_green", DrainS: 10, StopGraceS: 1}
	raw, _ := snap.Marshal()
	type job struct{ svc, deploy, id int64 }
	var jobsQueued []job
	for _, svc := range []int64{x.svc, svcB} {
		out, err := x.js.EnqueueDeploy(ctx, jobs.NewDeploy{ServiceID: svc, Kind: "deploy", Snapshot: raw, Settings: snap.Settings(),
			Actor: jobs.ActorOf(actions.TokenActor("1", actions.ScopeWrite))})
		if err != nil {
			t.Fatal(err)
		}
		jobsQueued = append(jobsQueued, job{svc, out.DeployID, out.JobID})
	}
	for range jobsQueued { // both are picked (they have different lock keys), then run together
		if _, ok, err := x.js.Pick(ctx, "it-boot"); err != nil || !ok {
			t.Fatalf("pick: %v %v", ok, err)
		}
	}
	ctxBefore := caddy.ActiveContext()
	applyBefore := x.cc.n("proxy_apply")
	errs := make([]error, len(jobsQueued))
	var wg sync.WaitGroup
	for i, j := range jobsQueued {
		wg.Go(func() { _, errs[i] = x.r.runDeploy(ctx, actions.Call{}, j.id) })
	}
	wg.Wait()
	loaded := caddyLoaded(ctxBefore)
	t.Logf("proxy_apply calls during the two deploys: %d; Caddy loaded: %v", x.cc.n("proxy_apply")-applyBefore, loaded)

	ips := map[string]bool{}
	for i, j := range jobsQueued {
		if errs[i] != nil {
			t.Fatalf("deploy of service %d: %v", j.svc, errs[i])
		}
		status, code, reason, _ := x.row(t, j.deploy)
		if status != "succeeded" {
			t.Fatalf("deploy %d: %s %s %q", j.deploy, status, code, reason)
		}
		if x.activeOf(t, j.svc) != j.deploy {
			t.Errorf("service %d: the deploy is not active", j.svc)
		}
		var ip string
		x.st.R.QueryRow(`SELECT ip FROM deploys WHERE id = ?`, j.deploy).Scan(&ip)
		ips[ip] = true
	}
	if len(ips) != 2 {
		t.Errorf("addresses %v, want two distinct", ips)
	}
	if loaded {
		t.Error("Caddy loaded a config while two services deployed")
	}
	// Each public port answers from its own service's container.
	for i, port := range []int{portA, portB} {
		ids := x.idsOfDeploy(t, jobsQueued[i].deploy)
		if len(ids) != 1 {
			t.Fatalf("service %d: %d containers", jobsQueued[i].svc, len(ids))
		}
		want := "app-" + ids[0][:12]
		var got string
		for try := 0; try < 20; try++ {
			resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/cgi-bin/id")
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				got = strings.TrimSpace(string(b))
				if resp.StatusCode == 200 && got == want {
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if got != want {
			t.Errorf("port %d answered %q, want %q (its own service's container)", port, got, want)
		}
	}

}
