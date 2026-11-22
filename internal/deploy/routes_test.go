package deploy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// fakeProxy records every proxy_apply. It can fail, report failed ports, and
// sleep (so concurrent callers really overlap if the route lock were missing).
type fakeProxy struct {
	mu      sync.Mutex
	calls   [][]agentops.Route
	events  *[]string // shared sequence log (may be nil)
	fail    error
	failed  []int
	sleep   time.Duration
	busy    bool // detects two proxy_apply running at once
	overlap bool
}

func (f *fakeProxy) ProxyApply(_ context.Context, routes []agentops.Route) (agentops.ApplyResult, error) {
	f.mu.Lock()
	if f.busy {
		f.overlap = true
	}
	f.busy = true
	f.mu.Unlock()
	time.Sleep(f.sleep)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy = false
	if f.fail != nil {
		return agentops.ApplyResult{}, f.fail
	}
	f.calls = append(f.calls, append([]agentops.Route(nil), routes...))
	if f.events != nil {
		*f.events = append(*f.events, "proxy_apply")
	}
	return agentops.ApplyResult{Applied: true, FailedPorts: f.failed}, nil
}

func (f *fakeProxy) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeProxy) last() []agentops.Route {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func cid(n int) string { return fmt.Sprintf("%064x", n) }

// expose makes service svc public on port with an active deploy (number n,
// address ip, container port cport) and returns the deploy id.
func expose(t *testing.T, st *store.Store, svc int64, port, cport int, number int, ip string) int64 {
	t.Helper()
	d := addDeploy(t, st, svc, number, "succeeded", ip)
	mustExec(t, st, `UPDATE services SET public_port = ?, container_port = ?, active_deploy_id = ? WHERE id = ?`, port, cport, d, svc)
	return d
}

func newRoutes(t *testing.T) (*Routes, *store.Store, *fakeProxy) {
	t.Helper()
	st := openStore(t)
	px := &fakeProxy{}
	return NewRoutes(st, px, nil), st, px
}

// open opens the boot gate with the given containers.
func open(t *testing.T, r *Routes, cs ...Container) {
	t.Helper()
	if err := r.ApplyRoutes(context.Background(), func() { r.state.replace(cs) }); err != nil {
		t.Fatal(err)
	}
}

func up(svc, dep int64, n int, state, ip string) Container {
	return Container{ID: cid(n), Service: svc, Deploy: dep, State: state, IP: ip}
}

var bg = context.Background()

func TestNoReloadWhenRoutesUnchanged(t *testing.T) {
	r, st, px := newRoutes(t)
	svc := addService(t, st, "web")
	d := expose(t, st, svc, 9000, 8080, 1, "10.250.1.2")
	open(t, r, up(svc, d, 1, StateRunning, "10.250.1.2"))
	if px.n() != 1 || !reflect.DeepEqual(px.last(), []agentops.Route{{Port: 9000, Upstream: "10.250.1.2:8080"}}) {
		t.Fatalf("%d %+v", px.n(), px.last())
	}
	for i := 0; i < 5; i++ {
		if err := r.Refresh(bg); err != nil {
			t.Fatal(err)
		}
	}
	// Same entry set again, the override cleared when none was set, an unrelated
	// container: none of them changes a route.
	_ = r.SetContainer(bg, up(svc, d, 1, StateRunning, "10.250.1.2"))
	_ = r.ClearOverride(bg, svc)
	_ = r.SetContainer(bg, up(99, 99, 7, StateRunning, "10.250.1.9"))
	if px.n() != 1 {
		t.Fatalf("%d proxy_apply calls for unchanged routes", px.n())
	}
	// A real change does reload.
	_ = r.SetContainer(bg, up(svc, d, 1, "exited", "10.250.1.2"))
	if px.n() != 2 || px.last()[0].Page != agentops.PageNotRunning {
		t.Fatalf("%d %+v", px.n(), px.last())
	}
}

func TestCrashLoopingContainerCausesNoReload(t *testing.T) {
	r, st, px := newRoutes(t)
	svc := addService(t, st, "web")
	d := expose(t, st, svc, 9000, 8080, 1, "10.250.1.2")
	open(t, r, up(svc, d, 1, StateRunning, "10.250.1.2"))
	after := px.n()
	// start, die (restarting: no live address, only the configured one), start...
	for i := 0; i < 50; i++ {
		restarting, _ := ContainerFromInspect(agentops.ContainerState{ID: cid(1), State: "restarting", Restarting: true,
			ConfiguredIP: "10.250.1.2", Owned: true, Labels: lbl(svc, d)})
		running, _ := ContainerFromInspect(agentops.ContainerState{ID: cid(1), State: "running", Running: true,
			IP: "10.250.1.2", ConfiguredIP: "10.250.1.2", Owned: true, Labels: lbl(svc, d)})
		if restarting.State != StateRestarting || restarting.IP != "10.250.1.2" || running.State != StateRunning {
			t.Fatalf("%+v %+v", restarting, running)
		}
		if err := r.SetContainer(bg, restarting); err != nil {
			t.Fatal(err)
		}
		if err := r.SetContainer(bg, running); err != nil {
			t.Fatal(err)
		}
	}
	if px.n() != after {
		t.Fatalf("a crash loop caused %d proxy_apply calls", px.n()-after)
	}
}

func lbl(svc, dep int64) map[string]string {
	return map[string]string{agentops.LabelService: strconv.FormatInt(svc, 10), agentops.LabelDeploy: strconv.FormatInt(dep, 10)}
}

func TestContainerFromInspect(t *testing.T) {
	ok := agentops.ContainerState{ID: cid(1), State: "running", Running: true, IP: "10.1.0.2", ConfiguredIP: "10.1.0.2", Owned: true, Labels: lbl(3, 4)}
	if c, good := ContainerFromInspect(ok); !good || c.Service != 3 || c.Deploy != 4 || c.IP != "10.1.0.2" {
		t.Fatalf("%+v", c)
	}
	un := ok
	un.Owned = false
	if _, good := ContainerFromInspect(un); good {
		t.Error("an unowned container was accepted")
	}
	for _, bad := range []map[string]string{lbl(0, 4), lbl(3, 0), {agentops.LabelService: "x", agentops.LabelDeploy: "4"}, {}} {
		c := ok
		c.Labels = bad
		if _, good := ContainerFromInspect(c); good {
			t.Errorf("labels %v accepted", bad)
		}
	}
	// The live address and the configured one disagree: never an upstream.
	mis := ok
	mis.IP = "10.1.0.9"
	if c, _ := ContainerFromInspect(mis); c.IP != "" {
		t.Errorf("address %q for a mismatch", c.IP)
	}
}

func TestStaticNotRunningAndMaintenancePages(t *testing.T) {
	r, st, px := newRoutes(t)
	a := addService(t, st, "a")
	b := addService(t, st, "b")
	c := addService(t, st, "c")
	da := expose(t, st, a, 9000, 8080, 1, "10.250.1.2")
	expose(t, st, b, 9001, 8080, 1, "10.250.1.3")                                                    // no container at all
	mustExec(t, st, `UPDATE services SET public_port = 9002, container_port = 8080 WHERE id = ?`, c) // never deployed
	open(t, r, up(a, da, 1, StateRunning, "10.250.1.2"))
	want := []agentops.Route{
		{Port: 9000, Upstream: "10.250.1.2:8080"},
		{Port: 9001, Page: agentops.PageNotRunning},
		{Port: 9002, Page: agentops.PageNotRunning},
	}
	if !reflect.DeepEqual(px.last(), want) {
		t.Fatalf("%+v", px.last())
	}
	// Maintenance wins over a running container, then clears.
	if err := r.SetOverride(bg, a, Override{Kind: OverrideMaintenance}); err != nil {
		t.Fatal(err)
	}
	if got := px.last()[0]; got.Page != agentops.PageMaintenance || got.Upstream != "" {
		t.Fatalf("%+v", got)
	}
	if err := r.ClearOverride(bg, a); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(px.last(), want) {
		t.Fatalf("%+v", px.last())
	}
	// A service that is not exposed has no route.
	mustExec(t, st, `UPDATE services SET public_port = NULL WHERE id = ?`, c)
	_ = r.Refresh(bg)
	if len(px.last()) != 2 {
		t.Fatalf("%+v", px.last())
	}
}

func TestUpstreamOnlyForTheServicesOwnContainerAtItsAddress(t *testing.T) {
	cases := map[string]func(svc, dep, other int64) []Container{
		"another service's container at the address": func(svc, dep, other int64) []Container {
			return []Container{up(other, 77, 1, StateRunning, "10.250.1.2")}
		},
		"another service's container with the same deploy id": func(svc, dep, other int64) []Container {
			return []Container{up(other, dep, 1, StateRunning, "10.250.1.2")}
		},
		"the right service, another deploy": func(svc, dep, other int64) []Container {
			return []Container{up(svc, dep+1, 1, StateRunning, "10.250.1.2")}
		},
		"a stopped own container": func(svc, dep, other int64) []Container {
			return []Container{up(svc, dep, 1, "exited", "10.250.1.2")}
		},
		"a created, never started one": func(svc, dep, other int64) []Container {
			return []Container{up(svc, dep, 1, "created", "10.250.1.2")}
		},
		"an address mismatch": func(svc, dep, other int64) []Container {
			return []Container{up(svc, dep, 1, StateRunning, "10.250.1.9")}
		},
		"no address known": func(svc, dep, other int64) []Container {
			return []Container{up(svc, dep, 1, StateRunning, "")}
		},
		"no container": func(svc, dep, other int64) []Container { return nil },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			r, st, px := newRoutes(t)
			svc := addService(t, st, "x")
			other := addService(t, st, "y")
			d := expose(t, st, svc, 9000, 8080, 1, "10.250.1.2")
			open(t, r, mk(svc, d, other)...)
			if got := px.last(); len(got) != 1 || got[0].Page != agentops.PageNotRunning || got[0].Upstream != "" {
				t.Fatalf("%+v", got)
			}
		})
	}
	// Positive controls: running and restarting at the address, with the other
	// service's container beside it.
	for _, state := range []string{StateRunning, StateRestarting} {
		r, st, px := newRoutes(t)
		svc := addService(t, st, "x")
		other := addService(t, st, "y")
		d := expose(t, st, svc, 9000, 8080, 1, "10.250.1.2")
		open(t, r, up(other, 5, 1, StateRunning, "10.250.1.3"), up(svc, d, 2, state, "10.250.1.2"))
		if got := px.last(); len(got) != 1 || got[0].Upstream != "10.250.1.2:8080" {
			t.Fatalf("%s: %+v", state, got)
		}
	}
	// The container port is the active deploy's snapshot's, not the service's
	// later setting.
	r, st, px := newRoutes(t)
	svc := addService(t, st, "x")
	res, err := st.W.Exec(`INSERT INTO deploys(service_id, number, kind, snapshot_json, ip, status, actor_kind, actor_id, created_at)
		VALUES (?, 1, 'deploy', '{"container_port":8080}', '10.250.1.2', 'succeeded', 'user', '1', 'x')`, svc)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := res.LastInsertId()
	mustExec(t, st, `UPDATE services SET public_port = 9000, container_port = 9999, active_deploy_id = ? WHERE id = ?`, d, svc)
	open(t, r, up(svc, d, 1, StateRunning, "10.250.1.2"))
	if got := px.last(); got[0].Upstream != "10.250.1.2:8080" {
		t.Fatalf("%+v", got)
	}
}

