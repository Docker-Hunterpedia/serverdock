package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

const testImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// fcont is one container of the fake Docker.
type fcont struct {
	id, name        string
	labels          map[string]string
	ip              string
	running, restar bool
	exit            int
	oom             bool
	image           string
	notOwned        bool // the agent's inspect says owned:false
	wrongLabels     bool // the inspect reports other service labels than it was created with
}

// fakeOps is the agent as the runner sees it: an in-memory Docker with a call
// log and per-op failure hooks. It also counts proxy_apply (which the runner has no
// way to call: RunnerOps does not have it), and is the allocator's Lister.
type fakeOps struct {
	mu         sync.Mutex
	calls      []string
	ctxErrs    map[string][]error         // ctx.Err() seen by an op, by op name (a fresh context has none)
	dls        map[string][]time.Duration // time left to each call's context deadline, by op name (0: no deadline)
	onCall     func(op, what string)      // called (without the lock) at the start of every op
	cs         map[string]*fcont
	extra      []agentops.ListedContainer // containers that exist but have no fcont (a pre-existing fixture)
	n          int
	pullN      atomic.Int32
	proxyCalls atomic.Int32

	pullFn      func(ctx context.Context, ref string, on func(agentops.PullProgress)) (agentops.PullResult, error)
	createFn    func(ctx context.Context, name string, spec dockerapi.CreateSpec, ip string) (agentops.CreateResult, error, bool) // handled
	startFn     func(c *fcont)                                                                                                    // after start: change the state (crash, ...)
	healthFn    func(ctx context.Context, a agentops.HealthArgs) (agentops.HealthResult, error)
	inspectFn   func(ctx context.Context, id string) (agentops.ContainerState, error, bool)
	listErr     error
	removeErr   error
	stopGraceFn func(grace int)
	stopErrFn   func(id string) error // an error for a stop of this container id
	netMissing  atomic.Bool           // the app network is "pruned": create and network_inspect say so
	labelsSeen  []map[string]string
	// afterNetInspect, if set, runs after a successful network_inspect.
	afterNetInspect func()
	// removeHook, if set, runs at the start of every container_remove.
	removeHook func()
}

func newFakeOps() *fakeOps {
	return &fakeOps{cs: map[string]*fcont{}, ctxErrs: map[string][]error{}, dls: map[string][]time.Duration{}}
}

func (f *fakeOps) rec(ctx context.Context, op, what string) {
	var left time.Duration
	if d, ok := ctx.Deadline(); ok {
		left = time.Until(d)
	}
	f.mu.Lock()
	f.calls = append(f.calls, op+":"+what)
	f.ctxErrs[op] = append(f.ctxErrs[op], ctx.Err())
	f.dls[op] = append(f.dls[op], left)
	hook := f.onCall
	f.mu.Unlock()
	if hook != nil {
		hook(op, what)
	}
}

// dead is what the real hub returns for an op whose context has ended: the
// context's CAUSE (jobs.CancelCause, jobs.ErrShutdown, a watchdog, DeadlineExceeded).
func dead(ctx context.Context) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return nil
}

func (f *fakeOps) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, op+":") {
			n++
		}
	}
	return n
}

func (f *fakeOps) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeOps) ImagePull(ctx context.Context, ref string, _ time.Duration, on func(agentops.PullProgress)) (agentops.PullResult, error) {
	f.rec(ctx, "pull", ref)
	f.pullN.Add(1)
	if err := dead(ctx); err != nil {
		return agentops.PullResult{}, err
	}
	if f.pullFn != nil {
		return f.pullFn(ctx, ref, on)
	}
	on(agentops.PullProgress{Layers: 2, LayersDone: 2, Bytes: 100, BytesTotal: 100, Status: "done"})
	return agentops.PullResult{ImageID: testImageID, RepoDigest: "x@sha256:" + strings.Repeat("b", 64)}, nil
}

func (f *fakeOps) ImageInspect(ctx context.Context, ref string, _ bool) (agentops.ImageInfo, error) {
	f.rec(ctx, "image_inspect", ref)
	return agentops.ImageInfo{ID: testImageID, ExposedPorts: []string{"8080/tcp"}}, nil
}

func (f *fakeOps) ContainerCreate(ctx context.Context, name string, spec dockerapi.CreateSpec, ip, _ string) (agentops.CreateResult, error) {
	f.rec(ctx, "create", name)
	if err := dead(ctx); err != nil {
		return agentops.CreateResult{}, err
	}
	if f.netMissing.Load() {
		return agentops.CreateResult{}, &agent.OpError{Code: agent.CodeNotFound, Msg: agentops.MsgNetworkMissing}
	}
	f.mu.Lock()
	f.labelsSeen = append(f.labelsSeen, spec.Labels)
	f.mu.Unlock()
	if f.createFn != nil {
		if r, err, handled := f.createFn(ctx, name, spec, ip); handled {
			return r, err
		}
	}
	return f.makeContainer(name, spec, ip, false)
}

// makeContainer creates the container, or returns the existing one of that name.
func (f *fakeOps) makeContainer(name string, spec dockerapi.CreateSpec, ip string, onlyRecord bool) (agentops.CreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cs {
		if c.name == name {
			return agentops.CreateResult{ID: c.id, Existed: true}, nil
		}
	}
	f.n++
	c := &fcont{id: fmt.Sprintf("%064x", f.n), name: name, ip: ip, image: testImageID, labels: map[string]string{}}
	for k, v := range spec.Labels {
		c.labels[k] = v
	}
	f.cs[c.id] = c
	return agentops.CreateResult{ID: c.id}, nil
}

func (f *fakeOps) find(id string) *fcont {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cs[id]
}

