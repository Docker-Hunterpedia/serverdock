package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// ---- the rig: the runner's rig plus a reconciler over the same fake Docker ----

// recOps is the fake Docker with an events stream.
type recOps struct {
	*fakeOps
	mu       sync.Mutex
	cur      *fstream
	streams  int
	openErr  error // the next opens fail with it while set
	onOpened func()
}

type fstream struct {
	fn  func(agentops.Event) error
	end chan error
}

func (o *recOps) Events(ctx context.Context, onReady func(), fn func(agentops.Event) error) error {
	o.mu.Lock()
	if o.openErr != nil {
		err := o.openErr
		o.mu.Unlock()
		return err
	}
	s := &fstream{fn: fn, end: make(chan error, 1)}
	o.cur = s
	o.streams++
	hook := o.onOpened
	o.mu.Unlock()
	onReady()
	if hook != nil {
		hook()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-s.end:
		return err
	}
}

func (o *recOps) emit(action, id string, svc int64) {
	o.mu.Lock()
	s := o.cur
	o.mu.Unlock()
	if s != nil {
		_ = s.fn(agentops.Event{Action: action, ID: id, Service: strconv.FormatInt(svc, 10)})
	}
}

// drop ends the current stream (Docker went away).
func (o *recOps) drop(err error) {
	o.mu.Lock()
	s := o.cur
	o.mu.Unlock()
	if s != nil {
		s.end <- err
	}
}

func (o *recOps) nStreams() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.streams
}

type recRig struct {
	*rrig
	rops   *recOps
	rec    *Reconciler
	gates  atomic.Int32
	tmu    sync.Mutex
	delays []time.Duration
	seq    orderLog
}

// orderLog records the order of calls that come from different fakes.
type orderLog struct {
	mu sync.Mutex
	l  []string
}

func (s *orderLog) add(x string) {
	s.mu.Lock()
	s.l = append(s.l, x)
	s.mu.Unlock()
}

func (s *orderLog) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.l...)
}

func (s *orderLog) index(x string) int {
	for i, v := range s.all() {
		if v == x {
			return i
		}
	}
	return -1
}

func newRecRig(t *testing.T, mod func(*recRig, *ReconcilerConfig)) *recRig {
	t.Helper()
	g := newRRig(t)
	r := &recRig{rrig: g, rops: &recOps{fakeOps: g.ops}}
	cfg := ReconcilerConfig{DB: g.st, Ops: r.rops, Routes: g.routes, Locks: g.runner.cfg.Locks, Act: g.runner.cfg.Inv, Proxy: true,
		OpenGate: func() { r.gates.Add(1) }, NewTimer: r.timer, Rand: func() float64 { return 1 }}
	if mod != nil {
		mod(r, &cfg)
	}
	r.rec = NewReconciler(cfg)
	RegisterReconcilerActions(g.reg, r.rec)
	return r
}

// timer is the fake timer: it fires at once and records its delay.
func (r *recRig) timer(d time.Duration) (<-chan time.Time, func() bool) {
	r.tmu.Lock()
	r.delays = append(r.delays, d)
	r.tmu.Unlock()
	c := make(chan time.Time, 1)
	c <- time.Now()
	return c, func() bool { return true }
}

// backoffs are the recorded delays that are not health-check intervals.
func (r *recRig) backoffs() []time.Duration {
	r.tmu.Lock()
	defer r.tmu.Unlock()
	var out []time.Duration
	for _, d := range r.delays {
		if d >= 100*time.Millisecond {
			out = append(out, d)
		}
	}
	return out
}

// run starts the watcher; it stops at the end of the test.
func (r *recRig) run() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.rec.Run(ctx); close(done) }()
	r.t.Cleanup(func() { cancel(); <-done })
}

func (r *recRig) waitUp() {
	r.t.Helper()
	r.until("the reconciler is up", r.rec.Up)
}

// put adds a container of this install to the fake Docker.
func (r *recRig) put(svc, dep int64, attempt int, ip string, running bool) string {
	r.ops.mu.Lock()
	defer r.ops.mu.Unlock()
	r.ops.n++
	id := fmt.Sprintf("%08x", 0xb0000000+r.ops.n) + strings.Repeat("0", 56)
	r.ops.cs[id] = &fcont{id: id, name: fmt.Sprintf("sd-p-w-d%d-a%d", dep, attempt), ip: ip, image: testImageID, running: running, labels: map[string]string{
		agentops.LabelProject: "1", agentops.LabelService: strconv.FormatInt(svc, 10), agentops.LabelDeploy: strconv.FormatInt(dep, 10), agentops.LabelAttempt: strconv.Itoa(attempt)}}
	return id
}

// runningC reads a container's running flag under the fake's lock.
func (r *recRig) runningC(id string) bool {
	r.ops.mu.Lock()
	defer r.ops.mu.Unlock()
	c := r.ops.cs[id]
	return c != nil && c.running
}

func (r *recRig) exists(id string) bool { return r.ops.find(id) != nil }

func (r *recRig) auditRows() int {
	var n int
	if err := r.st.R.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// activeDeploy makes a deploy the service's live one (with its container, running or not).
func (r *recRig) activeDeploy(svc int64, number int, ip string, running bool) (int64, string) {
	d := addDeploy(r.t, r.st, svc, number, "succeeded", ip)
	mustExec(r.t, r.st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, d, svc)
	return d, r.put(svc, d, 1, ip, running)
}

func (r *recRig) expose(svc int64, port int) {
	mustExec(r.t, r.st, `UPDATE services SET public_port = ? WHERE id = ?`, port, svc)
}

func (r *recRig) attention(svc int64) string {
	var s sql.NullString
	if err := r.st.R.QueryRow(`SELECT needs_attention FROM services WHERE id = ?`, svc).Scan(&s); err != nil {
		r.t.Fatal(err)
	}
	return s.String
}

func (r *recRig) resync(first bool) {
	r.t.Helper()
	if _, err := r.rec.fullResync(context.Background(), first, nil); err != nil {
		r.t.Fatal(err)
	}
}

// ---- boot pass and start gate ----

// The boot pass is DB only: it changes jobs, deploys, stages and flags and touches
// no container and no route (no Docker op before the first resync).
func TestBootPassChangesOnlyTheDB(t *testing.T) {
	g := newRecRig(t, nil)
	live := g.live(g.svc, 1, "10.250.0.2")
	_ = live
	d2, j2 := g.deploy(g.svc)
	mustExec(t, g.st, `UPDATE jobs SET state = 'running', boot_id = 'old', request_id = 'job-1-a1' WHERE id = ?`, j2)
	mustExec(t, g.st, `UPDATE deploys SET status = 'running' WHERE id = ?`, d2)
	callsBefore, proxyBefore := len(g.ops.names()), g.px.n()
	n, err := ResolveInterrupted(context.Background(), g.st, "new", time.Now(), nil)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got := len(g.ops.names()) - callsBefore; got != 0 {
		t.Errorf("the boot pass made %d Docker calls: %v", got, g.ops.names()[callsBefore:])
	}
	if g.px.n() != proxyBefore {
		t.Error("the boot pass sent routes")
	}
	if r := g.row(d2); r.status != "failed" || r.code != "interrupted" || r.appSafe != "unknown" {
		t.Errorf("%+v", r)
	}
}

// The scheduler dispatches nothing until the reconciler's first resync has finished
// (Docker down at start included): the gate is the resync, not the boot.
func TestSchedulerStartsOnlyAfterTheFirstResync(t *testing.T) {
	var g *recRig
	release := make(chan struct{})
	g = newRecRig(t, func(r *recRig, c *ReconcilerConfig) {
		c.OpenGate = func() { r.gates.Add(1); r.sched.OpenGate() }
	})
	g.start(true) // the scheduler's gate is closed
	g.rops.openErr = errors.New("docker down")
	g.ops.onCall = func(op, _ string) {
		if op == "list" {
			<-release
		}
	}
	g.live(g.svc, 1, "10.250.0.2")
	_, job := g.deploy(g.svc)
	g.run()
	time.Sleep(100 * time.Millisecond)
	if g.gates.Load() != 0 || g.jobState(job) != jobs.StateQueued {
		t.Fatalf("the gate opened while Docker was down (%d) or the job ran (%s)", g.gates.Load(), g.jobState(job))
	}
	g.rops.mu.Lock()
	g.rops.openErr = nil // Docker is back: the next attempt connects, and then blocks inside the list
	g.rops.mu.Unlock()
	g.until("a stream is open", func() bool { return g.rops.nStreams() == 1 })
	time.Sleep(100 * time.Millisecond)
	if g.gates.Load() != 0 || g.jobState(job) != jobs.StateQueued {
		t.Fatalf("the gate opened before the first list finished (%d), job %s", g.gates.Load(), g.jobState(job))
	}
	g.ops.mu.Lock()
	g.ops.onCall = nil
	g.ops.mu.Unlock()
	close(release)
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("job %s", s)
	}
	if g.gates.Load() != 1 {
		t.Errorf("the gate opened %d times", g.gates.Load())
	}
}