func TestOverrideWinsDuringSwitch(t *testing.T) {
	r, st, px := newRoutes(t)
	svc := addService(t, st, "web")
	old := expose(t, st, svc, 9000, 8080, 1, "10.250.1.2")
	cand := addDeploy(t, st, svc, 2, "running", "10.250.1.3")
	open(t, r, up(svc, old, 1, StateRunning, "10.250.1.2"), up(svc, cand, 2, StateRunning, "10.250.1.3"))
	if px.last()[0].Upstream != "10.250.1.2:8080" {
		t.Fatalf("%+v", px.last())
	}
	// The switch: the override points at the candidate while the DB still says
	// the old deploy is active.
	if err := r.SetOverride(bg, svc, Override{Kind: OverrideUpstream, Deploy: cand, IP: "10.250.1.3", Port: 8081}); err != nil {
		t.Fatal(err)
	}
	if px.last()[0].Upstream != "10.250.1.3:8081" {
		t.Fatalf("%+v", px.last())
	}
	// Events about the old container, a DB refresh, even the old one dying: the
	// override stays.
	n := px.n()
	_ = r.SetContainer(bg, up(svc, old, 1, "exited", "10.250.1.2"))
	_ = r.Refresh(bg)
	if px.n() != n || px.last()[0].Upstream != "10.250.1.3:8081" {
		t.Fatalf("override lost: %d %+v", px.n(), px.last())
	}
	// An upstream override still needs its own container: the candidate dying
	// puts the page up, not the old container.
	_ = r.SetContainer(bg, up(svc, cand, 2, "exited", "10.250.1.3"))
	if px.last()[0].Page != agentops.PageNotRunning {
		t.Fatalf("%+v", px.last())
	}
	_ = r.SetContainer(bg, up(svc, cand, 2, StateRunning, "10.250.1.3"))
	// Maintenance beats an upstream override; the commit (the DB write as the
	// mutation) plus clearing lands on the new deploy.
	_ = r.SetOverride(bg, svc, Override{Kind: OverrideMaintenance})
	if px.last()[0].Page != agentops.PageMaintenance {
		t.Fatalf("%+v", px.last())
	}
	err := r.ApplyRoutes(bg, func() {
		mustExec(t, st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, cand, svc)
		delete(r.overrides, svc)
	})
	if err != nil {
		t.Fatal(err)
	}
	if px.last()[0].Upstream != "10.250.1.3:8080" { // no snapshot port: the service's
		t.Fatalf("%+v", px.last())
	}
}