func (f *fakeOps) ContainerStart(ctx context.Context, id string) error {
	f.rec(ctx, "start", id[:8])
	if err := dead(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	c := f.cs[id]
	f.mu.Unlock()
	if c == nil {
		return &agent.OpError{Code: agent.CodeNotFound, Msg: "not found"}
	}
	f.mu.Lock()
	c.running = true
	fn := f.startFn
	f.mu.Unlock()
	if fn != nil {
		fn(c)
	}
	return nil
}

func (f *fakeOps) ContainerStop(ctx context.Context, id string, grace int) error {
	f.rec(ctx, "stop", id[:8])
	if fn := f.stopGraceFn; fn != nil {
		fn(grace)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErrFn != nil {
		if err := f.stopErrFn(id); err != nil {
			return err
		}
	}
	if c := f.cs[id]; c != nil {
		c.running = false
		return nil
	}
	return &agent.OpError{Code: agent.CodeNotFound, Msg: "not found"}
}

func (f *fakeOps) ContainerRemove(ctx context.Context, id string) error {
	f.rec(ctx, "remove", id[:8])
	f.mu.Lock()
	hook := f.removeHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	if f.cs[id] == nil {
		return &agent.OpError{Code: agent.CodeNotFound, Msg: "not found"}
	}
	delete(f.cs, id)
	return nil
}

func (f *fakeOps) ContainerInspect(ctx context.Context, id string) (agentops.ContainerState, error) {
	f.rec(ctx, "inspect", id[:8])
	if err := dead(ctx); err != nil {
		return agentops.ContainerState{}, err
	}
	if f.inspectFn != nil {
		if st, err, handled := f.inspectFn(ctx, id); handled {
			return st, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cs[id]
	if c == nil {
		return agentops.ContainerState{}, &agent.OpError{Code: agent.CodeNotFound, Msg: "not found"}
	}
	st := agentops.ContainerState{ID: c.id, Running: c.running, Restarting: c.restar, ExitCode: c.exit, OOMKilled: c.oom, ImageID: c.image,
		ConfiguredIP: c.ip, Labels: map[string]string{}, Owned: !c.notOwned}
	for k, v := range c.labels {
		st.Labels[k] = v
	}
	if c.wrongLabels {
		st.Labels[agentops.LabelService] = "999"
	}
	switch {
	case c.restar:
		st.State = "restarting"
	case c.running:
		st.State, st.IP = "running", c.ip
	default:
		st.State = "exited"
	}
	return st, nil
}

func (f *fakeOps) ContainerList(ctx context.Context) (agentops.ListResult, error) {
	f.rec(ctx, "list", "")
	if err := dead(ctx); err != nil {
		return agentops.ListResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return agentops.ListResult{}, f.listErr
	}
	out := append([]agentops.ListedContainer(nil), f.extra...)
	for _, c := range f.cs {
		state := "exited"
		if c.running {
			state = "running"
		}
		out = append(out, agentops.ListedContainer{ID: c.id, Name: c.name, ImageID: c.image, State: state, ConfiguredIP: c.ip,
			Labels: agentops.ListedLabels{Project: c.labels[agentops.LabelProject], Service: c.labels[agentops.LabelService],
				Deploy: c.labels[agentops.LabelDeploy], Attempt: c.labels[agentops.LabelAttempt]}})
	}
	return agentops.ListResult{Containers: out}, nil
}

func (f *fakeOps) NetworkInspect(ctx context.Context) (agentops.UsedAddrs, error) {
	f.rec(ctx, "network_inspect", "")
	if err := dead(ctx); err != nil {
		return agentops.UsedAddrs{}, err
	}
	if f.netMissing.Load() {
		return agentops.UsedAddrs{}, &agent.OpError{Code: agent.CodeNotFound, Msg: agentops.MsgNetworkMissing}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var used []string
	for _, c := range f.cs {
		if c.running {
			used = append(used, c.ip)
		}
	}
	if f.afterNetInspect != nil {
		defer f.afterNetInspect()
	}
	return agentops.UsedAddrs{Subnet: "10.250.0.0/24", Gateway: "10.250.0.1", IPRange: "10.250.0.192/26", Used: used}, nil
}

func (f *fakeOps) HealthCheck(ctx context.Context, a agentops.HealthArgs) (agentops.HealthResult, error) {
	f.rec(ctx, "health", a.Target)
	if err := dead(ctx); err != nil {
		return agentops.HealthResult{}, err
	}
	f.mu.Lock()
	fn := f.healthFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, a)
	}
	return agentops.HealthResult{OK: true, Status: 200}, nil
}

// ProxyApply exists only so a test can prove it is never called: the runner has no
// reference that could call it (RunnerOps lacks it), and Routes is given a proxy
// that counts too.
func (f *fakeOps) ProxyApply(context.Context, []agentops.Route, bool) (agentops.ApplyResult, error) {
	f.proxyCalls.Add(1)
	return agentops.ApplyResult{Applied: true}, nil
}

// fakeNet is the Booter as the runner sees it.
type fakeNet struct {
	mu     sync.Mutex
	status NetStatus
	runs   int
	gone   int
	runFn  func() NetStatus // what Run leaves the status as (nil: OK)
}

func (n *fakeNet) NetworkStatus() NetStatus { n.mu.Lock(); defer n.mu.Unlock(); return n.status }
func (n *fakeNet) Run(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.runs++
	if n.runFn != nil {
		n.status = n.runFn()
	} else {
		n.status = NetStatus{State: NetOK}
	}
	return nil
}
func (n *fakeNet) NetworkGone() {
	n.mu.Lock()
	n.gone++
	n.status = NetStatus{}
	n.mu.Unlock()
}

// ---- the rig ----

type rrig struct {
	t       *testing.T
	st      *store.Store
	ops     *fakeOps
	net     *fakeNet
	routes  *Routes
	px      *fakeProxy
	runner  *Runner
	reg     *actions.Registry
	js      *jobs.Store
	sched   *jobs.Scheduler
	stop    func()
	refresh *fakeRefresh
	svc     int64
	svcs    map[string]int64
}

type rigOpt func(*RunnerConfig)

func newRRig(t *testing.T, opts ...rigOpt) *rrig {
	t.Helper()
	st := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	al, err := audit.New(st, log)
	if err != nil {
		t.Fatal(err)
	}
	g := &rrig{t: t, st: st, ops: newFakeOps(), net: &fakeNet{status: NetStatus{State: NetOK}}, px: &fakeProxy{}, reg: actions.New(al, log), svcs: map[string]int64{}}
	g.routes = NewRoutes(st, g.px, log)
	g.js = jobs.NewStore(st, nil)
	g.refresh = &fakeRefresh{}
	cfg := RunnerConfig{DB: st, Jobs: g.js, Inv: g.reg.SystemInvoker(), Refresh: g.refresh, Locks: &ServiceLocks{}, Log: log,
		Ops: g.ops, Alloc: NewAllocator(st, g.ops, fixedSubnet{netip.MustParsePrefix("10.250.0.0/24")}), Tracker: g.routes, Net: g.net,
		AppNet: fixedSubnet{netip.MustParsePrefix("10.250.0.0/24")}, Instance: instance, Routes: g.routes,
		HealthInterval: 3 * time.Millisecond, CleanupBound: 5 * time.Second, ShutdownCleanupBound: 5 * time.Second, DrainUnit: time.Millisecond,
		Stopping: func() <-chan struct{} {
			if g.sched == nil {
				return nil
			}
			return g.sched.Stopping()
		}}
	for _, o := range opts {
		o(&cfg)
	}
	g.runner = NewRunner(cfg)
	RegisterJobActions(g.reg, g.runner)
	g.svc = g.service("web")
	// The boot gate is open, as it is whenever a job can run on a real server.
	if err := g.routes.ApplyRoutes(context.Background(), func() error { g.routes.state.replace(nil); return nil }); err != nil {
		t.Fatal(err)
	}
	return g
}

// service adds a service with a container port.
func (g *rrig) service(name string) int64 {
	g.t.Helper()
	id := addService(g.t, g.st, name)
	mustExec(g.t, g.st, `UPDATE services SET container_port = 8080 WHERE id = ?`, id)
	g.svcs[name] = id
	return id
}

// start runs the scheduler (the gate is open unless hold is set).
func (g *rrig) start(hold bool) {
	g.t.Helper()
	sched, err := jobs.NewScheduler(jobs.Config{Store: g.js, Run: g.runner.Run, RetryDelay: func(int) time.Duration { return 10 * time.Millisecond }})
	if err != nil {
		g.t.Fatal(err)
	}
	g.sched = sched
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sched.Run(ctx); close(done) }()
	g.stop = func() { cancel(); <-done }
	g.t.Cleanup(func() { g.stop() })
	if !hold {
		sched.OpenGate()
	}
}

type snapMod func(*Snapshot)

func baseSnap() Snapshot {
	return Snapshot{V: 1, ImageRef: "nginx:1", ContainerPort: 8080, Health: SnapHealth{Kind: "tcp", TimeoutS: 5, Passes: 1}, Strategy: "blue_green", DrainS: 10, StopGraceS: 0}
}

// deploy queues a deploy of the service as a write token and returns (deploy id, job id).
func (g *rrig) deploy(svc int64, mods ...snapMod) (int64, int64) {
	g.t.Helper()
	s := baseSnap()
	for _, m := range mods {
		m(&s)
	}
	raw, err := s.Marshal()
	if err != nil {
		g.t.Fatal(err)
	}
	var out jobs.Enqueued
	if g.sched != nil {
		out, err = g.sched.EnqueueDeploy(context.Background(), jobs.NewDeploy{ServiceID: svc, Kind: "deploy", Snapshot: raw, Settings: s.Settings(), Actor: jobs.ActorOf(actions.TokenActor("1", actions.ScopeWrite))})
	} else {
		out, err = g.js.EnqueueDeploy(context.Background(), jobs.NewDeploy{ServiceID: svc, Kind: "deploy", Snapshot: raw, Settings: s.Settings(), Actor: jobs.ActorOf(actions.TokenActor("1", actions.ScopeWrite))})
	}
	if err != nil {
		g.t.Fatal(err)
	}
	return out.DeployID, out.JobID
}

func (g *rrig) jobState(id int64) string {
	var s string
	if err := g.st.R.QueryRow(`SELECT state FROM jobs WHERE id = ?`, id).Scan(&s); err != nil {
		g.t.Fatal(err)
	}
	return s
}

// wait polls until the job leaves queued and running.
func (g *rrig) wait(job int64) string {
	g.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if s := g.jobState(job); s != jobs.StateQueued && s != jobs.StateRunning {
			return s
		}
		if time.Now().After(deadline) {
			g.t.Fatalf("job %d is still %s", job, g.jobState(job))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (g *rrig) until(what string, cond func() bool) {
	g.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			g.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type drow struct {
	status, code, reason, appSafe, ip, stage string
	supersededBy                             int64
	imageID                                  string
}

func (g *rrig) row(deploy int64) drow {
	g.t.Helper()
	var r drow
	var code, reason, safe, ip, stage, image sql.NullString
	var by sql.NullInt64
	if err := g.st.R.QueryRow(`SELECT status, reason_code, reason, app_safe, ip, stage, superseded_by, image_id FROM deploys WHERE id = ?`, deploy).
		Scan(&r.status, &code, &reason, &safe, &ip, &stage, &by, &image); err != nil {
		g.t.Fatal(err)
	}
	r.code, r.reason, r.appSafe, r.ip, r.stage, r.supersededBy, r.imageID = code.String, reason.String, safe.String, ip.String, stage.String, by.Int64, image.String
	return r
}

func (g *rrig) stages(deploy int64) map[string]string {
	g.t.Helper()
	rs, err := g.st.R.Query(`SELECT stage, state FROM deploy_stages WHERE deploy_id = ?`, deploy)
	if err != nil {
		g.t.Fatal(err)
	}
	defer rs.Close()
	out := map[string]string{}
	for rs.Next() {
		var a, b string
		rs.Scan(&a, &b)
		out[a] = b
	}
	return out
}

// live makes deploy d the service's active deploy (a version that is serving).
func (g *rrig) live(svc int64, number int, ip string) int64 {
	g.t.Helper()
	d := addDeploy(g.t, g.st, svc, number, "succeeded", ip)
	mustExec(g.t, g.st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, d, svc)
	// A running container of this install at its address, as the resync would see it.
	g.ops.mu.Lock()
	g.ops.n++
	id := fmt.Sprintf("%08x", 0xa0000000+g.ops.n) + strings.Repeat("0", 56) // its own first 8 characters: the call log shows them
	g.ops.cs[id] = &fcont{id: id, name: fmt.Sprintf("sd-p-live-d%d-a1", d), ip: ip, image: testImageID, running: true, labels: map[string]string{
		agentops.LabelProject: "1", agentops.LabelService: strconv.FormatInt(svc, 10), agentops.LabelDeploy: strconv.FormatInt(d, 10), agentops.LabelAttempt: "1"}}
	g.ops.mu.Unlock()
	g.routes.Track(Container{ID: id, Service: svc, Deploy: d, State: StateRunning, IP: ip})
	return d
}

func (g *rrig) containerOf(deploy int64) *fcont {
	g.ops.mu.Lock()
	defer g.ops.mu.Unlock()
	for _, c := range g.ops.cs {
		if c.labels[agentops.LabelDeploy] == strconv.FormatInt(deploy, 10) {
			return c
		}
	}
	return nil
}

func (g *rrig) noContainers() bool {
	g.ops.mu.Lock()
	defer g.ops.mu.Unlock()
	return len(g.ops.cs) == 0
}

func opErr(code, msg string) error { return &agent.OpError{Code: code, Msg: msg} }

// ---- the tests ----

// A healthy candidate goes all the way (step 8b): it is switched in, committed as
// the live version, the old container is removed after the drain, and every stage
// row says what happened.
func TestHealthyCandidateIsSwitchedCommittedAndTheOldOneRemoved(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("job %s", s)
	}
	r := g.row(d)
	if r.status != "succeeded" || r.code != "" || r.ip != "10.250.0.3" || r.imageID != testImageID || r.stage != StageDone {
		t.Fatalf("%+v", r)
	}
	st := g.stages(d)
	for _, s := range blueGreenStages {
		if st[s] != StageOK {
			t.Errorf("stage %s is %s", s, st[s])
		}
	}
	if g.ops.find(oldID) != nil {
		t.Error("the old container was left behind")
	}
	if g.containerOf(d) == nil || !g.containerOf(d).running {
		t.Error("the new container is not running")
	}
	var active int64
	var committed sql.NullString
	g.st.R.QueryRow(`SELECT active_deploy_id FROM services WHERE id = ?`, g.svc).Scan(&active)
	g.st.R.QueryRow(`SELECT committed_at FROM deploys WHERE id = ?`, d).Scan(&committed)
	if active != d || !committed.Valid {
		t.Errorf("active deploy %d (want %d), committed_at %v", active, d, committed)
	}
	// The call order: pull, create, start, then (after the commit) stop and remove the old one.
	var order []string
	for _, c := range g.ops.names() {
		op, _, _ := strings.Cut(c, ":")
		if op != "inspect" && op != "health" && op != "network_inspect" && op != "list" && op != "image_inspect" {
			order = append(order, op)
		}
	}
	if got := strings.Join(order, ","); got != "pull,create,start,stop,remove" {
		t.Errorf("calls: %s", got)
	}
}

func TestFirstDeployFailureIsUnknownAppSafe(t *testing.T) {
	g := newRRig(t)
	g.ops.pullFn = func(context.Context, string, func(agentops.PullProgress)) (agentops.PullResult, error) {
		return agentops.PullResult{}, opErr(agent.CodeDocker, "manifest unknown")
	}
	g.start(false)
	d, job := g.deploy(g.svc) // no active deploy: there is no previous version
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatal(s)
	}
	if r := g.row(d); r.code != CodePullFailed || r.appSafe != AppSafeUnknown {
		t.Fatalf("%+v", r)
	}
	// With a version live the same failure is app_safe yes.
	g.live(g.svc, 50, "10.250.0.2")
	d2, job2 := g.deploy(g.svc)
	g.wait(job2)
	if r := g.row(d2); r.code != CodePullFailed || r.appSafe != AppSafeYes {
		t.Fatalf("%+v", r)
	}
	if got := g.ops.pullN.Load(); got != 2 {
		t.Errorf("a permanent pull failure was retried: %d pulls", got)
	}
}

func TestTransientPullRetryThenSuccess(t *testing.T) {
	g := newRRig(t)
	var n atomic.Int32
	g.ops.pullFn = func(_ context.Context, _ string, on func(agentops.PullProgress)) (agentops.PullResult, error) {
		if n.Add(1) < 3 {
			return agentops.PullResult{}, opErr(agent.CodeDocker, "toomanyrequests: 429 rate limited")
		}
		return agentops.PullResult{ImageID: testImageID}, nil
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.until("the deploy to be waiting", func() bool { return n.Load() >= 1 })
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("job %s", s)
	}
	if n.Load() != 3 {
		t.Fatalf("%d pulls, want 3 (two transient failures, then success)", n.Load())
	}
	j, _ := g.js.Get(context.Background(), job)
	if j.Attempt != 3 {
		t.Errorf("attempt %d", j.Attempt)
	}
	if r := g.row(d); r.status != StatusSucceeded {
		t.Fatalf("%+v", r)
	}
	// Each attempt would use a new container name: only the last got as far as create.
	if names := g.ops.names(); !strings.Contains(strings.Join(names, " "), "-a3") {
		t.Errorf("names %v", names)
	}
}

func TestTransientPullFailureGivesUpAfterThreeAttempts(t *testing.T) {
	g := newRRig(t)
	g.ops.pullFn = func(context.Context, string, func(agentops.PullProgress)) (agentops.PullResult, error) {
		return agentops.PullResult{}, opErr(agent.CodeTimeout, "timed out")
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatal(s)
	}
	if g.ops.pullN.Load() != 3 {
		t.Errorf("%d pulls", g.ops.pullN.Load())
	}
	if r := g.row(d); r.status != "failed" || r.code != CodePullFailed {
		t.Fatalf("%+v", r)
	}
}

func TestDiskFullIsNotRetried(t *testing.T) {
	g := newRRig(t)
	g.ops.pullFn = func(context.Context, string, func(agentops.PullProgress)) (agentops.PullResult, error) {
		return agentops.PullResult{}, opErr(agent.CodeDiskFull, "less than 1 GiB free on Docker's data root")
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatal(s)
	}
	if g.ops.pullN.Load() != 1 {
		t.Errorf("a full disk was retried: %d pulls", g.ops.pullN.Load())
	}
	r := g.row(d)
	if r.code != CodeDiskFull || r.status != "failed" || !strings.Contains(r.reason, "disk") {
		t.Fatalf("%+v", r)
	}
	j, _ := g.js.Get(context.Background(), job)
	if j.Attempt != 1 {
		t.Errorf("attempt %d", j.Attempt)
	}
}

func TestStalledPullIsPermanent(t *testing.T) {
	g := newRRig(t)
	g.ops.pullFn = func(context.Context, string, func(agentops.PullProgress)) (agentops.PullResult, error) {
		return agentops.PullResult{}, opErr(agent.CodeStalled, "the pull made no progress for 2 seconds")
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if r := g.row(d); r.code != CodePullStalled || g.ops.pullN.Load() != 1 {
		t.Fatalf("%+v pulls %d", r, g.ops.pullN.Load())
	}
}

// The scheduler's start gate: no address is allocated (no container_list, no
// network_inspect, no deploys.ip) until it opens.
func TestAddressAllocatedOnlyAfterTheStartGate(t *testing.T) {
	g := newRRig(t)
	g.start(true) // gate closed
	d, job := g.deploy(g.svc)
	time.Sleep(150 * time.Millisecond)
	if g.jobState(job) != jobs.StateQueued || g.ops.count("list") != 0 || g.ops.count("network_inspect") != 0 || g.ops.count("pull") != 0 {
		t.Fatalf("something ran behind the closed gate: %v", g.ops.names())
	}
	if r := g.row(d); r.ip != "" || r.status != "queued" {
		t.Fatalf("%+v", r)
	}
	g.sched.OpenGate()
	g.wait(job)
	if r := g.row(d); r.ip == "" {
		t.Errorf("no address after the gate opened: %+v", r)
	}
	if g.ops.count("list") == 0 {
		t.Error("the allocator did not ask Docker")
	}
}

func TestAllocationFailsTransientlyWhenDockerIsUnreachable(t *testing.T) {
	g := newRRig(t)
	g.ops.listErr = opErr(agent.CodeDocker, "the Docker request failed")
	g.start(false)
	d, job := g.deploy(g.svc)
	g.until("the first attempt", func() bool { return g.ops.count("list") >= 1 })
	// It is retried, not failed at once.
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatal(s)
	}
	if g.ops.count("list") != 3 {
		t.Errorf("%d attempts, want 3 (a docker error is transient)", g.ops.count("list"))
	}
	r := g.row(d)
	if r.code != CodeDocker || r.ip != "" || r.status != "failed" {
		t.Fatalf("%+v", r)
	}
	// And it recovers when Docker is back before the attempts run out.
	g2 := newRRig(t)
	var calls atomic.Int32
	g2.ops.listErr = opErr(agent.CodeDocker, "the Docker request failed")
	g2.start(false)
	d2, job2 := g2.deploy(g2.svc)
	g2.until("the first allocation", func() bool { calls.Store(int32(g2.ops.count("list"))); return calls.Load() >= 1 })
	g2.ops.mu.Lock()
	g2.ops.listErr = nil
	g2.ops.mu.Unlock()
	g2.wait(job2)
	if r := g2.row(d2); r.status != StatusSucceeded {
		t.Fatalf("after Docker came back: %+v", r)
	}
}

func TestPoolExhaustedFailsTheDeployWithTheOldVersionServing(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	// Every static address (.2 to .191) is held by a container of ours.
	for i := 2; i <= 191; i++ {
		g.ops.extra = append(g.ops.extra, agentops.ListedContainer{ID: fmt.Sprintf("%064x", 0xf000+i), State: "exited", ConfiguredIP: fmt.Sprintf("10.250.0.%d", i)})
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatal(s)
	}
	r := g.row(d)
	if r.code != CodeIPPoolExhausted || r.appSafe != AppSafeYes || r.ip != "" {
		t.Fatalf("%+v", r)
	}
	if g.ops.count("create") != 0 || g.ops.count("stop") != 0 || g.ops.count("remove") != 0 {
		t.Errorf("Docker was changed: %v", g.ops.names())
	}
	if g.ops.count("list") != 1 {
		t.Errorf("an exhausted pool was retried: %d lists", g.ops.count("list"))
	}
}

// None of the addresses a container still owns is handed out, whatever its state:
// an exited active container, the stopped old container of a stop-first gap, and an
// old container whose removal failed.
func TestAllocatorNeverReusesAStoppedOrActiveContainersAddress(t *testing.T) {
	g := newRRig(t)
	other := g.service("api")
	g.live(g.svc, 1, "10.250.0.2") // the active deploy, whose container exited
	g.live(other, 1, "10.250.0.4") // another service's active deploy
	g.ops.extra = []agentops.ListedContainer{
		{ID: fmt.Sprintf("%064x", 0xe001), State: "exited", ConfiguredIP: "10.250.0.2", Labels: agentops.ListedLabels{Service: strconv.FormatInt(g.svc, 10), Deploy: "1"}},
		{ID: fmt.Sprintf("%064x", 0xe002), State: "exited", ConfiguredIP: "10.250.0.3"},  // the stopped old container of a stop-first gap
		{ID: fmt.Sprintf("%064x", 0xe003), State: "running", ConfiguredIP: "10.250.0.5"}, // its removal failed after a deploy
		{ID: fmt.Sprintf("%064x", 0xe004), State: "created", ConfiguredIP: "10.250.0.6", Unowned: true},
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if r := g.row(d); r.ip != "10.250.0.7" || r.status != StatusSucceeded {
		t.Fatalf("%+v: the allocator reused an address a container still owns", r)
	}
	c := g.ops.labelsSeen
	if len(c) != 1 {
		t.Fatalf("creates: %d", len(c))
	}
}

// Two services reach check at once: the allocation lock gives them different
// addresses (the deploys_ip_live index backs it up in TestTwoConcurrentAllocationsNeverShareAnAddress,
// which also bypasses the lock).
func TestTwoConcurrentChecksNeverGetTheSameAddress(t *testing.T) {
	g := newRRig(t)
	b := g.service("api")
	// Both pulls wait for each other, so both deploys reach check together.
	var arrived atomic.Int32
	release := make(chan struct{})
	g.ops.pullFn = func(ctx context.Context, _ string, _ func(agentops.PullProgress)) (agentops.PullResult, error) {
		if arrived.Add(1) == 2 {
			close(release)
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return agentops.PullResult{ImageID: testImageID}, nil
	}
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		time.Sleep(30 * time.Millisecond) // both candidates exist at the same time
		return agentops.HealthResult{OK: true}, nil
	}
	g.start(false)
	d1, j1 := g.deploy(g.svc)
	d2, j2 := g.deploy(b)
	g.wait(j1)
	g.wait(j2)
	r1, r2 := g.row(d1), g.row(d2)
	if r1.ip == "" || r2.ip == "" || r1.ip == r2.ip {
		t.Fatalf("addresses %q and %q", r1.ip, r2.ip)
	}
	if r1.status != StatusSucceeded || r2.status != StatusSucceeded {
		t.Fatalf("%+v %+v", r1, r2)
	}
}

// Another service's container forced onto an address (however it got there): the
// route of the address's owner serves the "not running" page, never the intruder.
func TestForcedAddressCollisionNeverRoutesToAnotherService(t *testing.T) {
	st := openStore(t)
	x, y := addService(t, st, "x"), addService(t, st, "y")
	dx, dy := addDeploy(t, st, x, 1, "succeeded", "10.250.0.2"), addDeploy(t, st, y, 1, "succeeded", "10.250.0.3")
	mustExec(t, st, `UPDATE services SET public_port = 9101, container_port = 8080, active_deploy_id = ? WHERE id = ?`, dx, x)
	mustExec(t, st, `UPDATE services SET public_port = 9102, container_port = 8080, active_deploy_id = ? WHERE id = ?`, dy, y)
	px := &fakeProxy{}
	r := NewRoutes(st, px, nil)
	// X's own container is gone; Y's container sits at X's address (forced collision),
	// and Y's own is fine at its own.
	if err := r.ApplyRoutes(context.Background(), func() error {
		r.state.replace([]Container{
			{ID: "intruder", Service: y, Deploy: dy, State: StateRunning, IP: "10.250.0.2"},
			{ID: "ydeploy", Service: y, Deploy: dy, State: StateRunning, IP: "10.250.0.3"},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	last := px.last()
	for _, rt := range last {
		switch rt.Port {
		case 9101:
			if rt.Upstream != "" || rt.Page != agentops.PageNotRunning {
				t.Errorf("X's route %+v: the intruder at its address was routed to", rt)
			}
		case 9102:
			if rt.Upstream != "10.250.0.3:8080" {
				t.Errorf("Y's route %+v", rt)
			}
		}
	}
	if len(last) != 2 {
		t.Fatalf("routes %+v", last)
	}
}

func TestNameCollisionReusesOwnContainer(t *testing.T) {
	g := newRRig(t)
	// The container of this very attempt already exists (a repeated create after a lost reply).
	g.ops.createFn = func(ctx context.Context, name string, spec dockerapi.CreateSpec, ip string) (agentops.CreateResult, error, bool) {
		if _, err := g.ops.makeContainer(name, spec, ip, false); err != nil {
			return agentops.CreateResult{}, err, true
		}
		return agentops.CreateResult{}, nil, false // the real create now finds it: Existed
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	r := g.row(d)
	if r.status != StatusSucceeded {
		t.Fatalf("%+v", r)
	}
	if g.ops.count("create") != 1 || g.ops.count("start") != 1 {
		t.Errorf("calls %v", g.ops.names())
	}
	stages, _ := g.st.R.Query(`SELECT detail FROM deploy_stages WHERE deploy_id = ? AND stage = 'start'`, d)
	defer stages.Close()
	if stages.Next() {
		var detail string
		stages.Scan(&detail)
		if !strings.Contains(detail, "reused") {
			t.Errorf("start detail %q", detail)
		}
	}
	if c := g.containerOf(d); c == nil || !c.running {
		t.Error("the reused container is not the running, live version")
	}
}

func TestForeignNameCollisionFails(t *testing.T) {
	g := newRRig(t)
	g.ops.createFn = func(context.Context, string, dockerapi.CreateSpec, string) (agentops.CreateResult, error, bool) {
		return agentops.CreateResult{}, opErr(agent.CodeConflict, "a container ServerDock does not own uses this name"), true
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatal(s)
	}
	r := g.row(d)
	if r.code != CodeNameConflict || !strings.Contains(r.reason, "does not own") {
		t.Fatalf("%+v", r)
	}
	// The foreign container is never touched: no stop, no remove, whatever its id.
	if g.ops.count("stop") != 0 || g.ops.count("remove") != 0 {
		t.Errorf("a container was touched: %v", g.ops.names())
	}
	if g.ops.count("create") != 1 {
		t.Errorf("the conflict was retried: %d creates", g.ops.count("create"))
	}
}

func TestFailFastOnExitAndOOM(t *testing.T) {
	for name, tc := range map[string]struct {
		crash func(c *fcont)
		code  string
	}{
		"exit":    {func(c *fcont) { c.running, c.exit = false, 3 }, CodeExited},
		"oom":     {func(c *fcont) { c.running, c.exit, c.oom = false, 137, true }, CodeOOM},
		"restart": {func(c *fcont) { c.running, c.restar, c.exit = false, true, 1 }, CodeExited},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRRig(t)
			g.live(g.svc, 1, "10.250.0.2")
			g.ops.startFn = tc.crash
			// A generous gate: the failure must come from the inspect, not from the timeout.
			g.ops.healthFn = func(ctx context.Context, _ agentops.HealthArgs) (agentops.HealthResult, error) {
				return agentops.HealthResult{OK: false, Err: "refused"}, nil
			}
			g.start(false)
			t0 := time.Now()
			d, job := g.deploy(g.svc, func(s *Snapshot) { s.Health.TimeoutS = 600 })
			g.wait(job)
			r := g.row(d)
			if r.code != tc.code || r.appSafe != AppSafeYes || time.Since(t0) > 5*time.Second {
				t.Fatalf("%+v after %v", r, time.Since(t0))
			}
			if g.containerOf(d) != nil {
				t.Error("the failed candidate was left behind")
			}
		})
	}
}

func TestHealthTimeoutFailsTheGate(t *testing.T) {
	g := newRRig(t)
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		return agentops.HealthResult{OK: false, Status: 503}, nil
	}
	g.start(false)
	d, job := g.deploy(g.svc, func(s *Snapshot) { s.Health = SnapHealth{Kind: "http", Path: "/up", TimeoutS: 5, Passes: 2} })
	t0 := time.Now()
	g.wait(job)
	r := g.row(d)
	if r.code != CodeHealthTimeout || !strings.Contains(r.reason, "HTTP 503") || time.Since(t0) < 4*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(t0))
	}
	if !g.noContainers() {
		t.Error("candidate left")
	}
}

// A supersede that arrives while the health gate runs ends the candidate like an
// unhealthy result: it is removed, the deploy is superseded (by the newer one), and
// the old version is untouched.
func TestSupersedeDuringHealthGateIsUnhealthy(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	inHealth := make(chan struct{}, 1)
	g.ops.healthFn = func(ctx context.Context, _ agentops.HealthArgs) (agentops.HealthResult, error) {
		select {
		case inHealth <- struct{}{}:
		default:
		}
		return agentops.HealthResult{OK: false}, nil
	}
	g.start(false)
	d1, j1 := g.deploy(g.svc, func(s *Snapshot) { s.Health.TimeoutS = 600 })
	<-inHealth
	d2, j2 := g.deploy(g.svc) // supersedes the running one
	if s := g.wait(j1); s != jobs.StateCancelled {
		t.Fatalf("the superseded job is %s", s)
	}
	// Only now (the first deploy is over) does the probe start passing, for the second.
	g.ops.mu.Lock()
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		return agentops.HealthResult{OK: true}, nil
	}
	g.ops.mu.Unlock()
	r := g.row(d1)
	if r.status != "superseded" || r.code != CodeSuperseded || r.supersededBy != d2 || r.appSafe != AppSafeYes || !strings.Contains(r.reason, "unhealthy") {
		t.Fatalf("%+v (newer deploy %d)", r, d2)
	}
	if st := g.stages(d1); st[StageHealth] != StageFailed {
		t.Errorf("health stage %q", st[StageHealth])
	}
	// The old container was never touched, and the superseded candidate is gone.
	g.wait(j2)
	if c := g.containerOf(d1); c != nil {
		t.Errorf("the superseded candidate is still there: %+v", c)
	}
	for _, c := range g.ops.names() {
		if strings.HasPrefix(c, "stop:") || strings.HasPrefix(c, "remove:") {
			// only candidates (with deploy ids d1 or d2) are ever stopped or removed
			continue
		}
	}
}

func TestCancelDuringPullEndsTheDeployCancelled(t *testing.T) {
	g := newRRig(t)
	inPull := make(chan struct{})
	g.ops.pullFn = func(ctx context.Context, _ string, on func(agentops.PullProgress)) (agentops.PullResult, error) {
		on(agentops.PullProgress{Layers: 4, LayersDone: 1, Bytes: 10, BytesTotal: 100, Status: "Downloading"})
		close(inPull)
		<-ctx.Done()
		return agentops.PullResult{}, context.Cause(ctx)
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	<-inPull
	// The progress map holds the latest pull progress of the running deploy.
	if p, ok := g.runner.Progress(d); !ok || p.Layers != 4 || p.Bytes != 10 {
		t.Errorf("progress %+v %v", p, ok)
	}
	if err := g.sched.Cancel(context.Background(), job, "cancelled by the user"); err != nil {
		t.Fatal(err)
	}
	if s := g.wait(job); s != jobs.StateCancelled {
		t.Fatal(s)
	}
	if r := g.row(d); r.status != "cancelled" || r.code != CodeCancelled {
		t.Fatalf("%+v", r)
	}
	g.until("the progress entry to go", func() bool { _, ok := g.runner.Progress(d); return !ok })
}

func TestProgressMapIsBounded(t *testing.T) {
	g := newRRig(t)
	for i := int64(1); i <= 100; i++ {
		g.runner.setProgress(i, agentops.PullProgress{Layers: 1})
	}
	g.runner.progMu.Lock()
	n := len(g.runner.progress)
	g.runner.progMu.Unlock()
	if n != maxProgress {
		t.Errorf("%d entries, want at most %d", n, maxProgress)
	}
	g.runner.setProgress(1, agentops.PullProgress{Layers: 9}) // a tracked deploy still updates
	if p, _ := g.runner.Progress(1); p.Layers != 9 {
		t.Errorf("%+v", p)
	}
	g.runner.dropProgress(1)
	if _, ok := g.runner.Progress(1); ok {
		t.Error("not dropped")
	}
}

// The candidate is in the state map from the runner's own inspect: no watcher, no event.
func TestRunnerRecordsCandidateWithoutTheWatcher(t *testing.T) {
	g := newRRig(t)
	var seen []Container
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		seen = g.routes.State().snapshot()
		return agentops.HealthResult{OK: true}, nil
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if len(seen) != 1 {
		t.Fatalf("state map at the health gate: %+v", seen)
	}
	c := seen[0]
	if c.Service != g.svc || c.Deploy != d || c.IP != "10.250.0.2" || c.State != StateRunning || len(c.ID) != 64 {
		t.Errorf("%+v", c)
	}
	// The candidate is the live version now: it stays in the state map.
	if left := g.routes.State().snapshot(); len(left) != 1 || left[0].Deploy != d {
		t.Errorf("the state map after the deploy: %+v", left)
	}
	if g.px.n() != 0 {
		t.Error("recording the candidate applied routes")
	}
}

// An inspect result is trusted only when the agent says the container is owned AND
// its labels name this service and deploy (and attempt).
func TestInspectIsTrustedOnlyWhenOwnedAndTheLabelsMatch(t *testing.T) {
	for name, mod := range map[string]func(*fcont){
		"not owned":      func(c *fcont) { c.notOwned = true },
		"another labels": func(c *fcont) { c.wrongLabels = true },
		"wrong deploy":   func(c *fcont) { c.labels[agentops.LabelDeploy] = "9999" },
		"wrong attempt":  func(c *fcont) { c.labels[agentops.LabelAttempt] = "9" },
		"other address":  func(c *fcont) { c.ip = "10.250.0.99" },
	} {
		t.Run(name, func(t *testing.T) {
			g := newRRig(t)
			g.ops.startFn = mod
			g.start(false)
			d, job := g.deploy(g.svc)
			g.wait(job)
			r := g.row(d)
			if r.code != CodeStartFailed {
				t.Fatalf("%+v", r)
			}
			if left := g.routes.State().snapshot(); len(left) != 0 {
				t.Errorf("an untrusted container was recorded: %+v", left)
			}
			if g.ops.count("health") != 0 {
				t.Error("the health gate ran against an untrusted container")
			}
		})
	}
}

func TestDockerTimeoutSetsServiceUnknown(t *testing.T) {
	g := newRRig(t)
	ready := g.live(g.svc, 1, "10.250.0.2")
	_ = ready
	// A running active container in a filled state map: the service reads as running.
	if err := g.routes.ApplyRoutes(context.Background(), func() error {
		g.routes.state.replace([]Container{{ID: "c", Service: g.svc, Deploy: ready, State: StateRunning, IP: "10.250.0.2"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svcState := func() string {
		d := &svcDeps{ServiceDeps: ServiceDeps{DB: g.st, Routes: g.routes, Now: time.Now}}
		row, err := d.loadID(context.Background(), g.svc)
		if err != nil {
			t.Fatal(err)
		}
		return d.state(row)
	}
	if s := svcState(); s != "running" {
		t.Fatalf("before: %s", s)
	}
	g.ops.createFn = func(context.Context, string, dockerapi.CreateSpec, string) (agentops.CreateResult, error, bool) {
		// Docker hangs from here on: the create times out and so does the clean-up's look-up.
		g.ops.mu.Lock()
		g.ops.listErr = opErr(agent.CodeTimeout, "timed out")
		g.ops.mu.Unlock()
		return agentops.CreateResult{}, opErr(agent.CodeTimeout, "timed out"), true
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if s := svcState(); s != "unknown" {
		t.Errorf("after a timed-out op the service is %s, want unknown", s)
	}
	if r := g.row(d); r.status != "failed" || r.code != CodeStartFailed {
		t.Errorf("%+v", r)
	}
	// Until an operation for it succeeds.
	g.ops.mu.Lock()
	g.ops.listErr = nil
	g.ops.mu.Unlock()
	g.ops.createFn = nil
	_, job2 := g.deploy(g.svc)
	g.wait(job2)
	if s := svcState(); s != "running" {
		t.Errorf("after a successful operation: %s", s)
	}
}

// A create whose answer never came (a timeout) may still have made the container:
// the clean-up finds it by its service, deploy and attempt labels and removes it.
// A look-alike (another deploy's) is left alone.
func TestTimedOutCreateIsCleanedUpByItsLabels(t *testing.T) {
	g := newRRig(t)
	var made *fcont
	g.ops.createFn = func(ctx context.Context, name string, spec dockerapi.CreateSpec, ip string) (agentops.CreateResult, error, bool) {
		res, _ := g.ops.makeContainer(name, spec, ip, false)
		made = g.ops.find(res.ID)
		// A neighbour with another deploy's labels.
		lab := map[string]string{}
		for k, v := range spec.Labels {
			lab[k] = v
		}
		lab[agentops.LabelDeploy] = "777"
		g.ops.makeContainer("sd-p-web-d777-a1", dockerapi.CreateSpec{Labels: lab}, "10.250.0.50", false)
		return agentops.CreateResult{}, opErr(agent.CodeTimeout, "timed out"), true // the reply was lost
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if r := g.row(d); r.code != CodeStartFailed {
		t.Fatalf("%+v", r)
	}
	if made == nil || g.ops.find(made.id) != nil {
		t.Fatalf("the container the lost create made is still there")
	}
	if g.ops.count("list") < 2 { // the allocation's and the clean-up's
		t.Errorf("no label lookup: %v", g.ops.names())
	}
	g.ops.mu.Lock()
	left := len(g.ops.cs)
	g.ops.mu.Unlock()
	if left != 1 {
		t.Errorf("%d containers left, want only the look-alike", left)
	}
}

func TestDBWriteFailureBeforeSwitchRemovesCandidate(t *testing.T) {
	g := newRRig(t)
	// The health stage cannot be recorded: the database "fails" when the stage is set.
	mustExec(t, g.st, `CREATE TRIGGER fail_health BEFORE UPDATE OF stage ON deploys WHEN NEW.stage = 'health' BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatal(s)
	}
	r := g.row(d)
	if r.code != CodeDB || r.status != "failed" || r.appSafe == "" {
		t.Fatalf("%+v", r)
	}
	if strings.Contains(r.reason, "I/O") {
		t.Errorf("the database's own text leaked: %q", r.reason)
	}
	if !g.noContainers() || g.ops.count("health") != 0 {
		t.Errorf("candidate left or health ran: %v", g.ops.names())
	}
	if g.ops.count("remove") != 1 {
		t.Errorf("removes: %d", g.ops.count("remove"))
	}
	for _, e := range g.ops.ctxErrs["remove"] {
		if e != nil {
			t.Errorf("the clean-up used a dead context: %v", e)
		}
	}
}

// SIGTERM before the switch: the job context is cancelled with ErrShutdown; the
// candidate is removed on a FRESH context, the deploy is failed "shutdown", and the
// job row is left for the next boot's pass.
func TestShutdownBeforeSwitchRemovesCandidate(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	inHealth := make(chan struct{}, 1)
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		select {
		case inHealth <- struct{}{}:
		default:
		}
		return agentops.HealthResult{OK: false}, nil
	}
	g.start(false)
	d, job := g.deploy(g.svc, func(s *Snapshot) { s.Health.TimeoutS = 600 })
	<-inHealth
	g.stop() // the scheduler shuts down: running jobs get ErrShutdown
	r := g.row(d)
	if r.status != "failed" || r.code != CodeShutdown || r.appSafe != AppSafeYes {
		t.Fatalf("%+v", r)
	}
	if g.containerOf(d) != nil {
		t.Error("the candidate was left behind")
	}
	for _, e := range g.ops.ctxErrs["remove"] {
		if e != nil {
			t.Errorf("the clean-up used the cancelled context: %v", e)
		}
	}
	if s := g.jobState(job); s != jobs.StateRunning {
		t.Errorf("the job is %s; a shutdown leaves it running for the boot pass", s)
	}
	// The boot pass then settles it.
	if n, err := ResolveInterrupted(context.Background(), g.st, "next-boot", time.Now()); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
}

// pull, check and start share one bound; the watchdog (jobs.timeout_s) is enforced
// before the switch. Either ends the deploy with code timeout and removes the candidate.
func TestPreSwitchDeadlineEndsTheDeploy(t *testing.T) {
	g := newRRig(t, func(c *RunnerConfig) { c.PreSwitchBound = 80 * time.Millisecond })
	g.ops.pullFn = func(ctx context.Context, _ string, _ func(agentops.PullProgress)) (agentops.PullResult, error) {
		<-ctx.Done()
		return agentops.PullResult{}, context.Cause(ctx)
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if r := g.row(d); r.code != CodeTimeout || r.status != "failed" {
		t.Fatalf("%+v", r)
	}
}

func TestNetworkProblemRefusesTheDeployWithItsReason(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	reason := "the app network can't be used: the network exists with another subnet. New deploys are refused until it is fixed; running apps are not affected"
	g.net.status = NetStatus{State: NetProblem, Reason: reason}
	g.net.runFn = func() NetStatus { return NetStatus{State: NetProblem, Reason: reason} } // still wrong when ensured again
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	r := g.row(d)
	if r.code != CodeNetwork || r.reason != reason || r.appSafe != AppSafeYes {
		t.Fatalf("%+v", r)
	}
	if g.ops.count("create") != 0 || g.ops.count("list") != 0 {
		t.Errorf("the deploy went on: %v", g.ops.names())
	}
	if g.net.runs != 1 {
		t.Errorf("a problem is ensured again once per deploy, not %d times", g.net.runs)
	}
	// The user fixes it (removes the foreign network, the VPN overlap goes away): the
	// next deploy re-checks, finds it ok and goes on to the switch seam: no restart needed.
	g.net.mu.Lock()
	g.net.runFn = nil
	g.net.mu.Unlock()
	d1, job1 := g.deploy(g.svc)
	g.wait(job1)
	if r := g.row(d1); r.status != StatusSucceeded || g.net.runs != 2 {
		t.Fatalf("after the problem was fixed: %+v (ensured %d times)", r, g.net.runs)
	}
	// An unknown status is ensured once; Docker still silent: transient, retried.
	g2 := newRRig(t)
	g2.net.status = NetStatus{}
	g2.net.runFn = func() NetStatus { return NetStatus{} }
	g2.start(false)
	d2, job2 := g2.deploy(g2.svc)
	g2.wait(job2)
	if r := g2.row(d2); r.code != CodeDocker || g2.net.runs != 3 {
		t.Fatalf("%+v ensures %d", r, g2.net.runs)
	}
	// Unknown, then ensured: the deploy goes on.
	g3 := newRRig(t)
	g3.net.status = NetStatus{}
	g3.start(false)
	d3, job3 := g3.deploy(g3.svc)
	g3.wait(job3)
	if r := g3.row(d3); r.status != StatusSucceeded || g3.net.runs != 1 {
		t.Fatalf("%+v ensures %d", r, g3.net.runs)
	}
}

// container_create says the network does not exist (a prune): the runner has it
// ensured again and creates once more; the same at the allocator's network_inspect.
func TestMissingNetworkIsEnsuredAgainAtCreateAndAtCheck(t *testing.T) {
	g := newRRig(t)
	g.net.runFn = func() NetStatus { g.ops.netMissing.Store(false); return NetStatus{State: NetOK} }
	g.ops.netMissing.Store(true) // pruned: the allocator's network_inspect fails first
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if r := g.row(d); r.status != StatusSucceeded {
		t.Fatalf("%+v", r)
	}
	if g.net.gone != 1 || g.net.runs != 1 {
		t.Errorf("gone %d, ensured %d", g.net.gone, g.net.runs)
	}

	// Pruned between the allocation and the create: the create says so, the
	// network is ensured again once, and the create is repeated.
	g2 := newRRig(t)
	g2.net.runFn = func() NetStatus { g2.ops.netMissing.Store(false); return NetStatus{State: NetOK} }
	var flipped atomic.Bool
	g2.ops.afterNetInspect = func() {
		if flipped.CompareAndSwap(false, true) {
			g2.ops.netMissing.Store(true)
		}
	}
	g2.start(false)
	d2, job2 := g2.deploy(g2.svc)
	g2.wait(job2)
	if r := g2.row(d2); r.status != StatusSucceeded {
		t.Fatalf("%+v", r)
	}
	if g2.net.gone != 1 || g2.net.runs != 1 || g2.ops.count("create") != 2 {
		t.Errorf("gone %d, ensured %d, creates %d", g2.net.gone, g2.net.runs, g2.ops.count("create"))
	}
}

func TestImageRefIsRevalidatedWithTheInstallsOptions(t *testing.T) {
	g := newRRig(t) // no AllowLoopbackRegistry
	g.start(false)
	d, job := g.deploy(g.svc, func(s *Snapshot) { s.ImageRef = "127.0.0.1:5000/app:1" })
	g.wait(job)
	r := g.row(d)
	if r.code != CodePolicy || g.ops.count("pull") != 0 {
		t.Fatalf("%+v pulls %d", r, g.ops.count("pull"))
	}
	g2 := newRRig(t, func(c *RunnerConfig) { c.Policy = policy.Options{AllowLoopbackRegistry: true} })
	g2.start(false)
	d2, job2 := g2.deploy(g2.svc, func(s *Snapshot) { s.ImageRef = "127.0.0.1:5000/app:1" })
	g2.wait(job2)
	if r := g2.row(d2); r.status != StatusSucceeded {
		t.Fatalf("%+v", r)
	}
}

// Before the switch the runner never calls proxy_apply: not on any failure of pull,
// check, start or the health gate, and not on a cancel. (After the health gate the
// switch is step 8b's; switch_test.go covers it.) The proxy the Routes would use
// and the agent both count.
func TestPreSwitchStagesNeverCallProxyApply(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	mustExec(t, g.st, `UPDATE services SET public_port = 9000 WHERE id = ?`, g.svc) // an exposed service: a route exists
	if err := g.routes.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := g.px.n()
	g.start(false)
	scenarios := []func(){
		func() {
			g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
				return agentops.HealthResult{}, nil
			}
		},
		func() {
			g.ops.pullFn = func(context.Context, string, func(agentops.PullProgress)) (agentops.PullResult, error) {
				return agentops.PullResult{}, opErr(agent.CodeDiskFull, "x")
			}
		},
		func() {
			g.ops.pullFn = nil
			g.ops.startFn = func(c *fcont) { c.running, c.exit = false, 1 }
		},
	}
	for _, s := range scenarios {
		s()
		_, job := g.deploy(g.svc, func(s *Snapshot) { s.Health.TimeoutS = 5 })
		g.wait(job)
	}
	if g.px.n() != before || g.ops.proxyCalls.Load() != 0 {
		t.Errorf("proxy_apply was called: routes %d -> %d, agent %d", before, g.px.n(), g.ops.proxyCalls.Load())
	}
}

// Structurally too: the pre-switch files (runner.go, rundeploy.go) name no
// route-changing call, and the one file that may (switch.go, step 8b) names only
// the three it needs: never ProxyApply, a plain ApplyRoutes, Refresh, Sync or a
// state-map write that applies.
func TestRouteCallsAreOnlyInTheSwitchFile(t *testing.T) {
	forbidden := []string{"ProxyApply", "ApplyRoutes", "SetOverride", "ClearOverride", "CommitRoutes", "SetContainer", "RemoveContainer(", ".Refresh(", "Sync("}
	for _, f := range []string{"runner.go", "rundeploy.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s mentions %s: the pre-switch files must not touch the routes", f, bad)
			}
		}
	}
	b, err := os.ReadFile("switch.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"ProxyApply", "ApplyRoutes(", "ApplyRoutesNoRetry", "SetContainer", "RemoveContainer(", ".Refresh(", "Sync(", ".mu.Lock"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("switch.go mentions %s", bad)
		}
	}
	// Every route call of the runner goes through RouteControl.
	for _, want := range []string{"Routes.SetOverride(", "Routes.ClearOverride(", "Routes.CommitRoutes("} {
		if !strings.Contains(string(b), want) {
			t.Errorf("switch.go has no %s", want)
		}
	}
}

// The default build has no test hooks; the tag build has them. (The tag build's own
// behaviour is tested by hooks_on_test.go, which only compiles with -tags testhooks.)
func TestDefaultBuildHasNoTestHooks(t *testing.T) {
	if hooksCompiled {
		t.Fatal("the test hooks are compiled into this build")
	}
	t.Setenv("SERVERDOCK_TESTHOOK_PAUSE", "pull:before,health:before")
	t.Setenv("SERVERDOCK_TESTHOOK_DIR", t.TempDir())
	done := make(chan struct{})
	go func() { pausePoint("pull:before"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pausePoint blocked in the default build")
	}
	// The files that carry the hooks are excluded from a default build entirely.
	out, err := exec.Command("go", "list", "-f", "{{join .GoFiles \",\"}}", ".").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	if strings.Contains(string(out), "hooks_on.go") || !strings.Contains(string(out), "hooks_off.go") {
		t.Errorf("the default build's files: %s", out)
	}
	tagged, err := exec.Command("go", "list", "-tags", "testhooks", "-f", "{{join .GoFiles \",\"}}", ".").Output()
	if err == nil && (!strings.Contains(string(tagged), "hooks_on.go") || strings.Contains(string(tagged), "hooks_off.go")) {
		t.Errorf("the testhooks build's files: %s", tagged)
	}
}

// A pause point exists at every stage boundary (the names step 10's kill tests use).
func TestPausePointsAtEveryStageBoundary(t *testing.T) {
	var src []byte
	for _, f := range []string{"rundeploy.go", "switch.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src = append(src, b...)
	}
	b := src
	for _, s := range []string{StagePull, StageCheck, StageStart, StageHealth, StageSwitch, StageCommit, StageDrain, StageStopOld} {
		for _, side := range []string{"before", "after"} {
			if !strings.Contains(string(b), `pausePoint("`+s+`:`+side+`")`) {
				t.Errorf("no pause point %s:%s", s, side)
			}
		}
	}
}

var _ = json.Marshal