// ---- the watcher ----

func TestEventsStreamDropTriggersResync(t *testing.T) {
	g := newRecRig(t, nil)
	_, id := g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.run()
	g.waitUp()
	lists := g.ops.count("list")
	if lists != 1 || g.gates.Load() != 1 {
		t.Fatalf("first connect: %d lists, gate %d", lists, g.gates.Load())
	}
	// A container appears while the stream is down: only the resync can find it.
	g.rops.drop(errors.New("docker restarted"))
	g.until("the second stream", func() bool { return g.rops.nStreams() == 2 })
	g.waitUp()
	if g.ops.count("list") != 2 {
		t.Errorf("%d lists after the reconnect, want 2", g.ops.count("list"))
	}
	if c, ok := g.routes.State().Get(id); !ok || c.State != StateRunning {
		t.Errorf("state after the resync: %+v %v", c, ok)
	}
	if g.gates.Load() != 1 {
		t.Errorf("the start gate opened %d times", g.gates.Load())
	}
	if len(g.backoffs()) != 1 {
		t.Errorf("backoffs %v", g.backoffs())
	}
}

// A candidate in its health gate (the service lock is held, the job runs) survives a
// reconnect: the resync never touches a service a job holds.
func TestResyncSkipsAServiceWithARunningJob(t *testing.T) {
	g := newRecRig(t, nil)
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	d2 := addDeploy(t, g.st, g.svc, 2, "running", "10.250.0.3")
	cand := g.put(g.svc, d2, 1, "10.250.0.3", true)
	unlock, err := g.runner.cfg.Locks.Lock(context.Background(), g.svc) // what the runner holds for the whole job
	if err != nil {
		t.Fatal(err)
	}
	g.run()
	g.waitUp()
	g.rops.drop(errors.New("reconnect"))
	g.until("the second stream", func() bool { return g.rops.nStreams() == 2 })
	g.waitUp()
	if !g.exists(cand) {
		t.Fatal("the candidate of a running deploy was removed")
	}
	unlock()
	// The job is over (its deploy failed): the per-service resync after it removes it.
	mustExec(t, g.st, `UPDATE deploys SET status = 'failed', reason_code = 'health_timeout' WHERE id = ?`, d2)
	g.rec.ResyncService(g.svc)
	g.until("the orphan is removed", func() bool { return !g.exists(cand) })
}

// A reconnect while a job holds a service's lock does not wait for it: the other
// services and the routes are done at once, and the per-service resync covers the
// skipped one.
func TestResyncNeverBlocksOnABusyServiceLock(t *testing.T) {
	g := newRecRig(t, nil)
	other := g.service("other")
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.activeDeploy(other, 1, "10.250.0.4", true)
	busyOrphan := g.put(g.svc, addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3"), 1, "10.250.0.3", true)
	otherOrphan := g.put(other, addDeploy(t, g.st, other, 2, "failed", "10.250.0.5"), 1, "10.250.0.5", true)
	unlock, err := g.runner.cfg.Locks.Lock(context.Background(), g.svc)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { g.resync(true); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the resync waited for a service lock")
	}
	if !g.exists(busyOrphan) || g.exists(otherOrphan) {
		t.Errorf("busy service orphan exists=%v, other service orphan exists=%v", g.exists(busyOrphan), g.exists(otherOrphan))
	}
	if !g.routes.Ready() {
		t.Error("the routes were not loaded")
	}
	unlock()
	if err := g.rec.serviceResync(context.Background(), g.svc); err != nil {
		t.Fatal(err)
	}
	if g.exists(busyOrphan) {
		t.Error("the per-service resync after the job did not remove the orphan")
	}
}

// The watcher never takes a service lock: events are handled while a job holds one.
func TestWatcherTakesOnlyTheRouteLock(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	d, id := g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.run()
	g.waitUp()
	unlock, err := g.runner.cfg.Locks.Lock(context.Background(), g.svc)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	c := g.ops.find(id)
	g.ops.mu.Lock()
	c.running = false
	g.ops.mu.Unlock()
	g.rops.emit("die", id, g.svc)
	g.until("the event is handled while the service lock is held", func() bool {
		c, ok := g.routes.State().Get(id)
		return ok && c.State == "exited"
	})
	_ = d
	// And the code says it too: the event path names no lock but the route lock.
	af, err := parser.ParseFile(token.NewFileSet(), "reconciler.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range af.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || (fd.Name.Name != "handle" && fd.Name.Name != "drain" && fd.Name.Name != "routeErr") {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if se, ok := n.(*ast.SelectorExpr); ok && (se.Sel.Name == "Locks" || se.Sel.Name == "TryLock" || se.Sel.Name == "Lock") {
				t.Errorf("%s uses %s", fd.Name.Name, se.Sel.Name)
			}
			return true
		})
	}
}

// The deploy of a running job whose older attempt left a container: that one is an
// orphan, the current attempt is never touched.
func TestResyncRemovesAnOlderAttemptOfTheRunningDeploy(t *testing.T) {
	g := newRecRig(t, nil)
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	d2, j2 := g.deploy(g.svc)
	mustExec(t, g.st, `UPDATE jobs SET state = 'running', attempt = 2, max_attempts = 3, boot_id = 'this' WHERE id = ?`, j2)
	mustExec(t, g.st, `UPDATE deploys SET status = 'running' WHERE id = ?`, d2)
	old := g.put(g.svc, d2, 1, "10.250.0.3", true)
	cur := g.put(g.svc, d2, 2, "10.250.0.3", true)
	g.resync(true)
	if g.exists(old) || !g.exists(cur) {
		t.Fatalf("older attempt exists=%v, current attempt exists=%v", g.exists(old), g.exists(cur))
	}
}

// An event that arrives after a container was listed and inspected is looked at again
// once the list is done: the state map never keeps the stale state.
func TestEventsDuringListAreReinspected(t *testing.T) {
	g := newRecRig(t, nil)
	_, a := g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	svc2 := g.service("two")
	_, b := g.activeDeploy(svc2, 1, "10.250.0.4", true)
	var mu sync.Mutex
	var first string
	inspects := map[string]int{}
	g.ops.onCall = func(op, what string) {
		if op != "inspect" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, id := range []string{a, b} {
			if strings.HasPrefix(id, what) {
				inspects[id]++
				if first == "" {
					first = id
					return
				}
				if id != first && inspects[id] == 1 && g.rops.cur != nil {
					// The second container is being inspected: the first one dies now,
					// after its own inspect, and its event arrives during the pass.
					// (restarting, not exited: a stopped active container is started again by rule 3)
					c := g.ops.find(first)
					c.running, c.restar = false, true
					go g.rops.emit("die", first, 1)
				}
			}
		}
	}
	g.run()
	g.waitUp()
	g.until("the die event is handled", func() bool {
		mu.Lock()
		f := first
		mu.Unlock()
		c, ok := g.routes.State().Get(f)
		return ok && c.State == StateRestarting
	})
	mu.Lock()
	defer mu.Unlock()
	if inspects[first] < 2 {
		t.Errorf("the container whose event came during the list was inspected %d times", inspects[first])
	}
}

func TestDieThenStopEndsExited(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	d, id := g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.run()
	g.waitUp()
	if st := g.rec.ServiceState(g.svc, d); st != ServiceRunning {
		t.Fatalf("state %s", st)
	}
	c := g.ops.find(id)
	g.ops.mu.Lock()
	c.running, c.exit = false, 137
	g.ops.mu.Unlock()
	g.rops.emit("die", id, g.svc)
	g.rops.emit("stop", id, g.svc)
	g.until("exited", func() bool { return g.rec.ServiceState(g.svc, d) == ServiceExited })
	// The state is visible before the apply that follows it has been sent.
	g.until("the route of an exited container is the not-running page", func() bool {
		last := g.px.last()
		return len(last) == 1 && last[0].Page == agentops.PageNotRunning
	})
}

func TestDieWithNoRestartIsReportedExited(t *testing.T) {
	g := newRecRig(t, nil)
	d, id := g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.run()
	g.waitUp()
	c := g.ops.find(id)
	g.ops.mu.Lock()
	c.running, c.exit = false, 1
	g.ops.mu.Unlock()
	g.rops.emit("die", id, g.svc)
	g.until("exited", func() bool { return g.rec.ServiceState(g.svc, d) == ServiceExited })
}

// A crash loop flips the container between running and restarting; its upstream stays
// and Caddy is never sent a new list for it.
func TestRestartingContainerKeepsItsUpstream(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	d, id := g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.run()
	g.waitUp()
	n := g.px.n()
	want := []agentops.Route{{Port: 9000, Upstream: "10.250.0.2:8080"}}
	for i := 0; i < 5; i++ {
		c := g.ops.find(id)
		g.ops.mu.Lock()
		c.running, c.restar = false, true
		g.ops.mu.Unlock()
		g.rops.emit("die", id, g.svc)
		g.until("restarting", func() bool { return g.rec.ServiceState(g.svc, d) == ServiceRestarting })
		g.ops.mu.Lock()
		c.running, c.restar = true, false
		g.ops.mu.Unlock()
		g.rops.emit("start", id, g.svc)
		g.until("running", func() bool { return g.rec.ServiceState(g.svc, d) == ServiceRunning })
	}
	if got := g.px.n() - n; got != 0 {
		t.Errorf("%d route lists were sent for a crash loop", got)
	}
	if last := g.px.last(); fmt.Sprint(last) != fmt.Sprint(want) {
		t.Errorf("route %+v", last)
	}
}