func TestBootAppliesAppRoutesOnlyAfterFirstListAndInspect(t *testing.T) {
	r, st, px := newRoutes(t)
	var seq []string
	px.events = &seq
	svc := addService(t, st, "web")
	d := expose(t, st, svc, 9000, 8080, 1, "10.250.1.2")
	src := &fakeSource{seq: &seq, list: []agentops.ListedContainer{
		{ID: cid(1), State: "running"},
		{ID: cid(2), State: "running", Unowned: true},
		{ID: cid(3), State: "exited"}, // gone before its inspect
	}, inspect: map[string]agentops.ContainerState{
		cid(1): {ID: cid(1), State: "running", Running: true, IP: "10.250.1.2", ConfiguredIP: "10.250.1.2", Owned: true, Labels: lbl(svc, d)},
	}}

	// Before the first list and inspect: events, overrides, refreshes and DB
	// changes all happen, and nothing is sent.
	_ = r.Refresh(bg)
	_ = r.SetContainer(bg, up(svc, d, 1, StateRunning, "10.250.1.2"))
	_ = r.SetOverride(bg, svc, Override{Kind: OverrideMaintenance})
	_ = r.ClearOverride(bg, svc)
	if px.n() != 0 || r.Ready() {
		t.Fatalf("%d proxy_apply calls before the state map was filled", px.n())
	}
	// A list that fails, and an inspect that fails (not "gone"): still closed.
	src.listErr = errors.New("docker down")
	if err := r.Sync(bg, src); err == nil || px.n() != 0 || r.Ready() {
		t.Fatalf("%v %d %v", err, px.n(), r.Ready())
	}
	src.listErr = nil
	src.inspectErr = &agent.OpError{Code: agent.CodeDocker, Msg: "x"}
	if err := r.Sync(bg, src); err == nil || px.n() != 0 || r.Ready() {
		t.Fatalf("%v %d %v", err, px.n(), r.Ready())
	}
	src.inspectErr = nil
	seq = nil

	if err := r.Sync(bg, src); err != nil {
		t.Fatal(err)
	}
	if !r.Ready() || px.n() != 1 || !reflect.DeepEqual(px.last(), []agentops.Route{{Port: 9000, Upstream: "10.250.1.2:8080"}}) {
		t.Fatalf("%d %+v", px.n(), px.last())
	}
	// The order: the list, every inspect, and only then the proxy.
	if got := strings.Join(seq, ","); got != "list,inspect,inspect,inspect,proxy_apply" && got != "list,inspect,inspect,proxy_apply" {
		t.Fatalf("sequence %s", got)
	}
	if seq[len(seq)-1] != "proxy_apply" || seq[0] != "list" {
		t.Fatalf("sequence %v", seq)
	}
	// The unowned container is not in the state map.
	if _, ok := r.State().Get(cid(2)); ok {
		t.Error("an unowned container is routable")
	}
}

