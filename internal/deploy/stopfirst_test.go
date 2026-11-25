package deploy

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

func stopFirstMod(s *Snapshot) { s.Strategy = jobs.StrategyStopFirst }

// seqOf is the log's events without the markers inside the commit.
func seqOf(l *seqLog) string {
	var seq []string
	for _, e := range l.list() {
		if e != "commit:begin" && e != "commit:lock" && e != "health:done" {
			seq = append(seq, e)
		}
	}
	return strings.Join(seq, ",")
}

func (g *rrig) routeLists() [][]agentops.Route {
	g.px.mu.Lock()
	defer g.px.mu.Unlock()
	return append([][]agentops.Route(nil), g.px.calls...)
}

// crashCandidates makes every container that is not the live version's start and
// exit at once (the new version is broken; the previous one still works).
func (g *rrig) crashCandidates(liveDeploy int64) {
	g.ops.startFn = func(c *fcont) {
		if c.labels[agentops.LabelDeploy] != itoa(liveDeploy) {
			c.running, c.exit = false, 1
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (r *Routes) overrideCount() int {
	r.ovMu.Lock()
	defer r.ovMu.Unlock()
	return len(r.overrides)
}

// The stop-first happy path: the maintenance page goes up, the old container is
// stopped before the new one is created, the switch and commit are as in
// blue-green, there is no drain, and the old container is removed last. Route
// changes are only the page and the new upstream (table swaps, no other apply).
func TestStopFirstGapThenCommit(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	l := g.record(oldID)
	g.start(false)
	d, job := g.deploy(g.svc, stopFirstMod)
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("job %s: %+v", s, g.row(d))
	}
	want := "pull,maintenance,proxy_apply,stop:old,create,start:new,health,switch,proxy_apply,commit,stop_old,stop:old,remove:old"
	if got := seqOf(l); got != want {
		t.Fatalf("order:\n got %s\nwant %s", got, want)
	}
	lists := g.routeLists()
	if len(lists) != 3 {
		t.Fatalf("%d route lists (the exposure, the page, the new upstream), want 3: %+v", len(lists), lists)
	}
	if lists[1][0].Page != agentops.PageMaintenance || lists[1][0].Upstream != "" {
		t.Errorf("the gap's route: %+v", lists[1])
	}
	if lists[2][0].Upstream != "10.250.0.3:8080" {
		t.Errorf("the final route: %+v", lists[2])
	}
	r := g.row(d)
	if r.status != "succeeded" || r.stage != StageDone || r.code != "" {
		t.Fatalf("%+v", r)
	}
	st := g.stages(d)
	for _, s := range stopFirstStages {
		if st[s] != StageOK {
			t.Errorf("stage %s is %s", s, st[s])
		}
	}
	if _, ok := st[StageDrain]; ok {
		t.Error("a stop-first deploy has no drain stage")
	}
	if g.alive(oldID) {
		t.Error("the old container was left behind")
	}
	if g.activeDeploy(g.svc) != d {
		t.Error("the new deploy is not the active one")
	}
}

// A first deploy, and a previous version that is not running, have no gap.
func TestStopFirstHasNoGapWithoutARunningPreviousVersion(t *testing.T) {
	t.Run("first deploy", func(t *testing.T) {
		g := newRRig(t)
		g.expose(g.svc, 9000)
		g.start(false)
		d, job := g.deploy(g.svc, stopFirstMod)
		if s := g.wait(job); s != jobs.StateSucceeded {
			t.Fatalf("job %s: %+v", s, g.row(d))
		}
		st := g.stages(d)
		if st[StageMaintenance] != StageSkipped || st[StageStopOldFirst] != StageSkipped {
			t.Errorf("stages %v", st)
		}
		for _, rl := range g.routeLists() {
			if rl[0].Page == agentops.PageMaintenance {
				t.Error("the maintenance page was shown for a first deploy")
			}
		}
	})
	t.Run("previous version already stopped", func(t *testing.T) {
		g := newRRig(t)
		old := g.live(g.svc, 1, "10.250.0.2")
		oldID := g.containerOf(old).id
		g.ops.find(oldID).running = false
		g.routes.Track(Container{ID: oldID, Service: g.svc, Deploy: old, State: "exited", IP: "10.250.0.2"})
		g.expose(g.svc, 9000)
		g.start(false)
		d, job := g.deploy(g.svc, stopFirstMod)
		if s := g.wait(job); s != jobs.StateSucceeded {
			t.Fatalf("job %s: %+v", s, g.row(d))
		}
		if st := g.stages(d); st[StageMaintenance] != StageSkipped || st[StageStopOldFirst] != StageSkipped {
			t.Errorf("stages %v", st)
		}
		for _, c := range g.ops.names() {
			if strings.HasPrefix(c, "stop:"+oldID[:8]) && g.ops.count("restart") > 0 {
				t.Errorf("unexpected %s", c)
			}
		}
		if g.alive(oldID) {
			t.Error("the stopped old container was not removed after the commit")
		}
	})
}

// A failed stop-first deploy (the new version crashes): the candidate is removed, the
// old container is started again and passes its gate, the page comes down and the
// routes are back on the old version. app_safe is yes.
func TestFailedStopFirstRestartsTheOldContainer(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	g.crashCandidates(old)
	l := g.record(oldID)
	g.start(false)
	d, job := g.deploy(g.svc, stopFirstMod)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	r := g.row(d)
	if r.status != "failed" || r.code != CodeExited || r.appSafe != AppSafeYes || !strings.Contains(r.reason, "serving again") {
		t.Fatalf("%+v", r)
	}
	seq := seqOf(l)
	if !strings.Contains(seq, "stop:old,create,start:new") || !strings.Contains(seq, "remove:new,restore_old,start:old,health") {
		t.Errorf("order: %s", seq)
	}
	if !g.alive(oldID) || !g.ops.find(oldID).running {
		t.Error("the old container is not running again")
	}
	if g.containerOf(d) != nil {
		t.Error("the candidate was left behind")
	}
	if g.activeDeploy(g.svc) != old {
		t.Error("the active deploy changed")
	}
	lists := g.routeLists()
	last := lists[len(lists)-1]
	if last[0].Upstream != "10.250.0.2:8080" || last[0].Page != "" {
		t.Errorf("the routes are not back on the old version: %+v", last)
	}
	var attention sql.NullString
	g.st.R.QueryRow(`SELECT needs_attention FROM services WHERE id = ?`, g.svc).Scan(&attention)
	if attention.Valid {
		t.Errorf("needs_attention %q", attention.String)
	}
	if g.routes.overrideCount() != 0 {
		t.Error("an override is left behind")
	}
	// The maintenance page and the restart never loaded Caddy: only apply calls (table
	// swaps) are visible here; the integration test counts Caddy loads exactly.
}

// A cancel after stop_old_first restarts the old container, on a FRESH context with
// the plan's bound: health_timeout_s + 30 s.
func TestCancelAfterStopOldFirstRestartsTheOldContainer(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	inHealth := make(chan struct{}, 1)
	g.ops.healthFn = func(ctx context.Context, a agentops.HealthArgs) (agentops.HealthResult, error) {
		if strings.HasPrefix(a.Target, "10.250.0.2") { // the old version's gate: it passes
			return agentops.HealthResult{OK: true, Status: 200}, nil
		}
		select {
		case inHealth <- struct{}{}:
		default:
		}
		return agentops.HealthResult{OK: false}, nil
	}
	g.start(false)
	d, job := g.deploy(g.svc, stopFirstMod, func(s *Snapshot) { s.Health.TimeoutS = 600 })
	<-inHealth
	if g.alive(oldID) && g.ops.find(oldID).running {
		t.Fatal("the old container still runs while the new one is checked: not stop-first")
	}
	if err := g.sched.Cancel(context.Background(), job, "cancelled by the user"); err != nil {
		t.Fatal(err)
	}
	if s := g.wait(job); s != jobs.StateCancelled {
		t.Fatalf("job %s", s)
	}
	r := g.row(d)
	if r.status != "cancelled" || r.appSafe != AppSafeYes {
		t.Fatalf("%+v", r)
	}
	if !g.alive(oldID) || !g.ops.find(oldID).running {
		t.Fatal("the old container was not started again")
	}
	if g.containerOf(d) != nil {
		t.Error("the candidate was left behind")
	}
	lists := g.routeLists()
	if last := lists[len(lists)-1]; last[0].Upstream != "10.250.0.2:8080" {
		t.Errorf("the routes are not back on the old version: %+v", last)
	}
	// The start of the old container used a fresh context bounded by health_timeout_s + 30 s.
	g.ops.mu.Lock()
	defer g.ops.mu.Unlock()
	starts, dls := g.ops.ctxErrs["start"], g.ops.dls["start"]
	if len(starts) < 2 {
		t.Fatalf("starts: %v", starts)
	}
	if err := starts[len(starts)-1]; err != nil {
		t.Errorf("the restart of the old container used a dead context: %v", err)
	}
	if left := dls[len(dls)-1]; left <= 30*time.Second || left > 636*time.Second {
		t.Errorf("the bound left %v, want health_timeout_s (600) + 30 s", left)
	}
}

// The old container fails too: app_safe is no, the reason says so, the service needs
// attention, and the route serves the "not running" page, not "Deploying".
func TestOldVersionThatDoesNotComeBackIsAppSafeNoAndNeedsAttention(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	g.ops.startFn = func(c *fcont) { c.running, c.exit = false, 137 } // nothing starts
	g.start(false)
	d, job := g.deploy(g.svc, stopFirstMod)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	r := g.row(d)
	if r.status != "failed" || r.appSafe != AppSafeNo || !strings.Contains(r.reason, "the previous version did not come back: it exited with code 137") {
		t.Fatalf("%+v", r)
	}
	var attention string
	g.st.R.QueryRow(`SELECT needs_attention FROM services WHERE id = ?`, g.svc).Scan(&attention)
	if attention != AttentionPreviousDown {
		t.Errorf("needs_attention %q", attention)
	}
	lists := g.routeLists()
	if last := lists[len(lists)-1]; last[0].Page != agentops.PageNotRunning {
		t.Errorf("the final route: %+v", last)
	}
	if c, ok := g.routes.State().Get(oldID); !ok || c.State == StateRunning {
		t.Errorf("the state map says the old container is %+v", c)
	}
	if g.routes.overrideCount() != 0 {
		t.Error("an override is left behind")
	}
	// A later successful deploy clears the flag.
	g.ops.mu.Lock()
	g.ops.startFn = nil
	g.ops.mu.Unlock()
	d2, job2 := g.deploy(g.svc)
	if s := g.wait(job2); s != jobs.StateSucceeded {
		t.Fatalf("job %s: %+v", s, g.row(d2))
	}
	var a sql.NullString
	g.st.R.QueryRow(`SELECT needs_attention FROM services WHERE id = ?`, g.svc).Scan(&a)
	if a.Valid {
		t.Errorf("needs_attention %q after a good deploy", a.String)
	}
}

// A switch that fails in stop-first goes back to the page, not to the stopped old
// container, and the old container is started again before the routes return to it.
func TestStopFirstSwitchFailureRestoresThePreviousVersion(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	g.px.failIf = func(routes []agentops.Route) error {
		if routes[0].Upstream == "10.250.0.3:8080" {
			return errors.New("proxy down")
		}
		return nil
	}
	l := g.record(oldID)
	g.start(false)
	d, job := g.deploy(g.svc, stopFirstMod)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	r := g.row(d)
	if r.code != CodeSwitchFailed || r.appSafe != AppSafeYes {
		t.Fatalf("%+v", r)
	}
	seq := seqOf(l)
	// revert puts the page back (a second proxy_apply), the candidate goes, the old comes back.
	if !strings.Contains(seq, "switch,revert") || !strings.Contains(seq, "restore_old") || !strings.Contains(seq, "start:old") {
		t.Errorf("order: %s", seq)
	}
	if strings.Index(seq, "revert") > strings.Index(seq, "start:old") {
		t.Errorf("the old container started before the routes left the failed candidate: %s", seq)
	}
	lists := g.routeLists()
	if last := lists[len(lists)-1]; last[0].Upstream != "10.250.0.2:8080" {
		t.Errorf("the final route: %+v", last)
	}
	// While the old container was down, the routes never pointed at it: every list
	// between the page going up and the final one is a page, not its address.
	for i := 1; i < len(lists)-1; i++ {
		if lists[i][0].Upstream != "" {
			t.Errorf("list %d routed to %s while the old container was stopped", i, lists[i][0].Upstream)
		}
	}
}

// SIGTERM during the gap: the old container is started again within the short
// shutdown bound (5 s), on a fresh context, and the deploy is failed "shutdown".
func TestShutdownDuringTheGapRestartsTheOldContainerWithinTheShortBound(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	inHealth := make(chan struct{}, 1)
	g.ops.healthFn = func(_ context.Context, a agentops.HealthArgs) (agentops.HealthResult, error) {
		if strings.HasPrefix(a.Target, "10.250.0.2") {
			return agentops.HealthResult{OK: true, Status: 200}, nil
		}
		select {
		case inHealth <- struct{}{}:
		default:
		}
		return agentops.HealthResult{OK: false}, nil
	}
	g.start(false)
	d, _ := g.deploy(g.svc, stopFirstMod, func(s *Snapshot) { s.Health.TimeoutS = 600 })
	<-inHealth
	g.stop()
	r := g.row(d)
	if r.status != "failed" || r.code != CodeShutdown {
		t.Fatalf("%+v", r)
	}
	if !g.alive(oldID) || !g.ops.find(oldID).running {
		t.Fatal("the old container was not started again at shutdown")
	}
	g.ops.mu.Lock()
	defer g.ops.mu.Unlock()
	dls := g.ops.dls["start"]
	if left := dls[len(dls)-1]; left <= 0 || left > 5*time.Second {
		t.Errorf("the shutdown bound left %v, want at most 5 s", left)
	}
	if err := g.ops.ctxErrs["start"][len(dls)-1]; err != nil {
		t.Errorf("used a dead context: %v", err)
	}
}

// A stop that cannot be done fails the deploy and still tries to start the old
// container (it may have stopped anyway), with the routes back on it.
func TestStopFirstStopFailureLeavesTheOldVersionServing(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	g.ops.stopErrFn = func(string) error { return opErr("docker", "stop refused") }
	g.start(false)
	d, job := g.deploy(g.svc, stopFirstMod)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	r := g.row(d)
	if r.code != CodeStopFailed || r.appSafe != AppSafeYes {
		t.Fatalf("%+v", r)
	}
	if !g.ops.find(oldID).running {
		t.Error("the old container is not running")
	}
	if g.ops.count("create") != 0 {
		t.Error("a candidate was created after the stop failed")
	}
	lists := g.routeLists()
	if last := lists[len(lists)-1]; last[0].Upstream != "10.250.0.2:8080" {
		t.Errorf("the final route: %+v", last)
	}
}

// A cancel during the maintenance stage, before anything was stopped, only takes the
// page down: the old container is neither stopped nor restarted.
func TestCancelBeforeTheStopOnlyTakesThePageDown(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	known := make(chan int64, 1)
	g.px.onApply = func(routes []agentops.Route) {
		if routes[0].Page == agentops.PageMaintenance {
			j := <-known
			known <- j
			_ = g.sched.Cancel(context.Background(), j, "cancelled by the user") // the page is up, nothing is stopped yet
		}
	}
	g.start(false)
	d, job := g.deploy(g.svc, stopFirstMod)
	known <- job
	if s := g.wait(job); s != jobs.StateCancelled {
		t.Fatalf("job %s: %+v", s, g.row(d))
	}
	if c := g.ops.count("stop"); c != 0 {
		t.Errorf("%d stops: %v", c, g.ops.names())
	}
	if !g.ops.find(oldID).running {
		t.Error("the old container is not running")
	}
	lists := g.routeLists()
	if last := lists[len(lists)-1]; last[0].Upstream != "10.250.0.2:8080" {
		t.Errorf("the final route: %+v", last)
	}
	if g.row(d).appSafe != AppSafeYes {
		t.Errorf("%+v", g.row(d))
	}
}

// A stop-first commit whose outcome cannot be read keeps what it has: the candidate
// is not removed and the old container is not started again (the database may name
// either version), and the routes follow the database.
func TestStopFirstUnknownCommitOutcomeTouchesNothingMore(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	g.runner.trace = func(ev string) {
		if ev == "commit:begin" {
			mustExecIgnore(g, `ALTER TABLE services RENAME TO services_gone`)
		}
	}
	g.start(false)
	d, job := g.deploy(g.svc, stopFirstMod)
	g.wait(job)
	r := g.row(d)
	if r.status != StatusFailed || r.code != CodeCommitFailed || r.appSafe != AppSafeUnknown {
		t.Fatalf("%+v", r)
	}
	if g.containerOf(d) == nil {
		t.Error("the candidate was removed although the commit's outcome is unknown")
	}
	if g.ops.find(oldID).running || g.ops.count("restart") != 0 {
		t.Error("the old container was started again on an unknown commit")
	}
	if _, ok := g.routes.overrideOf(g.svc); ok {
		t.Error("the override was left behind")
	}
}
