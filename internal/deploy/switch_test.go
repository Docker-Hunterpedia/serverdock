package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// The tests of step 8b: switch, commit, drain, stop_old and done, over the fake
// agent and the fake proxy of the 8a rig.

// seqLog records the ops of the fake agent, the proxy_apply calls of the fake
// proxy and the runner's phase boundaries in one order.
type seqLog struct {
	mu sync.Mutex
	ev []string
	at map[string]time.Time
}

func (l *seqLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, s)
	if l.at == nil {
		l.at = map[string]time.Time{}
	}
	if _, ok := l.at[s]; !ok {
		l.at[s] = time.Now()
	}
}

func (l *seqLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

func (l *seqLog) when(s string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.at[s]
}

// record wires the log into the rig. Container ids are shown as "old" or "new".
func (g *rrig) record(oldID string) *seqLog {
	l := &seqLog{}
	name := func(what string) string {
		if oldID != "" && strings.HasPrefix(oldID, what) {
			return "old"
		}
		return "new"
	}
	prevCall, prevApply, prevTrace := g.ops.onCall, g.px.onApply, g.runner.trace
	g.ops.onCall = func(op, what string) {
		if prevCall != nil {
			prevCall(op, what)
		}
		switch op {
		case "pull", "create", "health", "start", "stop", "remove":
			if op == "start" || op == "stop" || op == "remove" {
				op += ":" + name(what)
			}
			l.add(op)
		}
	}
	g.px.onApply = func(r []agentops.Route) {
		l.add("proxy_apply")
		if prevApply != nil {
			prevApply(r)
		}
	}
	g.runner.trace = func(ev string) {
		if prevTrace != nil {
			prevTrace(ev)
		}
		l.add(ev)
	}
	return l
}

// expose makes the service public on port and sends the routes of its live version.
func (g *rrig) expose(svc int64, port int) {
	g.t.Helper()
	mustExec(g.t, g.st, `UPDATE services SET public_port = ?, container_port = 8080 WHERE id = ?`, port, svc)
	if err := g.routes.Refresh(context.Background()); err != nil {
		g.t.Fatal(err)
	}
}

func (g *rrig) runningJob() int64 {
	var id int64
	if err := g.st.R.QueryRow(`SELECT id FROM jobs WHERE state = 'running' ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		g.t.Error(err)
	}
	return id
}

func (g *rrig) alive(id string) bool {
	c := g.ops.find(id)
	return c != nil
}

func (g *rrig) activeDeploy(svc int64) int64 {
	var a *int64
	if err := g.st.R.QueryRow(`SELECT active_deploy_id FROM services WHERE id = ?`, svc).Scan(&a); err != nil {
		g.t.Fatal(err)
	}
	if a == nil {
		return 0
	}
	return *a
}

func (g *rrig) lastRoutes() []agentops.Route { return g.px.last() }

// The commit order: create, start, health, proxy_apply (the switch), the commit
// transaction, drain, then stop and remove of the old container, and never a stop of
// the old container before the commit, on every path.
func TestCommitOrder(t *testing.T) {
	// The full sequence of the happy path, exactly.
	t.Run("success", func(t *testing.T) {
		g := newRRig(t)
		old := g.live(g.svc, 1, "10.250.0.2")
		oldID := g.containerOf(old).id
		g.expose(g.svc, 9000)
		l := g.record(oldID)
		g.start(false)
		d, job := g.deploy(g.svc)
		if s := g.wait(job); s != jobs.StateSucceeded {
			t.Fatalf("job %s: %+v", s, g.row(d))
		}
		var seq []string
		for _, e := range l.list() {
			if e != "commit:begin" && e != "commit:lock" && e != "health:done" { // markers inside the commit, not phases
				seq = append(seq, e)
			}
		}
		want := "pull,create,start:new,health,switch,proxy_apply,commit,drain,stop_old,stop:old,remove:old"
		if got := strings.Join(seq, ","); got != want {
			t.Fatalf("order:\n got %s\nwant %s", got, want)
		}
		// The switch sent the candidate's upstream and the commit changed nothing.
		if px := g.px.n(); px != 2 {
			t.Errorf("%d proxy_apply calls (the exposure and the switch), want 2", px)
		}
		if r := g.lastRoutes(); len(r) != 1 || r[0].Upstream != "10.250.0.3:8080" {
			t.Errorf("routes after the deploy: %+v", r)
		}
		if g.activeDeploy(g.svc) != d {
			t.Error("the new deploy is not the active one")
		}
	})

	// On every other path the old container is never stopped, restarted or removed.
	paths := map[string]struct {
		setup func(g *rrig)
		code  string
	}{
		"unhealthy candidate": {func(g *rrig) { g.ops.startFn = func(c *fcont) { c.running, c.exit = false, 1 } }, CodeExited},
		"switch fails":        {func(g *rrig) { g.px.fail, g.px.failN = errors.New("proxy down"), 1 }, CodeSwitchFailed},
		"commit fails": {func(g *rrig) {
			g.px.onApply = func([]agentops.Route) {
				mustExecIgnore(g, `CREATE TRIGGER IF NOT EXISTS no_commit BEFORE UPDATE OF active_deploy_id ON services BEGIN SELECT RAISE(ABORT, 'injected'); END`)
			}
		}, CodeCommitFailed},
		"cancelled just before the switch": {func(g *rrig) {
			g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
				mustExecIgnore(g, `UPDATE jobs SET cancel_requested = 1, cancel_reason = 'cancelled by the user' WHERE state = 'running'`)
				return agentops.HealthResult{OK: true}, nil
			}
		}, CodeCancelled},
	}
	for name, p := range paths {
		t.Run(name, func(t *testing.T) {
			g := newRRig(t)
			old := g.live(g.svc, 1, "10.250.0.2")
			oldID := g.containerOf(old).id
			g.expose(g.svc, 9000)
			p.setup(g)
			l := g.record(oldID)
			g.start(false)
			d, job := g.deploy(g.svc)
			g.wait(job)
			if r := g.row(d); r.status == StatusSucceeded || r.code != p.code {
				t.Fatalf("%+v", r)
			}
			for _, e := range l.list() {
				if strings.HasSuffix(e, ":old") {
					t.Errorf("the old container was touched (%s) on a path that never committed: %v", e, l.list())
				}
			}
			if !g.alive(oldID) || !g.ops.find(oldID).running {
				t.Error("the old container is not running")
			}
			if g.activeDeploy(g.svc) != old {
				t.Error("the active deploy changed")
			}
		})
	}
}

// A stop of the old container is refused by the code itself before the commit, not
// only by the order of the calls.
func TestStopOldRefusesToRunBeforeTheCommit(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	rn := &run{r: g.runner, deploy: 99, svc: svcInfo{id: g.svc, hasActive: true, activeDeploy: old}, jobCtx: context.Background(), snap: baseSnap()}
	if msg := rn.stopOld(context.Background(), old); !strings.Contains(msg, "nothing was stopped") {
		t.Errorf("stopOld ran before the commit: %q", msg)
	}
	if g.ops.count("stop") != 0 || g.ops.count("remove") != 0 || !g.ops.find(oldID).running {
		t.Errorf("the old container was touched: %v", g.ops.names())
	}
	rn.committed = true
	rn.deploy = old // the candidate is never the container to stop
	if msg := rn.stopOld(context.Background(), old); !strings.Contains(msg, "nothing was stopped") || g.ops.count("stop") != 0 {
		t.Errorf("stopOld stopped the candidate's own deploy: %q", msg)
	}
}

// deploys.cancel answers "not cancellable" from the instant the deploy enters the
// switch, through the commit, the drain and stop_old; and a cancel that does reach
// the job's context (a session revoke, a shutdown flag) changes nothing: the deploy
// switches, commits and ends succeeded.
func TestSwitchAndCommitAreNotCancellable(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	g.expose(g.svc, 9000)
	var mu sync.Mutex
	got := map[string]error{}
	try := func(phase string) {
		_, err := g.sched.CancelDeploy(context.Background(), g.runningJob(), "cancelled", nonCancellableStages)
		mu.Lock()
		got[phase] = err
		mu.Unlock()
	}
	g.px.enter = func() {
		if g.runner != nil && g.px.n() >= 1 { // not the first (exposure) call: the switch's
			try("proxy_apply")
		}
	}
	g.runner.trace = func(ev string) {
		switch ev {
		case "commit:begin", "drain", "stop_old":
			try(ev)
		}
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("job %s: %+v", s, g.row(d))
	}
	mu.Lock()
	defer mu.Unlock()
	for _, phase := range []string{"proxy_apply", "commit:begin", "drain", "stop_old"} {
		if !errors.Is(got[phase], jobs.ErrStageNotCancellable) {
			t.Errorf("a cancel during %s: %v, want ErrStageNotCancellable (409 not_cancellable)", phase, got[phase])
		}
	}
	if r := g.row(d); r.status != StatusSucceeded {
		t.Errorf("%+v", r)
	}
}

// The job's context ending (a cancel that reaches it, a session revoke) or its
// watchdog expiring during proxy_apply, or during the commit, changes nothing: both
// complete, then the drain and stop_old run to their own bounds.
func TestDeadlineNeverCutsSwitchOrCommit(t *testing.T) {
	run := func(t *testing.T, name string, setup func(g *rrig)) {
		t.Run(name, func(t *testing.T) {
			g := newRRig(t, func(c *RunnerConfig) { c.DrainUnit = 5 * time.Millisecond })
			old := g.live(g.svc, 1, "10.250.0.2")
			oldID := g.containerOf(old).id
			g.expose(g.svc, 9000)
			l := g.record(oldID)
			setup(g)
			g.start(false)
			d, job := g.deploy(g.svc, func(s *Snapshot) { s.StopGraceS = 7 })
			if s := g.wait(job); s != jobs.StateSucceeded {
				t.Fatalf("job %s: %+v", s, g.row(d))
			}
			r := g.row(d)
			if r.status != StatusSucceeded || r.code != "" {
				t.Fatalf("%+v", r)
			}
			if g.activeDeploy(g.svc) != d || g.alive(oldID) {
				t.Errorf("active %d (want %d), old alive %v", g.activeDeploy(g.svc), d, g.alive(oldID))
			}
			// The drain ran its full time (drain_s 10 x 5 ms), and stop_old to its own bounds.
			if waited := l.when("stop_old").Sub(l.when("drain")); waited < 45*time.Millisecond {
				t.Errorf("the drain lasted %v, want drain_s x the unit (50ms)", waited)
			}
			g.ops.mu.Lock()
			defer g.ops.mu.Unlock()
			for _, op := range []string{"stop", "remove"} {
				errs := g.ops.ctxErrs[op]
				if len(errs) == 0 || errs[len(errs)-1] != nil {
					t.Errorf("%s ran on a dead context: %v", op, errs)
				}
			}
			// stop: stop_grace_s (7) + 15 s; remove: 30 s.
			dl := g.ops.dls["stop"]
			if last := dl[len(dl)-1]; last < 21*time.Second || last > 22*time.Second {
				t.Errorf("stop deadline %v, want about 22s", last)
			}
			dl = g.ops.dls["remove"]
			if last := dl[len(dl)-1]; last < 29*time.Second || last > 30*time.Second {
				t.Errorf("remove deadline %v, want about 30s", last)
			}
		})
	}
	run(t, "the job context ends during proxy_apply", func(g *rrig) {
		var once sync.Once
		g.px.enter = func() {
			if g.px.n() >= 1 {
				once.Do(func() { _ = g.sched.Cancel(context.Background(), g.runningJob(), "session revoked") })
			}
		}
		g.px.sleep = 30 * time.Millisecond
	})
	run(t, "the job context ends during the commit", func(g *rrig) {
		prev := g.runner.trace
		g.runner.trace = func(ev string) {
			prev(ev)
			if ev == "commit:begin" {
				_ = g.sched.Cancel(context.Background(), g.runningJob(), "session revoked")
				time.Sleep(20 * time.Millisecond)
			}
		}
	})
	run(t, "the watchdog expires during proxy_apply", func(g *rrig) {
		g.runner.cfg.Watchdog = 400 * time.Millisecond
		g.px.sleep = 800 * time.Millisecond
	})
}

// A failed switch (here: its own bound runs out while the proxy is busy) puts the
// old routes back with a context of its own, not the failed call's.
func TestFailedSwitchReappliesOldRoutesWithAFreshContext(t *testing.T) {
	g := newRRig(t, func(c *RunnerConfig) { c.SwitchBound = 40 * time.Millisecond })
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	l := g.record(oldID)
	g.px.sleep = 120 * time.Millisecond // longer than the switch bound, shorter than the fresh one
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	r := g.row(d)
	if r.status != StatusFailed || r.code != CodeSwitchFailed || r.appSafe != AppSafeYes {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.reason, "keeps serving") {
		t.Errorf("reason %q", r.reason)
	}
	// The re-apply is the one recorded call after the exposure: the old routes again.
	if n := g.px.n(); n != 2 {
		t.Fatalf("%d recorded proxy_apply calls, want 2 (exposure, re-apply)", n)
	}
	if routes := g.lastRoutes(); len(routes) != 1 || routes[0].Upstream != "10.250.0.2:8080" {
		t.Errorf("the routes after the failed switch: %+v", routes)
	}
	if st := g.stages(d); st[StageSwitch] != StageFailed || st[StageCommit] != StageSkipped {
		t.Errorf("stages %+v", st)
	}
	if g.containerOf(d) != nil || !g.alive(oldID) || !g.ops.find(oldID).running {
		t.Error("the candidate was not removed or the old container was touched")
	}
	if g.activeDeploy(g.svc) != old {
		t.Error("the active deploy changed")
	}
	for _, e := range l.list() {
		if strings.HasSuffix(e, ":old") || e == "commit" {
			t.Errorf("unexpected %s", e)
		}
	}
}

// The re-apply uses a deadline of its own that outlasts the switch's.
func TestReapplyDeadlineIsItsOwn(t *testing.T) {
	g := newRRig(t, func(c *RunnerConfig) { c.SwitchBound = 40 * time.Millisecond; c.CleanupBound = 20 * time.Second })
	g.live(g.svc, 1, "10.250.0.2")
	g.expose(g.svc, 9000)
	var mu sync.Mutex
	var deadlines []time.Duration
	g.px.sleep = 120 * time.Millisecond
	g.start(false)
	// Record the time left of each call's context at the start of the call.
	g.px.enterCtx = func(ctx context.Context) {
		var left time.Duration
		if dl, ok := ctx.Deadline(); ok {
			left = time.Until(dl)
		}
		mu.Lock()
		deadlines = append(deadlines, left)
		mu.Unlock()
	}
	_, job := g.deploy(g.svc)
	g.wait(job)
	mu.Lock()
	defer mu.Unlock()
	if len(deadlines) != 2 {
		t.Fatalf("%d calls: %v", len(deadlines), deadlines)
	}
	if deadlines[0] > 40*time.Millisecond || deadlines[1] < 15*time.Second {
		t.Errorf("the switch had %v and the re-apply %v; want 40ms and about 20s", deadlines[0], deadlines[1])
	}
}

// A switch whose re-apply fails too keeps the candidate: the proxy may still point
// at it, and both containers keep the app up until the next resync.
func TestFailedReapplyKeepsTheCandidate(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	g.px.fail = errors.New("proxy down")
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	r := g.row(d)
	if r.status != StatusFailed || r.code != CodeSwitchFailed || r.appSafe != AppSafeUnknown || !strings.Contains(r.reason, "kept") {
		t.Fatalf("%+v", r)
	}
	if g.containerOf(d) == nil || !g.containerOf(d).running {
		t.Error("the candidate was removed although the routes were not confirmed")
	}
	if c := g.ops.find(oldID); c == nil || !c.running {
		t.Error("the old container is not running")
	}
	if !g.routes.Stale() {
		t.Error("the routes are not marked stale")
	}
	if g.activeDeploy(g.svc) != old {
		t.Error("the active deploy changed")
	}
}

// A refused switch for a job cancelled just before it: the switch never happens, the
// candidate is removed and the old version is kept.
func TestCancelRequestedJustBeforeTheSwitchRefusesTheSwitch(t *testing.T) {
	for reason, want := range map[string]struct{ status, code string }{
		"superseded":            {StatusSuperseded, CodeSuperseded},
		"cancelled by the user": {StatusCancelled, CodeCancelled},
	} {
		t.Run(reason, func(t *testing.T) {
			g := newRRig(t)
			old := g.live(g.svc, 1, "10.250.0.2")
			oldID := g.containerOf(old).id
			g.expose(g.svc, 9000)
			before := g.px.n()
			g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
				mustExecIgnore(g, fmt.Sprintf(`UPDATE jobs SET cancel_requested = 1, cancel_reason = '%s' WHERE state = 'running'`, reason))
				return agentops.HealthResult{OK: true}, nil
			}
			g.start(false)
			d, job := g.deploy(g.svc)
			g.wait(job)
			r := g.row(d)
			if r.status != want.status || r.code != want.code || r.appSafe != AppSafeYes {
				t.Fatalf("%+v", r)
			}
			if g.px.n() != before {
				t.Error("a route was applied for a refused switch")
			}
			if st := g.stages(d); st[StageSwitch] != StageSkipped || st[StageHealth] != StageOK {
				t.Errorf("stages %+v", st)
			}
			if r.stage == StageSwitch {
				t.Error("the deploy entered the switch stage")
			}
			if g.containerOf(d) != nil || !g.ops.find(oldID).running || g.activeDeploy(g.svc) != old {
				t.Error("the candidate was kept or the old version was touched")
			}
		})
	}
}

// The job's context ending before the switch (a shutdown) refuses it too.
func TestShutdownJustBeforeTheSwitchRefusesIt(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	g.expose(g.svc, 9000)
	before := g.px.n()
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		go g.sched.Shutdown(context.Background())
		time.Sleep(50 * time.Millisecond) // the job's context ends while the gate's last check returns
		return agentops.HealthResult{OK: true}, nil
	}
	g.start(false)
	d, _ := g.deploy(g.svc)
	g.until("the deploy to end", func() bool { s := g.row(d).status; return s != "queued" && s != "running" })
	if r := g.row(d); r.status != StatusFailed || r.code != CodeShutdown && r.code != CodeCancelled {
		t.Fatalf("%+v", r)
	}
	if g.px.n() != before {
		t.Error("a route was applied")
	}
	if g.containerOf(d) != nil {
		t.Error("the candidate was left behind")
	}
}

// After the commit a failed write is a warning: the deploy stays succeeded, marked
// "record incomplete". If even the end of the deploy cannot be written, the queue
// settles the row when the job ends succeeded.
func TestPostCommitWriteFailureIsAWarning(t *testing.T) {
	t.Run("a stage record", func(t *testing.T) {
		g := newRRig(t)
		old := g.live(g.svc, 1, "10.250.0.2")
		oldID := g.containerOf(old).id
		mustExec(t, g.st, `CREATE TRIGGER fail_stage BEFORE UPDATE ON deploy_stages WHEN NEW.stage = 'stop_old' AND NEW.state = 'running' BEGIN SELECT RAISE(ABORT, 'injected'); END`)
		g.start(false)
		d, job := g.deploy(g.svc)
		if s := g.wait(job); s != jobs.StateSucceeded {
			t.Fatalf("job %s", s)
		}
		r := g.row(d)
		if r.status != StatusSucceeded || r.code != "warning" || !strings.Contains(r.reason, "record incomplete") {
			t.Fatalf("%+v", r)
		}
		if g.alive(oldID) || g.activeDeploy(g.svc) != d {
			t.Error("the deploy did not run to its end")
		}
		if g.containerOf(d) == nil {
			t.Error("the live version was removed")
		}
	})
	t.Run("the end of the deploy", func(t *testing.T) {
		g := newRRig(t)
		g.live(g.svc, 1, "10.250.0.2")
		// The runner's own end write (no reason code) fails, twice; the queue's settle (reason code "warning") passes.
		mustExec(t, g.st, `CREATE TRIGGER fail_end BEFORE UPDATE ON deploys WHEN NEW.status = 'succeeded' AND NEW.reason_code IS NULL BEGIN SELECT RAISE(ABORT, 'injected'); END`)
		g.start(false)
		d, job := g.deploy(g.svc)
		if s := g.wait(job); s != jobs.StateSucceeded {
			t.Fatalf("job %s", s)
		}
		r := g.row(d)
		if r.status != StatusSucceeded || r.reason != jobs.RecordIncompleteReason || r.stage != StageDone {
			t.Fatalf("%+v", r)
		}
		for s, state := range g.stages(d) {
			if state == StageRunning || state == StagePending {
				t.Errorf("stage %s is still %s", s, state)
			}
		}
		if g.activeDeploy(g.svc) != d {
			t.Error("the new deploy is not live")
		}
	})
}

// A deploy-log write that fails (the hook of step 11) never fails the deploy: the
// stage detail says so.
func TestDeployLogWriteFailureDoesNotFailTheDeploy(t *testing.T) {
	var kept atomic.Pointer[string]
	g := newRRig(t, func(c *RunnerConfig) {
		c.KeepLog = func(_ context.Context, deploy int64, container string) error {
			s := fmt.Sprintf("%d/%s", deploy, container[:8])
			kept.Store(&s)
			return errors.New("disk full (injected)")
		}
	})
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	l := g.record(oldID)
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("job %s", s)
	}
	r := g.row(d)
	if r.status != StatusSucceeded {
		t.Fatalf("%+v", r)
	}
	if p := kept.Load(); p == nil || *p != fmt.Sprintf("%d/%s", old, oldID[:8]) {
		t.Errorf("the log hook was called for %v, want the old deploy's container", p)
	}
	var detail string
	g.st.R.QueryRow(`SELECT detail FROM deploy_stages WHERE deploy_id = ? AND stage = 'stop_old'`, d).Scan(&detail)
	if !strings.Contains(detail, "log not kept") {
		t.Errorf("stop_old detail %q", detail)
	}
	if g.alive(oldID) {
		t.Error("the old container was not removed")
	}
	if !strings.Contains(strings.Join(l.list(), ","), "stop:old,remove:old") {
		t.Errorf("order %v", l.list())
	}
}

// The state machine of a blue-green deploy past the health gate: every end, with
// its stage rows, containers, active deploy and app_safe.
func TestDeployStateMachine(t *testing.T) {
	type want struct {
		status, code, safe string
		stages             map[string]string // the stage rows that are checked
		oldAlive, newAlive bool
		active             string // old, new
		reasonHas          string
	}
	ok := StageOK
	skipped := StageSkipped
	cases := []struct {
		name  string
		first bool // no previous version
		setup func(g *rrig)
		want  want
	}{
		{"succeeds and removes the old version", false, func(g *rrig) {}, want{StatusSucceeded, "", "",
			map[string]string{StageSwitch: ok, StageCommit: ok, StageDrain: ok, StageStopOld: ok}, false, true, "new", ""}},
		{"first deploy has nothing to drain or stop", true, func(g *rrig) {}, want{StatusSucceeded, "", "",
			map[string]string{StageSwitch: ok, StageCommit: ok, StageDrain: skipped, StageStopOld: skipped}, false, true, "new", ""}},
		{"cancelled before the switch", false, func(g *rrig) {
			g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
				mustExecIgnore(g, `UPDATE jobs SET cancel_requested = 1, cancel_reason = 'cancelled by the user' WHERE state = 'running'`)
				return agentops.HealthResult{OK: true}, nil
			}
		}, want{StatusCancelled, CodeCancelled, AppSafeYes,
			map[string]string{StageHealth: ok, StageSwitch: skipped, StageCommit: skipped, StageDrain: skipped, StageStopOld: skipped}, true, false, "old", "cancelled"}},
		{"the switch fails and the old routes come back", false, func(g *rrig) { g.px.fail, g.px.failN = errors.New("down"), 1 }, want{StatusFailed, CodeSwitchFailed, AppSafeYes,
			map[string]string{StageSwitch: StageFailed, StageCommit: skipped, StageDrain: skipped, StageStopOld: skipped}, true, false, "old", "keeps serving"}},
		{"the switch fails and the old routes do not come back", false, func(g *rrig) { g.px.fail = errors.New("down") }, want{StatusFailed, CodeSwitchFailed, AppSafeUnknown,
			map[string]string{StageSwitch: StageFailed, StageCommit: skipped}, true, true, "old", "kept"}},
		{"the commit fails and the old routes come back", false, func(g *rrig) {
			g.px.onApply = func([]agentops.Route) {
				mustExecIgnore(g, `CREATE TRIGGER IF NOT EXISTS no_commit BEFORE UPDATE OF active_deploy_id ON services BEGIN SELECT RAISE(ABORT, 'injected'); END`)
			}
		}, want{StatusFailed, CodeCommitFailed, AppSafeYes,
			map[string]string{StageSwitch: ok, StageCommit: StageFailed, StageDrain: skipped, StageStopOld: skipped}, true, false, "old", "keeps serving"}},
		{"a first deploy whose switch fails", true, func(g *rrig) { g.px.fail, g.px.failN = errors.New("down"), 1 }, want{StatusFailed, CodeSwitchFailed, AppSafeUnknown,
			map[string]string{StageSwitch: StageFailed}, false, false, "none", "no previous version"}},
		{"the old container cannot be stopped", false, func(g *rrig) {
			g.ops.stopErrFn = func(string) error { return opErr(agent.CodeDocker, "the Docker request failed") }
		}, want{StatusSucceeded, "warning", "",
			map[string]string{StageCommit: ok, StageDrain: ok, StageStopOld: ok}, true, true, "new", "could not be stopped"}},
		{"the old container cannot be removed", false, func(g *rrig) {
			g.ops.removeErr = opErr(agent.CodeDocker, "the Docker request failed")
		}, want{StatusSucceeded, "warning", "",
			map[string]string{StageCommit: ok, StageStopOld: ok}, true, true, "new", "could not be removed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newRRig(t)
			var old int64
			var oldID string
			if !tc.first {
				old = g.live(g.svc, 1, "10.250.0.2")
				oldID = g.containerOf(old).id
			}
			g.expose(g.svc, 9000)
			tc.setup(g)
			g.start(false)
			d, job := g.deploy(g.svc)
			g.wait(job)
			r := g.row(d)
			if r.status != tc.want.status || r.code != tc.want.code || (tc.want.safe != "" && r.appSafe != tc.want.safe) || !strings.Contains(r.reason, tc.want.reasonHas) {
				t.Fatalf("%+v, want %+v", r, tc.want)
			}
			st := g.stages(d)
			for s, state := range tc.want.stages {
				if st[s] != state {
					t.Errorf("stage %s is %s, want %s (all: %v)", s, st[s], state, st)
				}
			}
			if oldID != "" && g.alive(oldID) != tc.want.oldAlive {
				t.Errorf("old container alive = %v, want %v", g.alive(oldID), tc.want.oldAlive)
			}
			if (g.containerOf(d) != nil) != tc.want.newAlive {
				t.Errorf("candidate alive = %v, want %v", g.containerOf(d) != nil, tc.want.newAlive)
			}
			wantActive := map[string]int64{"old": old, "new": d, "none": 0}[tc.want.active]
			if got := g.activeDeploy(g.svc); got != wantActive {
				t.Errorf("active deploy %d, want %s (%d)", got, tc.want.active, wantActive)
			}
			if tc.want.status == StatusSucceeded {
				if r.stage != StageDone {
					t.Errorf("a succeeded deploy's stage is %q", r.stage)
				}
			}
			for s, state := range st {
				if state == StageRunning || state == StagePending {
					t.Errorf("stage %s is left %s on an ended deploy", s, state)
				}
			}
		})
	}
}

// With nothing to apply (the proxy not ready on this server) the switch is not a
// failure: the deploy still commits, and says so.
func TestNotReadyRoutesDoNotFailTheSwitch(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	g.routes.state.mu.Lock()
	g.routes.state.ready = false
	g.routes.state.mu.Unlock()
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("job %s: %+v", s, g.row(d))
	}
	var detail string
	g.st.R.QueryRow(`SELECT detail FROM deploy_stages WHERE deploy_id = ? AND stage = 'switch'`, d).Scan(&detail)
	if !strings.Contains(detail, "not ready") || g.activeDeploy(g.svc) != d {
		t.Errorf("switch detail %q, active %d", detail, g.activeDeploy(g.svc))
	}
}

// The switch bound covers the wait for the route lock: one in-flight apply that
// outlasts it fails the switch (the old version keeps serving), and the repair, on
// a fresh context, waits its turn.
func TestSwitchBoundCoversWaitingForAnInFlightApply(t *testing.T) {
	g := newRRig(t, func(c *RunnerConfig) { c.SwitchBound = 60 * time.Millisecond })
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	other := g.service("api") // another service whose apply holds the route lock
	od := g.live(other, 1, "10.250.0.4")
	otherC := g.containerOf(od)
	g.expose(other, 9001)
	started, release := make(chan struct{}), make(chan struct{})
	var once, once2 sync.Once
	g.px.enter = func() { once.Do(func() { close(started); <-release }) }
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		once2.Do(func() {
			// A route change of the other service, blocked inside proxy_apply with the lock held.
			go g.routes.SetContainer(context.Background(), Container{ID: otherC.id, Service: other, Deploy: od, State: "exited", IP: "10.250.0.4"})
			<-started
			time.AfterFunc(400*time.Millisecond, func() { close(release) })
		})
		return agentops.HealthResult{OK: true}, nil
	}
	g.start(false)
	t0 := time.Now()
	d, job := g.deploy(g.svc)
	g.wait(job)
	r := g.row(d)
	if r.status != StatusFailed || r.code != CodeSwitchFailed || r.appSafe != AppSafeYes {
		t.Fatalf("%+v", r)
	}
	if time.Since(t0) < 300*time.Millisecond {
		t.Error("the repair did not wait for the lock holder")
	}
	if g.containerOf(d) != nil || !g.alive(oldID) || g.activeDeploy(g.svc) != old {
		t.Error("the candidate was kept, or the old version was touched")
	}
	// The repair put the service's old route back (the other service's own change landed too).
	var mine *agentops.Route
	for _, rt := range g.lastRoutes() {
		if rt.Port == 9000 {
			rt := rt
			mine = &rt
		}
	}
	if mine == nil || mine.Upstream != "10.250.0.2:8080" {
		t.Errorf("routes after the repair: %+v", g.lastRoutes())
	}
}

// A panic after the commit never removes the live version: the deploy ends
// succeeded with a warning.
func TestPanicAfterTheCommitKeepsTheNewVersion(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	g.runner.trace = func(ev string) {
		if ev == "drain" {
			panic("boom")
		}
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("job %s: %+v", s, g.row(d))
	}
	r := g.row(d)
	if r.status != StatusSucceeded || r.code != "warning" || strings.Contains(r.reason, "boom") {
		t.Fatalf("%+v", r)
	}
	c := g.containerOf(d)
	if c == nil || !c.running {
		t.Error("the live version was removed after a panic past the commit")
	}
	if g.activeDeploy(g.svc) != d {
		t.Error("the new deploy is not live")
	}
}

// SIGTERM during the drain: the committed deploy is written succeeded at once, with
// the honest reason (the previous container is removed at the next start), and the
// job ends; nothing is left for the boot pass but a failed write. The old container
// is left for the resync.
func TestShutdownDuringTheDrainWritesTheHonestEnd(t *testing.T) {
	g := newRRig(t, func(c *RunnerConfig) { c.DrainUnit = time.Second })
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	inDrain := make(chan struct{})
	g.runner.trace = func(ev string) {
		if ev == "drain" {
			close(inDrain)
		}
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	<-inDrain
	t0 := time.Now()
	g.stop() // the scheduler shuts down: the job context ends with ErrShutdown
	if took := time.Since(t0); took > 5*time.Second {
		t.Errorf("the shutdown waited %v for a 10 s drain", took)
	}
	if s := g.jobState(job); s != jobs.StateSucceeded {
		t.Errorf("job %s, want succeeded: the runner wrote the end itself", s)
	}
	if g.activeDeploy(g.svc) != d || !g.alive(oldID) || g.containerOf(d) == nil {
		t.Error("the committed state was disturbed")
	}
	r := g.row(d)
	if r.status != StatusSucceeded || r.code != "warning" || !strings.Contains(r.reason, "stopped during the drain") || strings.Contains(r.reason, "record incomplete") {
		t.Fatalf("%+v", r)
	}
	if n, err := ResolveInterrupted(context.Background(), g.st, "next-boot", time.Now()); err != nil || n != 0 {
		t.Fatalf("the boot pass found %d jobs (%v), want none", n, err)
	}
}

// The commit never retries another service's failed port: with a port failed, a
// commit that leaves the routes unchanged sends nothing, and one that changes them
// sends with retry_failed false. (ApplyRoutes with a mutate would force a send.)
func TestCommitRoutesNeverRetriesAFailedPort(t *testing.T) {
	r, st, px := newRoutes(t)
	a, b := addService(t, st, "a"), addService(t, st, "b")
	dA := expose(t, st, a, 9000, 8080, 1, "10.250.1.2")
	dB := expose(t, st, b, 9001, 8080, 1, "10.250.1.3")
	px.failed = []int{9001} // b's port could not be bound
	open(t, r, up(a, dA, 1, StateRunning, "10.250.1.2"), up(b, dB, 2, StateRunning, "10.250.1.3"))
	if !r.hasFailed() {
		t.Fatal("no failed port to test with")
	}
	// A candidate for a, switched in by an override.
	r.Track(Container{ID: cid(3), Service: a, Deploy: 50, State: StateRunning, IP: "10.250.1.9"})
	if err := r.SetOverride(bg, a, Override{Kind: OverrideUpstream, Deploy: 50, IP: "10.250.1.9", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	n := px.n()
	dNew := addDeploy(t, st, a, 2, "running", "10.250.1.9")
	err := r.CommitRoutes(bg, a, func() error {
		_, err := st.W.Exec(`UPDATE services SET active_deploy_id = ? WHERE id = ?`, dNew, a)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// The deploy id in the override (50) differs from the row's: the route is unchanged
	// only if the same deploy is meant; here the commit changes the deploy, so the route
	// goes to "not running" and is sent, with retry_failed false.
	if px.n() != n+1 {
		t.Fatalf("%d sends after the commit, want 1", px.n()-n)
	}
	px.mu.Lock()
	lastRetry := px.retries[len(px.retries)-1]
	px.mu.Unlock()
	if lastRetry {
		t.Error("the commit asked the agent to retry a failed port")
	}
	if _, ok := r.overrides[a]; ok {
		t.Error("the commit left the override in place")
	}
	// A commit that changes no route sends nothing at all.
	r.Track(Container{ID: cid(4), Service: a, Deploy: dNew, State: StateRunning, IP: "10.250.1.9"})
	_ = r.Refresh(bg) // the candidate is now a running container of the active deploy
	n = px.n()
	dNew2 := addDeploy(t, st, b, 2, "running", "10.250.1.4")
	if err := r.CommitRoutes(bg, b, func() error { _ = dNew2; return nil }); err != nil {
		t.Fatal(err)
	}
	if px.n() != n {
		t.Errorf("a commit that changed no route sent %d calls", px.n()-n)
	}
	// Whereas an ApplyRoutes with a mutate does force a send while a port is failed.
	if err := r.ApplyRoutes(bg, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if px.n() != n+1 {
		t.Errorf("ApplyRoutes with a mutate sent %d calls while a port was failed, want 1", px.n()-n)
	}
}

// The route lock wait honours ctx: a caller whose context ends while another apply
// is in flight gives up (its mutate never ran), and the lock still works after.
func TestRouteLockWaitHonoursCtx(t *testing.T) {
	r, st, px := newRoutes(t)
	svc := addService(t, st, "web")
	d := expose(t, st, svc, 9000, 8080, 1, "10.250.1.2")
	open(t, r, up(svc, d, 1, StateRunning, "10.250.1.2"))
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	px.enter = func() { once.Do(func() { close(started); <-release }) }
	first := make(chan error, 1)
	go func() { first <- r.SetContainer(bg, up(svc, d, 1, "exited", "10.250.1.2")) }() // changes the route: sends, and blocks
	<-started

	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	ran := false
	t0 := time.Now()
	err := r.ApplyRoutes(ctx, func() error { ran = true; return nil })
	var me *MutateError
	if !errors.As(err, &me) || !errors.Is(err, context.DeadlineExceeded) || ran {
		t.Fatalf("err %v, mutate ran %v", err, ran)
	}
	if took := time.Since(t0); took > 2*time.Second {
		t.Errorf("the wait took %v", took)
	}
	// A cancelled context on a FREE lock still takes it, like the plain mutex did.
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	done, cancelDone := context.WithCancel(bg)
	cancelDone()
	if err := r.Refresh(done); err != nil && errors.Is(err, context.Canceled) && strings.Contains(err.Error(), "route lock") {
		t.Errorf("a free lock was refused to a cancelled context: %v", err)
	}
	if err := r.Refresh(bg); err != nil {
		t.Errorf("the lock is stuck: %v", err)
	}
}

// Documentation only: nothing enforces a budget after the switch (the watchdog does
// not apply there), but the phases' bounds, at the largest settings, are meant to
// fit jobs.timeout_s: the lock wait and the commit, the switch, the drain, three
// old containers (each: the log hook's 10 s, the stop with its grace, the removal)
// and the two fresh-context repairs.
func TestPhaseBoundsFitTheJobTimeout(t *testing.T) {
	for _, strategy := range []string{"blue_green", "stop_first"} {
		set := jobs.Settings{Strategy: strategy, HealthTimeoutS: 600, DrainS: 300, StopGraceS: 300}
		total, err := jobs.TimeoutSeconds(set)
		if err != nil {
			t.Fatal(err)
		}
		const oldContainers = 3
		phases := SwitchBound + SwitchBound + CommitBound + 300*time.Second +
			oldContainers*(recordBound+300*time.Second+StopExtra+RemoveBound) + RemoveBound + 2*30*time.Second
		if time.Duration(total)*time.Second < phases {
			t.Logf("%s: timeout_s %d s is below the phases' worst case %v (documentation only: the watchdog is not applied after the switch)", strategy, total, phases)
		}
	}
}

// ---- review fixes of step 8b ----

// A panic after the switch was applied and before the commit must put the routes
// back BEFORE the candidate is removed: no override is left behind, the proxy is
// re-applied (old version) before the candidate goes, and app_safe is honest.
func TestPanicAfterTheSwitchUnwindsTheRoutesBeforeRemovingTheCandidate(t *testing.T) {
	cases := map[string]func(g *rrig){
		"inside the commit's mutate": func(g *rrig) {
			g.runner.trace = func(ev string) {
				if ev == "commit:begin" {
					panic("boom")
				}
			}
		},
		"in the proxy call of the switch": func(g *rrig) {
			var once sync.Once
			g.px.onApply = func([]agentops.Route) { once.Do(func() { panic("boom") }) }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			g := newRRig(t)
			old := g.live(g.svc, 1, "10.250.0.2")
			oldID := g.containerOf(old).id
			g.expose(g.svc, 9000)
			setup(g)
			l := g.record(oldID)
			g.start(false)
			d, job := g.deploy(g.svc)
			if s := g.wait(job); s != jobs.StateFailed {
				t.Fatalf("job %s: %+v", s, g.row(d))
			}
			r := g.row(d)
			if r.status != StatusFailed || r.code != CodeInternal || r.appSafe != AppSafeYes {
				t.Fatalf("%+v", r)
			}
			if _, ok := g.routes.overrideOf(g.svc); ok {
				t.Error("the override was left behind")
			}
			if routes := g.lastRoutes(); len(routes) != 1 || routes[0].Upstream != "10.250.0.2:8080" {
				t.Errorf("the proxy was not re-applied to the old version: %+v", routes)
			}
			ev := l.list()
			lastApply, stopNew := -1, -1
			for i, e := range ev {
				if e == "proxy_apply" {
					lastApply = i
				}
				if e == "stop:new" && stopNew < 0 {
					stopNew = i
				}
			}
			if stopNew < 0 || lastApply < 0 || lastApply > stopNew {
				t.Errorf("the candidate was removed before the routes were put back: %v", ev)
			}
			if g.containerOf(d) != nil || !g.ops.find(oldID).running || g.activeDeploy(g.svc) != old {
				t.Error("candidate not removed, or old version touched")
			}
			if g.routes.Stale() {
				t.Error("routes are stale although the repair was confirmed")
			}
		})
	}
	// After the commit transaction (a panic in the trace after it) the deploy is
	// succeeded and no override is left.
	t.Run("just after the commit", func(t *testing.T) {
		g := newRRig(t)
		g.live(g.svc, 1, "10.250.0.2")
		g.expose(g.svc, 9000)
		g.runner.trace = func(ev string) {
			if ev == "commit" {
				panic("boom")
			}
		}
		g.start(false)
		d, job := g.deploy(g.svc)
		if s := g.wait(job); s != jobs.StateSucceeded {
			t.Fatalf("job %s: %+v", s, g.row(d))
		}
		if _, ok := g.routes.overrideOf(g.svc); ok {
			t.Error("the override was left behind")
		}
		if r := g.lastRoutes(); len(r) != 1 || r[0].Upstream != "10.250.0.3:8080" {
			t.Errorf("routes %+v", r)
		}
		if g.containerOf(d) == nil {
			t.Error("the live version was removed")
		}
	})
}

// A supersede during the drain hides a later shutdown from the job's cancel cause;
// the scheduler's own stop signal still ends the drain, and the clean-up bound
// shrinks.
func TestShutdownAfterASupersedeDuringTheDrainReturnsPromptly(t *testing.T) {
	g := newRRig(t, func(c *RunnerConfig) { c.DrainUnit = time.Second })
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	inDrain := make(chan struct{})
	g.runner.trace = func(ev string) {
		if ev == "drain" {
			close(inDrain)
		}
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	<-inDrain
	if err := g.sched.Cancel(context.Background(), job, "superseded"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	g.stop()
	if took := time.Since(t0); took > 5*time.Second {
		t.Fatalf("the shutdown waited %v behind a 10 s drain", took)
	}
	if r := g.row(d); r.status != StatusSucceeded || !strings.Contains(r.reason, "stopped during the drain") {
		t.Errorf("a committed deploy was recorded %+v", r)
	}
	if g.activeDeploy(g.svc) != d || !g.alive(oldID) {
		t.Error("the committed state was disturbed (the old container is left for the resync)")
	}
}

// The commit's wait for the route lock has the switch's allowance; only the
// transaction has the 10 s (here 20 ms) bound.
func TestCommitWaitsForAnInFlightApplyOfAnotherService(t *testing.T) {
	g := newRRig(t, func(c *RunnerConfig) { c.CommitBound = 20 * time.Millisecond })
	g.live(g.svc, 1, "10.250.0.2")
	g.expose(g.svc, 9000)
	other := g.service("api")
	od := g.live(other, 1, "10.250.0.4")
	otherC := g.containerOf(od)
	g.expose(other, 9001)
	started := make(chan struct{})
	var once, once2 sync.Once
	g.px.enter = func() {
		if g.px.n() >= 3 { // after the switch's own apply (call 3)
			once.Do(func() { close(started); time.Sleep(200 * time.Millisecond) })
		}
	}
	g.runner.trace = func(ev string) {
		if ev == "commit:lock" {
			once2.Do(func() {
				go g.routes.SetContainer(context.Background(), Container{ID: otherC.id, Service: other, Deploy: od, State: "exited", IP: "10.250.0.4"})
				<-started
			})
		}
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("job %s: %+v", s, g.row(d))
	}
}

// The commit outcome is unknown (the write fails and the verifying read fails too):
// both containers stay, app_safe is unknown.
func TestUnknownCommitOutcomeKeepsBothVersions(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	g.runner.trace = func(ev string) {
		if ev == "commit:begin" { // the commit's write and the read after it both find no table
			mustExecIgnore(g, `ALTER TABLE services RENAME TO services_gone`)
		}
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	r := g.row(d)
	if r.status != StatusFailed || r.code != CodeCommitFailed || r.appSafe != AppSafeUnknown || !strings.Contains(r.reason, "could not tell") {
		t.Fatalf("%+v", r)
	}
	if g.containerOf(d) == nil || !g.ops.find(oldID).running {
		t.Error("a container was removed although the commit's outcome is unknown")
	}
	if _, ok := g.routes.overrideOf(g.svc); ok {
		t.Error("the override was left behind")
	}
}

// A candidate that stopped between the gate and the switch is never switched to or
// committed.
func TestADeadCandidateIsNeverSwitchedTo(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	before := g.px.n()
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		g.ops.mu.Lock()
		for _, c := range g.ops.cs {
			if c.id != oldID {
				c.running, c.exit = false, 1
			}
		}
		g.ops.mu.Unlock()
		return agentops.HealthResult{OK: true}, nil
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	r := g.row(d)
	if r.status != StatusFailed || (r.code != CodeExited && r.code != CodeStartFailed) || g.px.n() != before {
		t.Fatalf("%+v (proxy_apply calls %d -> %d)", r, before, g.px.n())
	}
	if g.activeDeploy(g.svc) != old || g.containerOf(d) != nil {
		t.Error("a dead candidate was committed or left behind")
	}
}

// The watchdog firing between the health gate and the switch still refuses it: it
// applies up to the switch.
func TestWatchdogBetweenTheGateAndTheSwitchRefusesTheSwitch(t *testing.T) {
	g := newRRig(t, func(c *RunnerConfig) { c.Watchdog = 100 * time.Millisecond })
	g.live(g.svc, 1, "10.250.0.2")
	g.expose(g.svc, 9000)
	before := g.px.n()
	// The gate is done and recorded; only then does the watchdog run out.
	g.runner.trace = func(ev string) {
		if ev == "health:done" {
			time.Sleep(200 * time.Millisecond)
		}
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if r := g.row(d); r.code != CodeTimeout || g.px.n() != before || g.containerOf(d) != nil {
		t.Fatalf("%+v", r)
	}
}

// A candidate the routes pointed at is stopped with stop_grace_s, not 0.
func TestASwitchedCandidateIsStoppedWithItsGrace(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	g.expose(g.svc, 9000)
	g.px.fail, g.px.failN = errors.New("down"), 1 // the switch fails: the routes were never applied...
	var graces []int
	g.ops.stopGraceFn = func(g int) { graces = append(graces, g) }
	g.start(false)
	_, job := g.deploy(g.svc, func(s *Snapshot) { s.StopGraceS = 7 })
	g.wait(job)
	// ...but the switch was attempted (the outcome of an apply is unknown), so the grace applies.
	g.ops.mu.Lock()
	defer g.ops.mu.Unlock()
	if len(graces) != 1 || graces[0] != 7 {
		t.Errorf("candidate stop graces %v, want [7]", graces)
	}
}

// Routes level: an override that IS set is cleared even when the repair cannot get
// the route lock within its context.
func TestClearOverrideNeedsNoRouteLock(t *testing.T) {
	r, st, _ := newRoutes(t)
	svc := addService(t, st, "web")
	d := expose(t, st, svc, 9000, 8080, 1, "10.250.1.2")
	open(t, r, up(svc, d, 1, StateRunning, "10.250.1.2"), up(svc, 50, 2, StateRunning, "10.250.1.9"))
	if err := r.SetOverride(bg, svc, Override{Kind: OverrideUpstream, Deploy: 50, IP: "10.250.1.9", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.overrideOf(svc); !ok {
		t.Fatal("the override was not set")
	}
	r.lock <- struct{}{} // another apply holds the route lock
	ctx, cancel := context.WithTimeout(bg, 30*time.Millisecond)
	defer cancel()
	err := r.ClearOverride(ctx, svc)
	var me *MutateError
	if !errors.As(err, &me) {
		t.Fatalf("err %v, want the lock wait to fail", err)
	}
	if _, ok := r.overrideOf(svc); ok {
		t.Fatal("the override survived a clear that could not get the lock")
	}
	<-r.lock
	if err := r.Refresh(bg); err != nil {
		t.Fatal(err)
	}
}

// Runner path: the switch's apply succeeds (the override IS set), then another
// service's apply takes the route lock just before the commit: the commit's lock
// wait fails (not committed), and the repair loses its wait too. No override is
// left, the candidate is kept, and the next Refresh routes to the old address.
func TestRepairThatLosesTheLockWaitStillClearsTheOverride(t *testing.T) {
	g := newRRig(t, func(c *RunnerConfig) { c.SwitchBound = 80 * time.Millisecond; c.CleanupBound = 50 * time.Millisecond })
	old := g.live(g.svc, 1, "10.250.0.2")
	g.expose(g.svc, 9000)
	other := g.service("api")
	od := g.live(other, 1, "10.250.0.4")
	otherC := g.containerOf(od)
	g.expose(other, 9001)
	started, release := make(chan struct{}), make(chan struct{})
	var once, once2 sync.Once
	g.px.enter = func() {
		if g.px.n() >= 3 { // after the switch's own apply
			once.Do(func() { close(started); <-release })
		}
	}
	g.runner.trace = func(ev string) {
		if ev == "commit:lock" {
			once2.Do(func() {
				go g.routes.SetContainer(context.Background(), Container{ID: otherC.id, Service: other, Deploy: od, State: "exited", IP: "10.250.0.4"})
				<-started
			})
		}
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	r := g.row(d)
	if r.status != StatusFailed || r.code != CodeCommitFailed || r.appSafe != AppSafeUnknown || g.containerOf(d) == nil {
		t.Fatalf("%+v (candidate kept: %v)", r, g.containerOf(d) != nil)
	}
	if _, ok := g.routes.overrideOf(g.svc); ok {
		t.Fatal("the override was left behind")
	}
	close(release)
	g.until("the holder to finish", func() bool { return g.px.n() >= 4 })
	if err := g.routes.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	var mine string
	for _, rt := range g.lastRoutes() {
		if rt.Port == 9000 {
			mine = rt.Upstream
		}
	}
	if mine != "10.250.0.2:8080" || g.activeDeploy(g.svc) != old {
		t.Errorf("the port routes to %q after the next refresh", mine)
	}
}