type fakeSource struct {
	seq        *[]string
	list       []agentops.ListedContainer
	listErr    error
	inspect    map[string]agentops.ContainerState
	inspectErr error
}

func (f *fakeSource) ContainerList(context.Context) (agentops.ListResult, error) {
	*f.seq = append(*f.seq, "list")
	return agentops.ListResult{Containers: f.list}, f.listErr
}

func (f *fakeSource) ContainerInspect(_ context.Context, id string) (agentops.ContainerState, error) {
	*f.seq = append(*f.seq, "inspect")
	if f.inspectErr != nil {
		return agentops.ContainerState{}, f.inspectErr
	}
	st, ok := f.inspect[id]
	if !ok {
		return agentops.ContainerState{}, &agent.OpError{Code: agent.CodeNotFound, Msg: "not found"}
	}
	return st, nil
}

func TestFailedApplyIsRetriedAndBindResultsAreRecorded(t *testing.T) {
	r, st, px := newRoutes(t)
	svc := addService(t, st, "web")
	d := expose(t, st, svc, 9000, 8080, 1, "10.250.1.2")
	px.fail = errors.New("agent gone")
	if err := r.ApplyRoutes(bg, func() { r.state.replace([]Container{up(svc, d, 1, StateRunning, "10.250.1.2")}) }); err == nil {
		t.Fatal("a failed proxy_apply was reported as success")
	}
	px.fail = nil
	px.failed = []int{9000}
	if err := r.Refresh(bg); err != nil || px.n() != 1 { // the hash was not recorded by the failure
		t.Fatalf("%v %d", err, px.n())
	}
	if msg := r.BindStatus(9000); msg != "port 9000 is in use by another program on this server" {
		t.Fatalf("%q", msg)
	}
	if r.BindStatus(9001) != "" {
		t.Error("a port that was not reported failed")
	}
	// The next apply with a change clears it.
	px.failed = nil
	_ = r.SetContainer(bg, up(svc, d, 1, "exited", "10.250.1.2"))
	if r.BindStatus(9000) != "" {
		t.Error("the bind failure outlived the next apply")
	}
}