// Steady state does nothing and writes nothing: no action, so no audit row.
func TestNoOpReconcileWritesNoAuditRows(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.resync(true)
	rows, stops := g.auditRows(), g.ops.count("stop")+g.ops.count("remove")+g.ops.count("start")
	for i := 0; i < 3; i++ {
		g.resync(false)
		if err := g.rec.serviceResync(context.Background(), g.svc); err != nil {
			t.Fatal(err)
		}
	}
	if g.auditRows() != rows {
		t.Errorf("%d audit rows after no-op passes, want %d", g.auditRows(), rows)
	}
	if g.ops.count("stop")+g.ops.count("remove")+g.ops.count("start") != stops {
		t.Error("a no-op pass touched a container")
	}
	// A pass that removes an orphan writes exactly its intent and its outcome, as the system.
	g.put(g.svc, addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3"), 1, "10.250.0.3", true)
	g.resync(false)
	if g.auditRows() != rows+2 {
		t.Fatalf("%d audit rows after one removal, want %d", g.auditRows(), rows+2)
	}
	var kind, id, exec, res string
	if err := g.st.R.QueryRow(`SELECT actor_kind, actor_id, COALESCE(executor, ''), result FROM audit_log WHERE action = ? AND phase = 'outcome'`, ActionRemoveContainer).Scan(&kind, &id, &exec, &res); err != nil {
		t.Fatal(err)
	}
	if kind != "system" || id != "system" || res != "ok" {
		t.Errorf("outcome: %s %s %s %s", kind, id, exec, res)
	}
}

// Only containers of this instance are touched: a look-alike (the agent lists it as
// unowned) is never stopped or removed; a container of a service that is gone is.
func TestOrphanRemovalOnlyTouchesOurInstance(t *testing.T) {
	g := newRecRig(t, nil)
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	lookalike := "ee000000" + strings.Repeat("0", 56)
	g.ops.extra = []agentops.ListedContainer{{ID: lookalike, Name: "sd-look", State: "running", Unowned: true,
		Labels: agentops.ListedLabels{Project: "1", Service: strconv.FormatInt(g.svc, 10), Deploy: "77", Attempt: "1"}}}
	gone := g.put(4242, 4243, 1, "10.250.0.9", true) // a service that does not exist
	g.resync(true)
	if g.exists(gone) {
		t.Error("the orphan of a deleted service was left")
	}
	for _, c := range g.ops.names() {
		if (strings.HasPrefix(c, "stop:") || strings.HasPrefix(c, "remove:")) && strings.HasSuffix(c, lookalike[:8]) {
			t.Errorf("the look-alike was touched: %s", c)
		}
	}
	// The grace of a deleted service is 10 s.
	var grace int
	g.ops.stopGraceFn = func(gr int) { grace = gr }
	again := g.put(4242, 4244, 1, "10.250.0.10", true)
	g.resync(false)
	if g.exists(again) || grace != 10 {
		t.Errorf("exists=%v grace=%d", g.exists(again), grace)
	}
}

// The routes are applied before any orphan is removed: an orphan's stop grace never
// delays the apps, and a removal always follows a confirmed apply.
func TestRoutesAreAppliedBeforeAnOrphanIsRemoved(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.put(g.svc, addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3"), 1, "10.250.0.3", true)
	g.px.onApply = func([]agentops.Route) { g.seq.add("apply") }
	g.ops.onCall = func(op, _ string) {
		if op == "stop" || op == "remove" {
			g.seq.add(op)
		}
	}
	g.resync(true)
	apply, stop, remove := g.seq.index("apply"), g.seq.index("stop"), g.seq.index("remove")
	if apply < 0 || stop < 0 || remove < 0 || apply > stop || stop > remove {
		t.Fatalf("order %v: want the apply, then the stop, then the removal", g.seq.all())
	}
	// With the apply failing nothing is removed.
	g2 := newRecRig(t, nil)
	g2.expose(g2.svc, 9000)
	g2.activeDeploy(g2.svc, 1, "10.250.0.2", true)
	orphan := g2.put(g2.svc, addDeploy(t, g2.st, g2.svc, 2, "failed", "10.250.0.3"), 1, "10.250.0.3", true)
	g2.px.fail = errors.New("proxy down")
	g2.resync(true)
	if !g2.exists(orphan) {
		t.Error("an orphan was removed although the apply failed")
	}
	g2.px.fail = nil
	g2.resync(false)
	if g2.exists(orphan) {
		t.Error("the orphan was not removed once the apply was confirmed")
	}
}

func TestStateUnknownWhileEventsAreDown(t *testing.T) {
	g := newRecRig(t, nil)
	d, id := g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	if st := g.rec.ServiceState(g.svc, d); st != ServiceUnknown {
		t.Fatalf("before the first resync: %s", st)
	}
	g.run()
	g.waitUp()
	if st := g.rec.ServiceState(g.svc, d); st != ServiceRunning {
		t.Fatalf("after: %s", st)
	}
	// An op that ended in a docker error: unknown until an event for the service.
	g.routes.State().MarkUnknown(g.svc)
	if st := g.rec.ServiceState(g.svc, d); st != ServiceUnknown {
		t.Fatalf("after a docker error: %s", st)
	}
	g.rops.emit("start", id, g.svc)
	g.until("running again", func() bool { return g.rec.ServiceState(g.svc, d) == ServiceRunning })
	// The stream goes down and Docker stays unreachable: unknown for as long as it is.
	g.rops.mu.Lock()
	g.rops.openErr = errors.New("down")
	g.rops.mu.Unlock()
	g.rops.drop(errors.New("gone"))
	g.until("unknown while down", func() bool { return g.rec.ServiceState(g.svc, d) == ServiceUnknown })
	g.until("a retry happened", func() bool { return len(g.backoffs()) >= 2 })
	if st := g.rec.ServiceState(g.svc, d); st != ServiceUnknown {
		t.Fatalf("still down: %s", st)
	}
	g.rops.mu.Lock()
	g.rops.openErr = nil
	g.rops.mu.Unlock()
	g.waitUp()
	if st := g.rec.ServiceState(g.svc, d); st != ServiceRunning {
		t.Fatalf("back: %s", st)
	}
	if st := g.rec.ServiceState(g.svc, 0); st != ServiceNone {
		t.Errorf("no live version: %q", st)
	}
	// A destroyed active container is missing, not running.
	delete(g.ops.cs, id)
	g.rops.emit("destroy", id, g.svc)
	g.until("missing", func() bool { return g.rec.ServiceState(g.svc, d) == ServiceMissing })
}

func TestBackoffOnlyWhileDown(t *testing.T) {
	g := newRecRig(t, func(r *recRig, c *ReconcilerConfig) { c.Rand = func() float64 { return 1 } })
	g.rops.openErr = errors.New("down")
	g.run()
	g.until("six retries", func() bool { return len(g.backoffs()) >= 7 })
	got := g.backoffs()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("delay %d is %v, want %v (all %v)", i, got[i], w, got[:len(want)])
		}
	}
	g.rops.mu.Lock()
	g.rops.openErr = nil
	g.rops.mu.Unlock()
	g.waitUp()
	n := len(g.backoffs())
	time.Sleep(150 * time.Millisecond)
	if len(g.backoffs()) != n {
		t.Error("a back-off timer was created while the stream was up")
	}
	// The next drop starts the climb again at the first step.
	g.rops.drop(errors.New("again"))
	g.until("one more", func() bool { return len(g.backoffs()) == n+1 })
	if d := g.backoffs()[n]; d != time.Second {
		t.Errorf("first delay after being up is %v", d)
	}
	// With the jitter at its lowest the delay is half the step.
	jr := newRecRig(t, func(r *recRig, c *ReconcilerConfig) { c.Rand = func() float64 { return 0 } })
	jr.rops.openErr = errors.New("down")
	jr.run()
	jr.until("a retry", func() bool { return len(jr.backoffs()) >= 2 })
	if d := jr.backoffs()[0]; d != 500*time.Millisecond {
		t.Errorf("jittered delay %v", d)
	}
}

