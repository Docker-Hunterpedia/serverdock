//go:build integration

package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// countingCaller counts the ops that go to the real agent, by name. The runner's
// client and the Routes client both use it, so a proxy_apply from anywhere shows.
type countingCaller struct {
	c   agentops.Caller
	ops sync.Map // name -> *atomic.Int64
}

func (c *countingCaller) Call(ctx context.Context, op string, args any, onLine func(agent.Line)) (json.RawMessage, error) {
	v, _ := c.ops.LoadOrStore(op, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
	return c.c.Call(ctx, op, args, onLine)
}

func (c *countingCaller) n(op string) int64 {
	if v, ok := c.ops.Load(op); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

// itRunner is the runner over the real chain (Docker, the agent's ops over a hub
// session, the real allocator), with the app network always OK.
type itRunner struct {
	g   *switchRig
	cc  *countingCaller
	cl  *agentops.Client
	st  *store.Store
	r   *Runner
	js  *jobs.Store
	rt  *Routes
	svc int64
	reg *actions.Registry
}

func newITRunner(t *testing.T, g *switchRig, mod func(*RunnerConfig)) *itRunner {
	t.Helper()
	cc := &countingCaller{c: g.sess}
	cl := agentops.NewClient(cc)
	st := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	al, err := audit.New(st, log)
	if err != nil {
		t.Fatal(err)
	}
	reg := actions.New(al, log)
	rt := NewRoutes(st, cl, log)
	subnet := fixedSubnet{netip.MustParsePrefix(g.subnet)}
	cfg := RunnerConfig{DB: st, Jobs: jobs.NewStore(st, nil), Inv: reg.SystemInvoker(), Refresh: &fakeRefresh{}, Locks: &ServiceLocks{}, Log: log,
		Ops: cl, Alloc: NewAllocator(st, cl, subnet), Tracker: rt, Routes: rt, Net: &fakeNet{status: NetStatus{State: NetOK}}, AppNet: subnet, Instance: g.inst,
		Policy: policy.Options{AllowLoopbackRegistry: true}, HealthInterval: 200 * time.Millisecond}
	if mod != nil {
		mod(&cfg)
	}
	return &itRunner{g: g, cc: cc, cl: cl, st: st, r: NewRunner(cfg), js: cfg.Jobs, rt: rt, svc: addService(t, st, "web"), reg: reg}
}

// deploy queues a deploy and runs its job through the runner directly (the
// scheduler is not what is under test here); it returns the deploy id.
func (x *itRunner) deploy(t *testing.T, s Snapshot) (int64, error) {
	t.Helper()
	raw, _ := s.Marshal()
	out, err := x.js.EnqueueDeploy(context.Background(), jobs.NewDeploy{ServiceID: x.svc, Kind: "deploy", Snapshot: raw, Settings: s.Settings(),
		Actor: jobs.ActorOf(actions.TokenActor("1", actions.ScopeWrite))})
	if err != nil {
		t.Fatal(err)
	}
	j, ok, err := x.js.Pick(context.Background(), "it-boot")
	if err != nil || !ok || j.ID != out.JobID {
		t.Fatalf("pick: %v %v", ok, err)
	}
	_, runErr := x.r.runDeploy(x.g.ctx, actions.Call{}, j.ID)
	return out.DeployID, runErr
}

func (x *itRunner) row(t *testing.T, d int64) (status, code, reason, appSafe string) {
	t.Helper()
	var c, r, a *string
	if err := x.st.R.QueryRow(`SELECT status, reason_code, reason, app_safe FROM deploys WHERE id = ?`, d).Scan(&status, &c, &r, &a); err != nil {
		t.Fatal(err)
	}
	return status, deref(c), deref(r), deref(a)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// candidates are the containers this install has for a deploy (by label).
func (x *itRunner) containersOfDeploy(t *testing.T, d int64) int {
	t.Helper()
	list, err := x.cl.ContainerList(x.g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range list.Containers {
		if c.Labels.Deploy == strconv.FormatInt(d, 10) {
			n++
		}
	}
	return n
}

// TestUnhealthyCandidateKeepsOldServingWithZeroNon2xx: the old version serves under
// constant POST load (keep-alive and new connections) while a deploy of an unhealthy
// candidate runs, fails its health gate and is removed. STRICT: zero non-2xx, zero
// dropped connections, zero Go-internal retries; and proxy_apply is called exactly
// 0 times by the whole failed deploy, checked two ways: the counting caller under
// both clients, and Caddy's active context (a load installs a new one).
func TestUnhealthyCandidateKeepsOldServingWithZeroNon2xx(t *testing.T) {
	g, ref := newSwitchRig(t)
	ctx := g.ctx
	x := newITRunner(t, g, nil)
	base := netip.MustParsePrefix(g.subnet).Addr().As4()
	ipOld := netip.AddrFrom4([4]byte{base[0], base[1], base[2], 2}).String()
	g.startApp(t, ref, "OLD", ipOld) // the version being served, started by a direct container fixture

	// The DB fixture: the old version is the service's active deploy, exposed on a port.
	old := addDeploy(t, x.st, x.svc, 1, "succeeded", ipOld)
	port := itFreePort(t)
	mustExec(t, x.st, `UPDATE services SET public_port = ?, container_port = 8080, active_deploy_id = ? WHERE id = ?`, port, old, x.svc)
	if err := x.rt.ApplyRoutes(ctx, func() error {
		x.rt.state.replace([]Container{{ID: "cOld", Service: x.svc, Deploy: old, State: StateRunning, IP: ipOld}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := g.applier.Applied(); len(got) != 1 {
		t.Fatalf("applied %v", got)
	}

	type class struct {
		name                          string
		total, drops, retries, failed atomic.Int64
		non2xx                        atomic.Int64
	}
	keep, fresh := &class{name: "keep-alive"}, &class{name: "new-connection"}
	var firstBad atomic.Value
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/cgi-bin/id"
	hit := func(c *http.Client, cl *class) {
		cl.total.Add(1)
		var resp *http.Response
		var err error
		for attempt := 0; attempt < 2; attempt++ {
			var gotConns atomic.Int64
			req, _ := http.NewRequest("POST", url, strings.NewReader("payload=1"))
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { gotConns.Add(1) }}))
			resp, err = c.Do(req)
			if n := gotConns.Load(); n > 1 {
				cl.retries.Add(n - 1)
			}
			if err == nil {
				break
			}
			if attempt == 0 {
				cl.drops.Add(1)
				firstBad.CompareAndSwap(nil, fmt.Sprintf("%s: %v", cl.name, err))
			}
		}
		if err != nil {
			cl.failed.Add(1)
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 || !strings.HasPrefix(string(b), "app-OLD") {
			cl.non2xx.Add(1)
			firstBad.CompareAndSwap(nil, fmt.Sprintf("%s: %d %q", cl.name, resp.StatusCode, b))
		}
	}
	stop := make(chan struct{})
	var load sync.WaitGroup
	for _, c := range []*class{keep, fresh} {
		tr := &http.Transport{DisableKeepAlives: c == fresh, MaxIdleConnsPerHost: 4}
		cli := &http.Client{Transport: tr, Timeout: 10 * time.Second}
		load.Go(func() {
			defer tr.CloseIdleConnections()
			for {
				select {
				case <-stop:
					return
				default:
					hit(cli, c)
					time.Sleep(2 * time.Millisecond)
				}
			}
		})
	}
	time.Sleep(300 * time.Millisecond)

	ctxBefore := caddy.ActiveContext()
	applyBefore := x.cc.n("proxy_apply")
	snap := Snapshot{V: 1, ImageRef: ref, ContainerPort: 8080, Health: SnapHealth{Kind: "http", Path: "/definitely-not-here", TimeoutS: 5, Passes: 1},
		Strategy: "blue_green", DrainS: 10}
	d, runErr := x.deploy(t, snap)
	time.Sleep(300 * time.Millisecond)
	loaded := caddyLoaded(ctxBefore)
	applyAfter := x.cc.n("proxy_apply")
	close(stop)
	load.Wait()

	if runErr == nil {
		t.Fatal("the unhealthy deploy reported success")
	}
	status, code, reason, safe := x.row(t, d)
	if status != "failed" || code != CodeHealthTimeout || safe != AppSafeYes {
		t.Fatalf("deploy: %s %s %q app_safe=%s", status, code, reason, safe)
	}
	if n := x.containersOfDeploy(t, d); n != 0 {
		t.Errorf("%d candidate containers left behind", n)
	}
	for _, c := range []*class{keep, fresh} {
		t.Logf("%s: %d requests, non-2xx %d, dropped %d, Go-internal retries %d, failed after a client retry %d",
			c.name, c.total.Load(), c.non2xx.Load(), c.drops.Load(), c.retries.Load(), c.failed.Load())
		if c.total.Load() < 100 {
			t.Errorf("%s: only %d requests", c.name, c.total.Load())
		}
		if c.non2xx.Load() != 0 || c.drops.Load() != 0 || c.retries.Load() != 0 || c.failed.Load() != 0 {
			t.Errorf("%s: non-2xx %d, dropped %d, retries %d, failed %d (first: %v)", c.name, c.non2xx.Load(), c.drops.Load(), c.retries.Load(), c.failed.Load(), firstBad.Load())
		}
	}
	t.Logf("proxy_apply calls during the failed deploy: %d; Caddy loaded: %v", applyAfter-applyBefore, loaded)
	if applyAfter-applyBefore != 0 {
		t.Errorf("proxy_apply was called %d times during the failed deploy, want exactly 0", applyAfter-applyBefore)
	}
	if loaded {
		t.Error("Caddy loaded a config during the failed deploy")
	}
	// The old container is untouched and still serving.
	if out := itDocker(t, "ps", "--filter", "name=sd-it-"+g.inst[:8]+"-old", "--format", "{{.Status}}"); !strings.HasPrefix(out, "Up") {
		t.Errorf("the old version's container: %q", out)
	}
}

// TestStalledPullFails: a registry that accepts the connection and never answers.
// The agent's stall timer ends the pull ("stalled"); the deploy fails pull_stalled
// without a retry, nothing is created, and the failure comes within seconds.
func TestStalledPullFails(t *testing.T) {
	g, _ := newSwitchRig(t)
	x := newITRunner(t, g, func(c *RunnerConfig) { c.StallTimeout = 2 * time.Second })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int64
	hold := make(chan struct{})
	defer close(hold)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() { <-hold; c.Close() }() // never answers
		}
	}()
	ref := "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + "/stall/app:1"
	snap := Snapshot{V: 1, ImageRef: ref, ContainerPort: 8080, Health: SnapHealth{Kind: "tcp", TimeoutS: 5, Passes: 1}, Strategy: "blue_green", DrainS: 10}
	t0 := time.Now()
	d, runErr := x.deploy(t, snap)
	took := time.Since(t0)
	if runErr == nil {
		t.Fatal("a stalled pull reported success")
	}
	if jobs.IsRetryable(runErr) {
		t.Error("a stalled pull is not retried")
	}
	status, code, reason, _ := x.row(t, d)
	t.Logf("stalled pull ended after %v: %s %s %q (registry connections %d)", took, status, code, reason, accepted.Load())
	if status != "failed" || code != CodePullStalled {
		t.Fatalf("deploy: %s %s %q", status, code, reason)
	}
	if took < 2*time.Second || took > 30*time.Second {
		t.Errorf("took %v, want about the 2 s stall limit", took)
	}
	if n := x.containersOfDeploy(t, d); n != 0 {
		t.Errorf("%d containers", n)
	}
}