func TestConcurrentRouteChangesForTwoServicesLoseNothing(t *testing.T) {
	r, st, px := newRoutes(t)
	px.sleep = time.Millisecond // make overlap likely if the lock were missing
	a, b, c := addService(t, st, "a"), addService(t, st, "b"), addService(t, st, "c")
	oldA := expose(t, st, a, 9000, 8080, 1, "10.250.1.2")
	oldB := expose(t, st, b, 9001, 8080, 1, "10.250.1.3")
	expose(t, st, c, 9002, 8080, 1, "10.250.1.4")
	newA := addDeploy(t, st, a, 2, "running", "10.250.1.5")
	newB := addDeploy(t, st, b, 2, "running", "10.250.1.6")
	cDeploy := int64(0)
	_ = st.R.QueryRow(`SELECT active_deploy_id FROM services WHERE id = ?`, c).Scan(&cDeploy)
	open(t, r,
		up(a, oldA, 1, StateRunning, "10.250.1.2"), up(b, oldB, 2, StateRunning, "10.250.1.3"),
		up(c, cDeploy, 3, StateRunning, "10.250.1.4"),
		up(a, newA, 4, StateRunning, "10.250.1.5"), up(b, newB, 5, StateRunning, "10.250.1.6"))

	switchService := func(svc, newDeploy int64, ip string) {
		// stage 5, switch: an upstream override; stage 6, commit: the DB write.
		if err := r.SetOverride(bg, svc, Override{Kind: OverrideUpstream, Deploy: newDeploy, IP: ip}); err != nil {
			t.Error(err)
		}
		err := r.ApplyRoutes(bg, func() {
			mustExec(t, st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, newDeploy, svc)
			delete(r.overrides, svc)
		})
		if err != nil {
			t.Error(err)
		}
	}
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); switchService(a, newA, "10.250.1.5") }()
	go func() { defer wg.Done(); switchService(b, newB, "10.250.1.6") }()
	go func() { // events: both old containers flip, repeatedly, then die
		defer wg.Done()
		for i := 0; i < 30; i++ {
			s := StateRestarting
			if i%2 == 0 {
				s = StateRunning
			}
			_ = r.SetContainer(bg, up(a, oldA, 1, s, "10.250.1.2"))
			_ = r.SetContainer(bg, up(b, oldB, 2, s, "10.250.1.3"))
		}
		_ = r.RemoveContainer(bg, cid(1))
		_ = r.RemoveContainer(bg, cid(2))
	}()
	go func() { // services.set_public_port for c: the write is the mutation
		defer wg.Done()
		err := r.ApplyRoutes(bg, func() { mustExec(t, st, `UPDATE services SET public_port = 9100 WHERE id = ?`, c) })
		if err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()

	want := []agentops.Route{
		{Port: 9000, Upstream: "10.250.1.5:8080"},
		{Port: 9001, Upstream: "10.250.1.6:8080"},
		{Port: 9100, Upstream: "10.250.1.4:8080"},
	}
	if got := px.last(); !reflect.DeepEqual(got, want) {
		t.Fatalf("final applied config:\n got %+v\nwant %+v", got, want)
	}
	if px.overlap {
		t.Error("two proxy_apply calls ran at once: the route lock is not global")
	}
	// Nothing is pending: a refresh finds the applied config current.
	n := px.n()
	_ = r.Refresh(bg)
	if px.n() != n {
		t.Error("the last applied config was not what the state says")
	}
}