// The reconciler's actions are system-only, listed in the threat model, and the
// system actor is the only one that can run them.
func TestReconcilerActionsAreSystemOnlyAndListed(t *testing.T) {
	g := newRecRig(t, nil)
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "security.md"))
	if err != nil {
		t.Fatal(err)
	}
	if m := actions.UnlistedUnattendedDestructive(g.reg.List(), string(doc)); len(m) != 0 {
		t.Errorf("not listed under \"Unattended destructive actions\": %v", m)
	}
	found := map[string]bool{}
	for _, a := range g.reg.List() {
		if a.Name != ActionRemoveContainer && a.Name != ActionFinishDeploy {
			continue
		}
		found[a.Name] = true
		if !a.ConsoleOnly || a.System == "" || a.JobOnly {
			t.Errorf("%s: console-only=%v system=%q job-only=%v", a.Name, a.ConsoleOnly, a.System, a.JobOnly)
		}
	}
	if len(found) != 2 {
		t.Fatalf("registered: %v", found)
	}
	raw, _ := json.Marshal(RemoveContainerIn{Container: strings.Repeat("a", 64), Service: 1, Deploy: 1, Grace: 1})
	for name, a := range map[string]actions.Actor{
		"admin user": actions.UserActor("1", actions.ScopeAdmin, time.Now().Add(time.Hour)),
		"token":      actions.TokenActor("1", actions.ScopeAdmin),
		"mcp":        actions.MCPActor("1"),
	} {
		if _, err := g.reg.Invoke(context.Background(), a, ActionRemoveContainer, raw, actions.Options{Confirm: "container:" + strings.Repeat("a", 12)}); !errors.Is(err, actions.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := g.runner.cfg.Inv.Invoke(context.Background(), ActionRemoveContainer, raw, actions.Options{}); err != nil {
		t.Errorf("the system actor: %v", err)
	}
	// The input is validated at the boundary.
	bad, _ := json.Marshal(RemoveContainerIn{Container: "../../etc", Service: 1, Deploy: 1})
	if _, err := g.runner.cfg.Inv.Invoke(context.Background(), ActionRemoveContainer, bad, actions.Options{}); !errors.Is(err, actions.ErrInvalidInput) {
		t.Errorf("a bad container id: %v", err)
	}
}

// No proxy: the state map is kept, no route is applied, the boot gate of Routes stays
// closed, and the scheduler's gate still opens (the resync is what it waits for).
func TestReconcilerWithTheProxyOffAppliesNoRoutes(t *testing.T) {
	g := newRecRig(t, func(r *recRig, c *ReconcilerConfig) {
		c.Proxy = false
		c.Routes = NewRoutes(r.st, r.px, nil) // a gate that is still closed
	})
	_, id := g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.put(g.svc, addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3"), 1, "10.250.0.3", true)
	g.run()
	g.waitUp()
	if g.rec.cfg.Routes.Ready() || g.px.n() != 0 {
		t.Errorf("ready=%v, %d proxy calls", g.rec.cfg.Routes.Ready(), g.px.n())
	}
	if _, ok := g.rec.cfg.Routes.State().Get(id); !ok || g.gates.Load() != 1 {
		t.Errorf("state has the container: %v, gate %d", ok, g.gates.Load())
	}
	if g.ops.count("remove") != 1 {
		t.Errorf("%d removals", g.ops.count("remove"))
	}
}

// ---- Revision 12: later resyncs never retry a failed port ----

func TestLaterResyncsDoNotRetryAFailedPort(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.px.failed = []int{9000}
	g.resync(true) // the first resync after a start: retries
	g.px.mu.Lock()
	first := append([]bool(nil), g.px.retries...)
	g.px.mu.Unlock()
	n := g.px.n()
	for i := 0; i < 3; i++ {
		g.resync(false)
		if err := g.rec.serviceResync(context.Background(), g.svc); err != nil {
			t.Fatal(err)
		}
	}
	g.px.mu.Lock()
	later := append([]bool(nil), g.px.retries[len(first):]...)
	g.px.mu.Unlock()
	for _, r := range later {
		if r {
			t.Error("a later resync asked the agent to retry a failed port")
		}
	}
	if g.px.n() != n {
		t.Errorf("%d reloads for a resync that changed nothing", g.px.n()-n)
	}
	// "Try again" still retries.
	if err := g.routes.Refresh(context.Background()); err != nil || g.px.n() != n+1 {
		t.Fatalf("%v %d", err, g.px.n())
	}
}

// ---- Revision 14 and 15: the kept candidate, versions_kept, delete_interrupted ----

// stopFirstKept sets up a stop-first deploy that ended with its candidate kept: the
// previous version (the active deploy) is stopped, the candidate runs.
func (r *recRig) stopFirstKept(attention string) (old, cand int64, oldID, candID string) {
	r.expose(r.svc, 9000)
	old, oldID = r.activeDeploy(r.svc, 1, "10.250.0.2", false)
	cand = addDeploy(r.t, r.st, r.svc, 2, "failed", "10.250.0.3")
	candID = r.put(r.svc, cand, 1, "10.250.0.3", true)
	if attention != "" {
		mustExec(r.t, r.st, `UPDATE services SET needs_attention = ? WHERE id = ?`, attention, r.svc)
	}
	return
}

func TestResyncSettlesVersionsKeptAfterAConfirmedApply(t *testing.T) {
	g := newRecRig(t, nil)
	_, _, oldID, candID := g.stopFirstKept(AttentionVersionsKept)
	g.px.onApply = func([]agentops.Route) { g.seq.add("apply") }
	g.ops.onCall = func(op, what string) {
		if op == "remove" || op == "start" {
			g.seq.add(op)
		}
	}
	g.resync(true)
	// The order is the rule: the apply that does not name the candidate, then its removal, then the start.
	if a, r, st := g.seq.index("apply"), g.seq.index("remove"), g.seq.index("start"); a < 0 || r < 0 || st < 0 || a > r || r > st {
		t.Errorf("order %v: want apply, remove, start", g.seq.all())
	}
	if g.exists(candID) {
		t.Error("the kept candidate is still there")
	}
	if c := g.ops.find(oldID); c == nil || !c.running {
		t.Error("the previous version was not started")
	}
	if a := g.attention(g.svc); a != "" {
		t.Errorf("needs_attention is %q", a)
	}
	// The final list sends traffic to the previous version again.
	if last := g.px.last(); len(last) != 1 || last[0].Upstream != "10.250.0.2:8080" {
		t.Errorf("routes %+v", last)
	}
}

// The kept candidate is removed only after an apply that does not name it succeeded:
// with the proxy failing nothing is removed and nothing is started.
func TestVersionsKeptWaitsForAConfirmedApply(t *testing.T) {
	g := newRecRig(t, nil)
	_, _, oldID, candID := g.stopFirstKept(AttentionVersionsKept)
	// The gate is open and the proxy holds the empty list; the proxy then fails.
	cand2 := candID
	g.px.fail = errors.New("proxy down")
	g.resync(false)
	if !g.exists(cand2) || g.runningC(oldID) {
		t.Fatalf("with an unconfirmed apply: candidate exists=%v, previous running=%v", g.exists(cand2), g.runningC(oldID))
	}
	if a := g.attention(g.svc); a != AttentionVersionsKept {
		t.Errorf("needs_attention %q", a)
	}
	g.px.fail = nil
	g.resync(false)
	if g.exists(cand2) || !g.runningC(oldID) || g.attention(g.svc) != "" {
		t.Errorf("after the apply was confirmed: candidate exists=%v previous running=%v attention=%q", g.exists(cand2), g.runningC(oldID), g.attention(g.svc))
	}
}

// The commit did happen (its outcome was unknown, the deploy ended failed): it is
// finished, the flag is cleared, and the candidate, now the live version, is kept
// while the previous version is removed.
func TestUnknownCommitOutcomeThatCommittedIsFinished(t *testing.T) {
	g := newRecRig(t, nil)
	old, cand, oldID, candID := g.stopFirstKept(AttentionVersionsKept)
	_ = old
	mustExec(t, g.st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, cand, g.svc)
	mustExec(t, g.st, `UPDATE deploys SET committed_at = 'x', status = 'failed', reason_code = 'commit_failed' WHERE id = ?`, cand)
	g.resync(true)
	var status, code string
	g.st.R.QueryRow(`SELECT status, COALESCE(reason_code, '') FROM deploys WHERE id = ?`, cand).Scan(&status, &code)
	if status != "succeeded" {
		t.Errorf("the deploy is %s (%s)", status, code)
	}
	if a := g.attention(g.svc); a != "" {
		t.Errorf("needs_attention %q", a)
	}
	if !g.exists(candID) || g.exists(oldID) {
		t.Errorf("candidate (now live) exists=%v, previous version exists=%v", g.exists(candID), g.exists(oldID))
	}
}

// The previous version came back but fails its health gate: it is flagged, the
// candidate is gone, and there is no second version.
func TestVersionsKeptWithAnUnhealthyPreviousVersionNeedsAttention(t *testing.T) {
	g := newRecRig(t, nil)
	_, _, oldID, candID := g.stopFirstKept(AttentionVersionsKept)
	g.ops.startFn = func(c *fcont) {
		if c.id == oldID {
			c.running, c.exit = false, 3
		}
	}
	g.resync(true)
	if g.exists(candID) {
		t.Error("the candidate is still there")
	}
	if a := g.attention(g.svc); a != AttentionPreviousDown {
		t.Errorf("needs_attention %q, want previous_version_down", a)
	}
}

// A blue-green commit (or a rollback to a blue-green snapshot) cleared the flag but
// the kept candidate is still there: it is removed, but only after a confirmed apply.
func TestOrphanedKeptCandidateAfterACommitIsRemovedAfterAConfirmedApply(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	_, live := g.activeDeploy(g.svc, 3, "10.250.0.5", true)
	g.resync(true) // the proxy holds the upstream list
	kept := g.put(g.svc, addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3"), 1, "10.250.0.3", true)
	g.px.fail = errors.New("proxy down")
	g.routes.lastHash = "" // the outcome of the last send is unknown (a lost reply): the next apply must send
	g.resync(false)
	if !g.exists(kept) {
		t.Fatal("removed before the apply was confirmed")
	}
	g.px.fail = nil
	g.resync(false)
	if g.exists(kept) || !g.exists(live) {
		t.Errorf("kept exists=%v, live exists=%v", g.exists(kept), g.exists(live))
	}
}

// Rule 3: a stopped active container is started, its flags cleared; a service flagged
// delete_interrupted is left alone.
func TestRule3StartsAStoppedActiveContainerAndClearsTheFlags(t *testing.T) {
	for _, flag := range []string{AttentionPreviousDown, ""} {
		g := newRecRig(t, nil)
		_, id := g.activeDeploy(g.svc, 1, "10.250.0.2", false)
		if flag != "" {
			mustExec(t, g.st, `UPDATE services SET needs_attention = ? WHERE id = ?`, flag, g.svc)
		}
		g.resync(true)
		if c := g.ops.find(id); c == nil || !c.running {
			t.Errorf("flag %q: the stopped active container was not started", flag)
		}
		if a := g.attention(g.svc); a != "" {
			t.Errorf("flag %q: needs_attention %q after the restart", flag, a)
		}
		if c, ok := g.routes.State().Get(id); !ok || c.State != StateRunning {
			t.Errorf("flag %q: state %+v", flag, c)
		}
	}
}

func TestRule3SkipsAServiceFlaggedDeleteInterrupted(t *testing.T) {
	g := newRecRig(t, nil)
	_, id := g.activeDeploy(g.svc, 1, "10.250.0.2", false)
	mustExec(t, g.st, `UPDATE services SET needs_attention = ? WHERE id = ?`, AttentionDeleteInterrupted, g.svc)
	g.resync(true)
	if g.runningC(id) || g.ops.count("start") != 0 {
		t.Error("a service whose delete was interrupted had its container started")
	}
	if a := g.attention(g.svc); a != AttentionDeleteInterrupted {
		t.Errorf("needs_attention %q", a)
	}
	// And the action refuses by itself, whatever called it.
	raw, _ := json.Marshal(FinishDeployIn{Service: g.svc, Deploy: 1})
	d, _ := g.activeDeploy(g.svc, 2, "10.250.0.3", false)
	raw, _ = json.Marshal(FinishDeployIn{Service: g.svc, Deploy: d})
	if _, err := g.runner.cfg.Inv.Invoke(context.Background(), ActionFinishDeploy, raw, actions.Options{}); err == nil {
		t.Error("finish_deploy started a container of a service being deleted")
	}
}

// A delete's in-memory "not running" override is dropped when the service is reconciled.
func TestResyncDropsTheDeleteOverride(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.resync(true)
	if err := g.routes.SetOverride(context.Background(), g.svc, Override{Kind: OverrideNotRunning}); err != nil {
		t.Fatal(err)
	}
	if last := g.px.last(); last[0].Page != agentops.PageNotRunning {
		t.Fatalf("the override is not applied: %+v", last)
	}
	g.resync(false)
	if last := g.px.last(); len(last) != 1 || last[0].Upstream != "10.250.0.2:8080" {
		t.Errorf("the override survived the resync: %+v", last)
	}
}

// app_safe: a deploy the boot pass or the queue's backstop ended with unknown becomes
// yes when the old version is seen running; never when it is not.
func TestResyncRaisesAppSafeWhenTheOldVersionRuns(t *testing.T) {
	g := newRecRig(t, nil)
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	d2 := addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3")
	mustExec(t, g.st, `UPDATE deploys SET reason_code = 'interrupted', app_safe = 'unknown' WHERE id = ?`, d2)
	g.resync(true)
	if r := g.row(d2); r.appSafe != "yes" {
		t.Errorf("app_safe %q", r.appSafe)
	}
	// The backstop's job_failed, with the old version not running (and flagged so
	// that rule 3 leaves it down): stays unknown.
	g2 := newRecRig(t, nil)
	g2.activeDeploy(g2.svc, 1, "10.250.0.2", false)
	mustExec(t, g2.st, `UPDATE services SET needs_attention = ? WHERE id = ?`, AttentionDeleteInterrupted, g2.svc)
	d3 := addDeploy(t, g2.st, g2.svc, 2, "failed", "10.250.0.3")
	mustExec(t, g2.st, `UPDATE deploys SET reason_code = 'job_failed', app_safe = 'unknown' WHERE id = ?`, d3)
	g2.resync(true)
	if r := g2.row(d3); r.appSafe != "unknown" {
		t.Errorf("app_safe %q for a version that is not running", r.appSafe)
	}
}

// ---- the audit outcome of an interrupted job ----

func TestBootPassGivesTheInterruptedIntentItsSingleOutcome(t *testing.T) {
	g := newRecRig(t, nil)
	al, err := audit.New(g.st, nil)
	if err != nil {
		t.Fatal(err)
	}
	d1, j1 := g.deploy(g.svc)
	mustExec(t, g.st, `UPDATE jobs SET state = 'running', boot_id = 'old', request_id = 'job-1-a1' WHERE id = ?`, j1)
	mustExec(t, g.st, `UPDATE deploys SET status = 'running' WHERE id = ?`, d1)
	// The intent the run wrote before the process was killed (the user's own actor).
	intent := audit.Entry{ActorKind: "user", ActorID: "u1", Executor: "system", Action: "jobs.run_deploy", Target: "job:1", Risk: "mutate",
		Phase: audit.PhaseIntent, Result: audit.ResultPending, ArgsJSON: `{"job":1}`, RequestID: "job-1-a1"}
	id, err := al.Append(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := ResolveInterrupted(context.Background(), g.st, "new", time.Now(), al); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	var count int
	var kind, uid, exec, result, errText string
	g.st.R.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE phase = 'outcome' AND intent_id = ?`, id).Scan(&count)
	g.st.R.QueryRow(`SELECT actor_kind, actor_id, COALESCE(executor, ''), result, error FROM audit_log WHERE phase = 'outcome' AND intent_id = ?`, id).Scan(&kind, &uid, &exec, &result, &errText)
	if count != 1 || kind != "user" || uid != "u1" || exec != "system" || result != "error" || errText == "" {
		t.Errorf("%d outcomes: %s %s %s %s %q", count, kind, uid, exec, result, errText)
	}
	if st, err := al.IntentStatus(context.Background(), id); err != nil || st != audit.StatusError {
		t.Errorf("intent status %s %v", st, err)
	}
	// Run again: nothing is left to settle, and no second outcome is written.
	if n, _ := ResolveInterrupted(context.Background(), g.st, "newer", time.Now(), al); n != 0 {
		t.Errorf("settled %d", n)
	}
	g.st.R.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE phase = 'outcome' AND intent_id = ?`, id).Scan(&count)
	if count != 1 {
		t.Errorf("%d outcomes after the second pass", count)
	}
	if _, err := audit.Verify(context.Background(), g.st.R); err != nil {
		t.Errorf("the chain does not verify: %v", err)
	}
}

// A committed deploy resolves its intent as ok: the commit point is authoritative.
func TestBootPassOfACommittedDeployResolvesItsIntentOK(t *testing.T) {
	g := newRecRig(t, nil)
	al, _ := audit.New(g.st, nil)
	d1, j1 := g.deploy(g.svc)
	mustExec(t, g.st, `UPDATE jobs SET state = 'running', boot_id = 'old', request_id = 'job-1-a1' WHERE id = ?`, j1)
	mustExec(t, g.st, `UPDATE deploys SET status = 'running', committed_at = 'x' WHERE id = ?`, d1)
	mustExec(t, g.st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, d1, g.svc)
	id, _ := al.Append(context.Background(), audit.Entry{ActorKind: "token", ActorID: "t1", Executor: "system", Action: "jobs.run_deploy", Target: "job:1", Risk: "mutate",
		Phase: audit.PhaseIntent, Result: audit.ResultPending, ArgsJSON: `{"job":1}`, RequestID: "job-1-a1"})
	if _, err := ResolveInterrupted(context.Background(), g.st, "new", time.Now(), al); err != nil {
		t.Fatal(err)
	}
	var result string
	if err := g.st.R.QueryRow(`SELECT result FROM audit_log WHERE phase = 'outcome' AND intent_id = ?`, id).Scan(&result); err != nil || result != "ok" {
		t.Errorf("%q %v", result, err)
	}
	if r := g.row(d1); r.status != "succeeded" {
		t.Errorf("%+v", r)
	}
}

// ---- fail closed ----

func TestServiceVersionsKeptFailsClosed(t *testing.T) {
	g := newRecRig(t, nil)
	// A closed database: the read fails, and the answer is an error, never "not flagged".
	st := openStore(t)
	_ = st.Close()
	kept, err := serviceVersionsKept(context.Background(), st.R, 1)
	if err == nil || kept {
		t.Fatalf("kept=%v err=%v: a database error must be an error", kept, err)
	}
	// A readable database still answers.
	mustExec(t, g.st, `UPDATE services SET needs_attention = 'versions_kept' WHERE id = ?`, g.svc)
	if kept, err := serviceVersionsKept(context.Background(), g.st.R, g.svc); err != nil || !kept {
		t.Errorf("%v %v", kept, err)
	}
	// A stop-first deploy refused with versions_kept: nothing touched, app_safe unknown.
	g.live(g.svc, 1, "10.250.0.2")
	d, job := g.deploy(g.svc, func(s *Snapshot) { s.Strategy = "stop_first" })
	g.start(false)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	r := g.row(d)
	if r.status != "failed" || r.code != CodeVersionsKept || r.appSafe != AppSafeUnknown {
		t.Errorf("%+v", r)
	}
	if n := g.ops.count("pull") + g.ops.count("create") + g.ops.count("stop"); n != 0 {
		t.Errorf("a deploy refused with versions_kept touched Docker (%d calls)", n)
	}
}

// ---- the old container left by a shutdown during the drain ----

// After the restart the first resync removes the previous container that a shutdown
// during the drain left behind (the deploy is already succeeded).
func TestResyncRemovesThePreviousContainerLeftByAShutdownDuringTheDrain(t *testing.T) {
	g := newRecRig(t, nil)
	_, oldID := g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	d2 := addDeploy(t, g.st, g.svc, 2, "succeeded", "10.250.0.3")
	newID := g.put(g.svc, d2, 1, "10.250.0.3", true)
	mustExec(t, g.st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, d2, g.svc)
	g.resync(true)
	if g.exists(oldID) || !g.exists(newID) {
		t.Errorf("previous exists=%v, new exists=%v", g.exists(oldID), g.exists(newID))
	}
}

// ServiceOfJob reads the service from the lock key.
func TestServiceOfJob(t *testing.T) {
	if got := ServiceOfJob(jobs.Job{LockKey: "service:42"}); got != 42 {
		t.Errorf("%d", got)
	}
	for _, bad := range []string{"", "x", "service:0", "service:-3", "service:"} {
		if got := ServiceOfJob(jobs.Job{LockKey: bad}); got != 0 {
			t.Errorf("%q: %d", bad, got)
		}
	}
}

// An event queue never grows past its bound: the overflow is a full resync.
func TestEventQueueIsBounded(t *testing.T) {
	q := newEventQueue(3)
	for i := 0; i < 10; i++ {
		_ = q.add(agentops.Event{ID: fmt.Sprintf("%064x", i), Service: "1"})
	}
	ids, overflow := q.take()
	if len(ids) != 3 || !overflow {
		t.Errorf("%d ids, overflow %v", len(ids), overflow)
	}
	if ids, overflow = q.take(); len(ids) != 0 || overflow {
		t.Error("take did not empty the queue")
	}
	_ = agent.CodeNotFound
}

// ---- review round 1 ----

// Two versions never run: the stopped active container is started only after every
// container of another deploy is gone (the kill -9 in the stop-first gap leaves the
// candidate running), and if one cannot be removed the active container stays stopped.
func TestActiveContainerIsNeverStartedBesideARunningCandidate(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	_, oldID := g.activeDeploy(g.svc, 1, "10.250.0.2", false)
	cand := g.put(g.svc, addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3"), 1, "10.250.0.3", true)
	var overlap atomic.Int32
	g.ops.onCall = func(op, _ string) {
		if op == "start" && g.exists(cand) && g.runningC(cand) {
			overlap.Add(1)
		}
	}
	g.resync(true)
	if overlap.Load() != 0 {
		t.Fatal("the active container was started while the candidate still ran")
	}
	if g.exists(cand) || !g.runningC(oldID) {
		t.Errorf("candidate exists=%v, previous running=%v", g.exists(cand), g.runningC(oldID))
	}

	// The candidate cannot be removed: nothing is started.
	g2 := newRecRig(t, nil)
	g2.expose(g2.svc, 9000)
	_, old2 := g2.activeDeploy(g2.svc, 1, "10.250.0.2", false)
	cand2 := g2.put(g2.svc, addDeploy(t, g2.st, g2.svc, 2, "failed", "10.250.0.3"), 1, "10.250.0.3", true)
	g2.ops.stopErrFn = func(id string) error {
		if id == cand2 {
			return opErr(agent.CodeDocker, "stuck")
		}
		return nil
	}
	g2.resync(true)
	if g2.runningC(old2) || g2.ops.count("start") != 0 {
		t.Error("the previous version was started although the candidate could not be removed")
	}
	// And the action refuses by itself while another version runs.
	d, _ := g2.activeDeploy(g2.svc, 3, "10.250.0.4", false)
	raw, _ := json.Marshal(FinishDeployIn{Service: g2.svc, Deploy: d})
	if _, err := g2.runner.cfg.Inv.Invoke(context.Background(), ActionFinishDeploy, raw, actions.Options{}); err == nil || !strings.Contains(err.Error(), "another version") {
		t.Errorf("finish_deploy beside a running version: %v", err)
	}
	// remove_container refuses the live deploy's container, whatever called it.
	live := g2.put(g2.svc, d, 1, "10.250.0.4", true)
	rm, _ := json.Marshal(RemoveContainerIn{Container: live, Service: g2.svc, Deploy: d, Grace: 1})
	if _, err := g2.runner.cfg.Inv.Invoke(context.Background(), ActionRemoveContainer, rm, actions.Options{}); err == nil || !g2.exists(live) {
		t.Errorf("remove_container removed the live deploy's container: %v", err)
	}
}

// A versions_kept service whose candidate is slow to stop delays nothing: the other
// apps are routed and the scheduler's start gate opens at once; the slow part runs after.
func TestSlowVersionsKeptDoesNotDelayRoutesOrTheStartGate(t *testing.T) {
	g := newRecRig(t, nil)
	_, _, oldID, candID := g.stopFirstKept(AttentionVersionsKept)
	other := g.service("other")
	g.expose(other, 9001)
	g.activeDeploy(other, 1, "10.250.0.9", true)
	block := make(chan struct{})
	g.ops.onCall = func(op, what string) {
		if op == "stop" && strings.HasPrefix(candID, what) {
			<-block
		}
	}
	g.run()
	defer close(block) // runs before the run's own clean-up (LIFO)
	g.until("the start gate opened with the slow removal still pending", func() bool { return g.gates.Load() == 1 })
	found := false
	for _, r := range g.px.last() {
		if r.Port == 9001 && r.Upstream == "10.250.0.9:8080" {
			found = true
		}
	}
	if !found {
		t.Errorf("the other app is not routed: %+v", g.px.last())
	}
	if !g.exists(candID) || g.runningC(oldID) {
		t.Error("the slow settling finished early (the test did not hold it)")
	}
	block <- struct{}{} // let the removal go (the hook reads once per stop)
	g.until("the previous version runs and the flag is clear", func() bool { return g.runningC(oldID) && g.attention(g.svc) == "" })
}

// A job's tracked candidate survives a full resync that could not get the service's lock.
func TestATrackedCandidateSurvivesAFullResync(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	cand := Container{ID: strings.Repeat("c", 64), Service: g.svc, Deploy: 99, State: StateRunning, IP: "10.250.0.3"}
	g.routes.Track(cand) // what the runner does at start; the container is not in the resync's list
	unlock, err := g.runner.cfg.Locks.Lock(context.Background(), g.svc)
	if err != nil {
		t.Fatal(err)
	}
	g.resync(false)
	if _, ok := g.routes.State().Get(cand.ID); !ok {
		t.Error("the candidate of a running job was wiped from the state map")
	}
	unlock()
	g.resync(false) // no job holds the lock now: the list is the truth
	if _, ok := g.routes.State().Get(cand.ID); ok {
		t.Error("an entry of a free service survived a resync that did not list it")
	}
}

// A request for a service's resync made while the stream is down is kept and runs after
// the reconnect (here through the scheduler's OnDone, as in the server).
func TestAResyncRequestMadeWhileTheStreamIsDownIsKept(t *testing.T) {
	g := newRecRig(t, nil)
	g.onDone = func(j jobs.Job) { g.rec.ResyncService(ServiceOfJob(j)) }
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.run()
	g.waitUp()
	g.rops.mu.Lock()
	g.rops.openErr = errors.New("down")
	g.rops.mu.Unlock()
	g.rops.drop(errors.New("gone"))
	g.until("down", func() bool { return !g.rec.Up() })
	g.start(false)
	_, job := g.deploy(g.svc)
	g.wait(job)
	g.rec.svcMu.Lock()
	_, kept := g.rec.svcWant[g.svc]
	g.rec.svcMu.Unlock()
	if !kept {
		t.Fatal("the request made while the stream was down was dropped")
	}
	g.rops.mu.Lock()
	g.rops.openErr = nil
	g.rops.mu.Unlock()
	g.waitUp()
	g.until("the request was taken", func() bool {
		g.rec.svcMu.Lock()
		defer g.rec.svcMu.Unlock()
		return len(g.rec.svcWant) == 0
	})
}

// The API shows ◌ unknown while the events stream is down, not a state last seen.
func TestServiceStateIsUnknownThroughTheAPIWhileTheStreamIsDown(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	g := newSvcRig(t, func(d *ServiceDeps) { d.StreamUp = up.Load })
	g.create("p", "web", m{"container_port": 80, "public_port": 9000})
	var svc int64
	_ = g.st.R.QueryRow(`SELECT id FROM services WHERE name = 'web'`).Scan(&svc)
	id := addDeploy(t, g.st, svc, 1, "succeeded", "10.250.1.2")
	mustExec(t, g.st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, id, svc)
	open(t, g.r, up2(svc, id))
	if s := g.get("p/web"); s.State != "running" {
		t.Fatalf("stream up: %q", s.State)
	}
	up.Store(false)
	if s := g.get("p/web"); s.State != "unknown" {
		t.Errorf("stream down: %q, want unknown", s.State)
	}
}

func up2(svc, dep int64) Container {
	return Container{ID: cid(1), Service: svc, Deploy: dep, State: StateRunning, IP: "10.250.1.2"}
}

// A service whose delete failed in this process is never restarted, even if its flag could not be written.
func TestRule3SkipsAServiceWhoseDeleteFailedInMemory(t *testing.T) {
	g := newRecRig(t, nil)
	_, id := g.activeDeploy(g.svc, 1, "10.250.0.2", false)
	g.routes.State().MarkDeleteFailed(g.svc)
	g.resync(true)
	if g.runningC(id) {
		t.Error("started a container of a service whose delete failed")
	}
	g.routes.State().ClearDeleteFailed(g.svc)
	g.resync(false)
	if !g.runningC(id) {
		t.Error("not started once the marker was cleared")
	}
}

// A commit that turned out to have happened corrects the audit outcome of its run.
func TestUnknownCommitOutcomeThatCommittedCorrectsItsAuditOutcome(t *testing.T) {
	var al *audit.Logger
	g := newRecRig(t, func(r *recRig, c *ReconcilerConfig) {
		var err error
		if al, err = audit.New(r.st, nil); err != nil {
			t.Fatal(err)
		}
		c.Audit = al
	})
	d, job := g.deploy(g.svc)
	mustExec(t, g.st, `UPDATE jobs SET state = 'failed', request_id = 'job-1-a1' WHERE id = ?`, job)
	mustExec(t, g.st, `UPDATE deploys SET status = 'failed', reason_code = 'commit_failed', committed_at = 'x' WHERE id = ?`, d)
	mustExec(t, g.st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, d, g.svc)
	id, err := al.Append(context.Background(), audit.Entry{ActorKind: "user", ActorID: "u1", Executor: "system", Action: "jobs.run_deploy", Target: "job:1", Risk: "mutate",
		Phase: audit.PhaseIntent, Result: audit.ResultPending, ArgsJSON: `{"job":1}`, RequestID: "job-1-a1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := al.Append(context.Background(), audit.Entry{ActorKind: "user", ActorID: "u1", Executor: "system", Action: "jobs.run_deploy", Target: "job:1", Risk: "mutate",
		Phase: audit.PhaseOutcome, IntentID: id, Result: audit.ResultError, Error: "unknown commit", ArgsJSON: `{"job":1}`, RequestID: "job-1-a1"}); err != nil {
		t.Fatal(err)
	}
	g.put(g.svc, d, 1, "10.250.0.5", true)
	g.resync(true)
	if st, err := al.IntentStatus(context.Background(), id); err != nil || st != audit.StatusOK {
		t.Errorf("intent status %s %v, want ok", st, err)
	}
	g.resync(false) // nothing more to correct
	var n int
	g.st.R.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE phase = 'outcome' AND intent_id = ?`, id).Scan(&n)
	if n != 2 {
		t.Errorf("%d outcome rows, want 2 (the error and its correction)", n)
	}
}

// The log tail is reported kept only when a hook ran.
func TestRemoveContainerReportsLogKeptOnlyWhenAHookKeptIt(t *testing.T) {
	g := newRecRig(t, nil)
	d := addDeploy(t, g.st, g.svc, 1, "failed", "10.250.0.3")
	id := g.put(g.svc, d, 1, "10.250.0.3", true)
	out, err := g.rec.removeContainer(context.Background(), RemoveContainerIn{Container: id, Service: g.svc, Deploy: d, Grace: 0})
	if err != nil || !out.Removed || out.LogKept {
		t.Errorf("%+v %v", out, err)
	}
}

// ---- review round 2 ----

// A job switched, failed its revert and kept its candidate, then released the lock
// before the finish half: nothing is removed until a route apply without the candidate
// has succeeded (the proxy may still route to it, and its address would be freed).
func TestNothingIsRemovedUntilARouteApplyUnderTheLockSucceeds(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.resync(true)
	cand := addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3")
	candID := g.put(g.svc, cand, 1, "10.250.0.3", true)
	// The job: its candidate is tracked and routed to by an override; the revert's apply fails.
	cc, _ := ContainerFromInspectForTest(g, candID)
	g.routes.Track(cc)
	if err := g.routes.SetOverride(context.Background(), g.svc, Override{Kind: OverrideUpstream, Deploy: cand, IP: "10.250.0.3", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	g.px.fail = errors.New("proxy down")
	if err := g.routes.ClearOverride(context.Background(), g.svc); err == nil {
		t.Fatal("the revert's apply was expected to fail")
	}
	// The pass's own apply came earlier; the finish half runs now, the lock being free.
	items, err := g.rec.collect(context.Background(), g.svc)
	if err != nil {
		t.Fatal(err)
	}
	svcs, _ := g.rec.loadServices(context.Background(), g.svc)
	p := &pass{rc: g.rec, ctx: context.Background(), items: items, svcs: svcs, skipped: map[int64]bool{}, applied: true}
	p.finish(g.svc)
	if !g.exists(candID) {
		t.Fatal("the candidate was removed although the proxy could not confirm a list without it")
	}
	g.px.fail = nil
	items, _ = g.rec.collect(context.Background(), g.svc)
	p = &pass{rc: g.rec, ctx: context.Background(), items: items, svcs: svcs, skipped: map[int64]bool{}, applied: true}
	p.finish(g.svc)
	if g.exists(candID) {
		t.Error("the candidate stayed although the apply was confirmed")
	}
	if last := g.px.last(); len(last) != 1 || last[0].Upstream != "10.250.0.2:8080" {
		t.Errorf("routes %+v", last)
	}
}

func ContainerFromInspectForTest(g *recRig, id string) (Container, bool) {
	st, err := g.ops.ContainerInspect(context.Background(), id)
	if err != nil {
		g.t.Fatal(err)
	}
	return ContainerFromInspect(st)
}

// An interrupted stop-first deploy whose start stage had begun flags the service
// versions_kept at the boot pass, so a restart queued behind the (early) start gate is
// refused and no second version runs; the resync then settles it.
func TestInterruptedStopFirstCandidateFlagsTheServiceAndRefusesAQueuedRestart(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	_, oldID := g.activeDeploy(g.svc, 1, "10.250.0.2", false)
	d, j := g.deploy(g.svc, func(s *Snapshot) { s.Strategy = "stop_first" })
	mustExec(t, g.st, `UPDATE jobs SET state = 'running', boot_id = 'old', request_id = 'job-1-a1' WHERE id = ?`, j)
	mustExec(t, g.st, `UPDATE deploys SET status = 'running' WHERE id = ?`, d)
	mustExec(t, g.st, `INSERT INTO deploy_stages(deploy_id, stage, state) VALUES (?, 'start', 'running')`, d)
	candID := g.put(g.svc, d, 1, "10.250.0.3", true)
	if n, err := ResolveInterrupted(context.Background(), g.st, "new", time.Now(), nil); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if a := g.attention(g.svc); a != AttentionVersionsKept {
		t.Fatalf("needs_attention %q, want versions_kept", a)
	}
	// A restart queued before the gate opened is refused at run time and touches nothing.
	payload, _ := json.Marshal(restartPayload{Service: g.svc})
	rj, err := g.js.Enqueue(context.Background(), jobs.NewJob{Kind: jobs.KindRestart, LockKey: jobs.LockKey(g.svc), Actor: jobs.ActorOf(actions.TokenActor("1", actions.ScopeWrite)), Payload: payload, MaxAttempts: 1, TimeoutS: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.runner.runRestart(context.Background(), actions.Call{}, rj); err == nil || !strings.Contains(err.Error(), "two versions") {
		t.Fatalf("restart: %v", err)
	}
	if g.ops.count("restart") != 0 || g.runningC(oldID) || !g.runningC(candID) {
		t.Error("the refused restart changed a container")
	}
	// The resync settles it: candidate removed, previous version started, flag cleared; never both running.
	var overlap atomic.Int32
	g.ops.onCall = func(op, _ string) {
		if op == "start" && g.runningC(candID) {
			overlap.Add(1)
		}
	}
	g.resync(true)
	if overlap.Load() != 0 || g.exists(candID) || !g.runningC(oldID) || g.attention(g.svc) != "" {
		t.Errorf("overlap=%d candidate=%v previous running=%v attention=%q", overlap.Load(), g.exists(candID), g.runningC(oldID), g.attention(g.svc))
	}
	// Not flagged: a blue-green deploy, a deploy that never started, another reason on the service.
	for name, setup := range map[string]func(*recRig, int64, int64){
		"start pending": func(g *recRig, d, j int64) {
			mustExec(t, g.st, `INSERT INTO deploy_stages(deploy_id, stage, state) VALUES (?, 'start', 'pending')`, d)
		},
		"delete interrupted": func(g *recRig, d, j int64) {
			mustExec(t, g.st, `INSERT INTO deploy_stages(deploy_id, stage, state) VALUES (?, 'start', 'running')`, d)
			mustExec(t, g.st, `UPDATE services SET needs_attention = ? WHERE id = ?`, AttentionDeleteInterrupted, g.svc)
		},
		"blue green": func(g *recRig, d, j int64) {
			mustExec(t, g.st, `INSERT INTO deploy_stages(deploy_id, stage, state) VALUES (?, 'start', 'running')`, d)
		},
	} {
		h := newRecRig(t, nil)
		strategy := "stop_first"
		if name == "blue green" {
			strategy = "blue_green"
		}
		d2, j2 := h.deploy(h.svc, func(s *Snapshot) { s.Strategy = strategy })
		mustExec(t, h.st, `UPDATE jobs SET state = 'running', boot_id = 'old' WHERE id = ?`, j2)
		mustExec(t, h.st, `UPDATE deploys SET status = 'running' WHERE id = ?`, d2)
		setup(h, d2, j2)
		if _, err := ResolveInterrupted(context.Background(), h.st, "new", time.Now(), nil); err != nil {
			t.Fatal(err)
		}
		want := ""
		if name == "delete interrupted" {
			want = AttentionDeleteInterrupted
		}
		if a := h.attention(h.svc); a != want {
			t.Errorf("%s: needs_attention %q, want %q", name, a, want)
		}
	}
}

// The grace cap applies only to a container of an uncommitted deploy that keeps the
// active container from starting.
func TestGraceCapAppliesOnlyToTheCandidateThatBlocksTheStart(t *testing.T) {
	grace := func(setup func(g *recRig)) int {
		g := newRecRig(t, nil)
		mustExec(t, g.st, `UPDATE services SET stop_grace_s = 30 WHERE id = ?`, g.svc)
		var got atomic.Int32
		got.Store(-1)
		g.ops.stopGraceFn = func(gr int) { got.Store(int32(gr)) }
		setup(g)
		g.resync(true)
		return int(got.Load())
	}
	// 1. stopped active container + uncommitted candidate: capped at 10.
	if gr := grace(func(g *recRig) {
		g.activeDeploy(g.svc, 1, "10.250.0.2", false)
		g.put(g.svc, addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3"), 1, "10.250.0.3", true)
	}); gr != 10 {
		t.Errorf("blocking candidate: grace %d, want 10", gr)
	}
	// 2. running active container + stray uncommitted deploy container: the service's 30.
	if gr := grace(func(g *recRig) {
		g.activeDeploy(g.svc, 1, "10.250.0.2", true)
		g.put(g.svc, addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3"), 1, "10.250.0.3", true)
	}); gr != 30 {
		t.Errorf("nothing waits for the removal: grace %d, want 30", gr)
	}
	// 3. stopped active container + the old version of a committed deploy: the service's 30.
	if gr := grace(func(g *recRig) {
		g.activeDeploy(g.svc, 2, "10.250.0.2", false)
		old := addDeploy(t, g.st, g.svc, 1, "succeeded", "10.250.0.3")
		mustExec(t, g.st, `UPDATE deploys SET committed_at = 'x' WHERE id = ?`, old)
		g.put(g.svc, old, 1, "10.250.0.3", true)
	}); gr != 30 {
		t.Errorf("a committed old version: grace %d, want 30", gr)
	}
	// 4. a lower stop grace is never raised.
	g := newRecRig(t, nil)
	mustExec(t, g.st, `UPDATE services SET stop_grace_s = 3 WHERE id = ?`, g.svc)
	var got atomic.Int32
	g.ops.stopGraceFn = func(gr int) { got.Store(int32(gr)) }
	g.activeDeploy(g.svc, 1, "10.250.0.2", false)
	g.put(g.svc, addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3"), 1, "10.250.0.3", true)
	g.resync(true)
	if got.Load() != 3 {
		t.Errorf("grace %d, want the service's 3", got.Load())
	}
}

// finish_deploy refuses for a service whose delete failed in this process.
func TestFinishDeployRefusesAServiceWhoseDeleteFailedInMemory(t *testing.T) {
	g := newRecRig(t, nil)
	d, id := g.activeDeploy(g.svc, 1, "10.250.0.2", false)
	g.routes.State().MarkDeleteFailed(g.svc)
	raw, _ := json.Marshal(FinishDeployIn{Service: g.svc, Deploy: d})
	if _, err := g.runner.cfg.Inv.Invoke(context.Background(), ActionFinishDeploy, raw, actions.Options{}); err == nil || g.runningC(id) {
		t.Errorf("finish_deploy started it: %v", err)
	}
}

// A deploy that failed before this process started is not raised to app_safe yes.
func TestRaiseAppSafeIgnoresADeployThatFinishedBeforeSince(t *testing.T) {
	g := newRecRig(t, func(r *recRig, c *ReconcilerConfig) { c.Since = time.Now().Add(time.Hour) })
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	d2 := addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3")
	mustExec(t, g.st, `UPDATE deploys SET reason_code = 'job_failed', app_safe = 'unknown', finished_at = ? WHERE id = ?`, jobs.FormatTime(time.Now()), d2)
	g.resync(true)
	if r := g.row(d2); r.appSafe != "unknown" {
		t.Errorf("app_safe %q for a deploy that ended before Since", r.appSafe)
	}
	g2 := newRecRig(t, func(r *recRig, c *ReconcilerConfig) { c.Since = time.Now().Add(-time.Hour) })
	g2.activeDeploy(g2.svc, 1, "10.250.0.2", true)
	d3 := addDeploy(t, g2.st, g2.svc, 2, "failed", "10.250.0.3")
	mustExec(t, g2.st, `UPDATE deploys SET reason_code = 'job_failed', app_safe = 'unknown', finished_at = ? WHERE id = ?`, jobs.FormatTime(time.Now()), d3)
	g2.resync(true)
	if r := g2.row(d3); r.appSafe != "yes" {
		t.Errorf("app_safe %q for a deploy that ended after Since", r.appSafe)
	}
}

// A failed first Load keeps retrying failed ports (first stays true until a Load works);
// an overflow during the resync ends it with its own error.
func TestFirstStaysTrueUntilALoadWorksAndOverflowHasItsOwnError(t *testing.T) {
	g := newRecRig(t, nil)
	g.expose(g.svc, 9000)
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	g.px.fail = errors.New("down")
	applied, err := g.rec.fullResync(context.Background(), true, nil)
	if err != nil || applied || g.rec.loaded.Load() {
		t.Fatalf("applied=%v err=%v loaded=%v", applied, err, g.rec.loaded.Load())
	}
	g.px.fail = nil
	if applied, err := g.rec.fullResync(context.Background(), true, nil); err != nil || !applied || !g.rec.loaded.Load() {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	// An overflow buffered during the resync.
	for i := 0; i < g.rec.cfg.MaxQueued+5; i++ {
		_ = g.rec.q.add(agentops.Event{ID: fmt.Sprintf("%064x", i+1), Service: "1"})
	}
	if _, err := g.rec.fullResync(context.Background(), false, nil); !errors.Is(err, errTooManyEvents) {
		t.Errorf("overflow: %v", err)
	}
}

// "Try again" while the start-up run is blocked waiting for the first resync reconnects
// at once (the kick is sent outside the Booter's single flight).
func TestEnsureRoutesKicksTheReconnect(t *testing.T) {
	var kicks atomic.Int32
	g := newSvcRig(t, func(d *ServiceDeps) {
		d.Kick = func() { kicks.Add(1) }
		d.Boot = func(context.Context) error { return nil }
	})
	g.create("p", "web", m{"container_port": 80, "public_port": 9000})
	if kicks.Load() == 0 {
		t.Error("a route action found the gate closed and did not kick the reconciler")
	}
}
