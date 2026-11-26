package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// services registers the service actions on the rig's registry (the scheduler must
// exist: call g.start first).
func (g *rrig) services() {
	g.t.Helper()
	if g.sched == nil {
		g.t.Fatal("start the scheduler first")
	}
	RegisterServiceActions(g.reg, ServiceDeps{DB: g.st, Routes: g.routes, Jobs: g.sched, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

var (
	rigWrite = actions.TokenActor("1", actions.ScopeWrite)
	rigRead  = actions.TokenActor("2", actions.ScopeRead)
)

func (g *rrig) invoke(a actions.Actor, name string, in any) (any, error) {
	g.t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		g.t.Fatal(err)
	}
	res, err := g.reg.Invoke(context.Background(), a, name, raw, actions.Options{})
	return res.Out, err
}

// live deploys the service and waits for it to be the live version.
func (g *rrig) liveVia(mods ...snapMod) int64 {
	g.t.Helper()
	d, job := g.deploy(g.svc, mods...)
	if s := g.wait(job); s != jobs.StateSucceeded {
		g.t.Fatalf("job %s: %+v", s, g.row(d))
	}
	return d
}

func (g *rrig) deployOut(out any, err error) DeployOut {
	g.t.Helper()
	if err != nil {
		g.t.Fatal(err)
	}
	return out.(DeployOut)
}

func (g *rrig) snapOf(deploy int64) (Snapshot, string, int64) {
	g.t.Helper()
	var raw, kind string
	var src sql.NullInt64
	if err := g.st.R.QueryRow(`SELECT snapshot_json, kind, source_deploy_id FROM deploys WHERE id = ?`, deploy).Scan(&raw, &kind, &src); err != nil {
		g.t.Fatal(err)
	}
	s, err := ParseSnapshot(raw)
	if err != nil {
		g.t.Fatal(err)
	}
	return s, kind, src.Int64
}

func (g *rrig) deployID(job int64) int64 {
	var id int64
	if err := g.st.R.QueryRow(`SELECT id FROM deploys WHERE job_id = ?`, job).Scan(&id); err != nil {
		g.t.Fatal(err)
	}
	return id
}

func (g *rrig) serviceImage() string {
	var s string
	g.st.R.QueryRow(`SELECT image_ref FROM services WHERE id = ?`, g.svc).Scan(&s)
	return s
}

// Rollback runs an earlier once-live version again, pinned to its image id: the
// image is present locally, so it is not pulled, and the service's desired image
// is untouched.
func TestRollbackPinsTheImageIdAndRunsThatSnapshot(t *testing.T) {
	g := newRRig(t)
	g.start(false)
	g.services()
	d1 := g.liveVia(func(s *Snapshot) { s.ImageRef = "nginx:1" })
	d2 := g.liveVia(func(s *Snapshot) { s.ImageRef = "nginx:2" })
	pulls := int(g.ops.pullN.Load())

	out := g.deployOut(g.invoke(rigWrite, "services.rollback", m{"service": "p/web", "to": 1}))
	if out.Deploy != 3 || out.Status != "queued" {
		t.Fatalf("%+v", out)
	}
	d3 := g.deployID(out.Job)
	snap, kind, src := g.snapOf(d3)
	if kind != "rollback" || src != d1 || snap.ImageRef != "nginx:1" || snap.ImageID != testImageID {
		t.Fatalf("kind %s source %d snapshot %+v", kind, src, snap)
	}
	if s := g.wait(out.Job); s != jobs.StateSucceeded {
		t.Fatalf("job %s: %+v", s, g.row(d3))
	}
	if g.activeDeploy(g.svc) != d3 {
		t.Error("the rollback is not live")
	}
	if int(g.ops.pullN.Load()) != pulls {
		t.Error("a local image was pulled again")
	}
	if g.row(d3).imageID != testImageID {
		t.Errorf("%+v", g.row(d3))
	}
	if got := g.serviceImage(); got != "nginx:1" { // addService's default; set_source and deploy own this, not rollback
		t.Errorf("the service's desired image changed to %q", got)
	}
	_ = d2
}

// Only a deploy that was once live is a target.
func TestRollbackToADeployThatWasNeverLiveIsRefused(t *testing.T) {
	g := newRRig(t)
	g.start(false)
	g.services()
	d1 := g.liveVia()
	g.ops.startFn = func(c *fcont) { c.running, c.exit = false, 1 }
	d2, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	g.ops.mu.Lock()
	g.ops.startFn = nil
	g.ops.mu.Unlock()
	before := countRows(t, g.st, "deploys")
	_, err := g.invoke(rigWrite, "services.rollback", m{"service": "p/web", "to": 2})
	wantKind(t, err, actions.ErrInvalidInput)
	if !strings.Contains(err.Error(), "never live") {
		t.Errorf("%v", err)
	}
	_, err = g.invoke(rigWrite, "services.rollback", m{"service": "p/web", "to": 1}) // the live one
	wantKind(t, err, actions.ErrConflict)
	_, err = g.invoke(rigWrite, "services.rollback", m{"service": "p/web", "to": 9})
	wantKind(t, err, actions.ErrNotFound)
	_, err = g.invoke(rigRead, "services.rollback", m{"service": "p/web", "to": 1})
	wantKind(t, err, actions.ErrDenied)
	if countRows(t, g.st, "deploys") != before {
		t.Error("a refused rollback left a deploy")
	}
	_ = d1
	_ = d2
}

// With the local image removed, a rollback pulls the pinned digest; with no digest
// to pull by, the deploy fails image_gone and the live version keeps serving.
func TestRollbackWithTheLocalImageRemovedPullsByDigest(t *testing.T) {
	g := newRRig(t)
	digest := "nginx@sha256:" + strings.Repeat("b", 64)
	g.ops.pullFn = func(_ context.Context, ref string, on func(agentops.PullProgress)) (agentops.PullResult, error) {
		g.ops.imagesGone.Store(false)
		return agentops.PullResult{ImageID: testImageID, RepoDigest: digest}, nil
	}
	g.start(false)
	g.services()
	g.liveVia()
	d2 := g.liveVia()
	g.ops.imagesGone.Store(true)
	pulls := int(g.ops.pullN.Load())
	out := g.deployOut(g.invoke(rigWrite, "services.rollback", m{"service": "p/web", "to": 1}))
	d3 := g.deployID(out.Job)
	if snap, _, _ := g.snapOf(d3); snap.RepoDigest != digest || snap.ImageID != testImageID {
		t.Fatalf("the snapshot is not pinned: %+v", snap)
	}
	if s := g.wait(out.Job); s != jobs.StateSucceeded {
		t.Fatalf("job %s: %+v", s, g.row(d3))
	}
	if int(g.ops.pullN.Load()) != pulls+1 {
		t.Errorf("pulls: %d, want %d", g.ops.pullN.Load(), pulls+1)
	}
	var pulled []string
	for _, c := range g.ops.names() {
		if strings.HasPrefix(c, "pull:") {
			pulled = append(pulled, strings.TrimPrefix(c, "pull:"))
		}
	}
	if last := pulled[len(pulled)-1]; !strings.HasSuffix(last, "@sha256:"+strings.Repeat("b", 64)) {
		t.Errorf("the rollback pulled %q, want the digest", last)
	}
	_ = d2
}

func TestRollbackWhenTheImageIsGoneFailsImageGone(t *testing.T) {
	g := newRRig(t)
	g.start(false)
	g.services()
	g.liveVia()
	d2 := g.liveVia()
	g.ops.imagesGone.Store(true)
	out := g.deployOut(g.invoke(rigWrite, "services.rollback", m{"service": "p/web", "to": 1}))
	d3 := g.deployID(out.Job)
	if s := g.wait(out.Job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	r := g.row(d3)
	if r.status != "failed" || r.code != "image_gone" || r.appSafe != AppSafeYes {
		t.Fatalf("%+v", r)
	}
	if g.activeDeploy(g.svc) != d2 {
		t.Error("the live version changed")
	}
	if g.ops.count("create") != 2 {
		t.Errorf("a container was created for a deploy whose image is gone: %v", g.ops.names())
	}
}

// Redeploy recreates a container that went missing, runs the live image without a
// pull, takes the service's current strategy, and leaves the desired image alone.
func TestRedeployRecreatesAMissingContainer(t *testing.T) {
	g := newRRig(t)
	g.start(false)
	g.services()
	d1 := g.liveVia()
	old := g.containerOf(d1)
	g.ops.mu.Lock()
	delete(g.ops.cs, old.id) // removed by hand: the service is ◆ missing
	g.ops.mu.Unlock()
	g.routes.Untrack(old.id)
	mustExec(t, g.st, `UPDATE services SET strategy = 'stop_first' WHERE id = ?`, g.svc)
	pulls := int(g.ops.pullN.Load())

	out := g.deployOut(g.invoke(rigWrite, "services.redeploy", m{"service": "p/web"}))
	d2 := g.deployID(out.Job)
	snap, kind, src := g.snapOf(d2)
	if kind != "redeploy" || src != d1 || snap.ImageID != testImageID || snap.Strategy != "stop_first" {
		t.Fatalf("kind %s source %d snapshot %+v", kind, src, snap)
	}
	if s := g.wait(out.Job); s != jobs.StateSucceeded {
		t.Fatalf("job %s: %+v", s, g.row(d2))
	}
	if c := g.containerOf(d2); c == nil || !c.running {
		t.Error("the container was not recreated")
	}
	if g.activeDeploy(g.svc) != d2 {
		t.Error("the redeploy is not live")
	}
	if int(g.ops.pullN.Load()) != pulls {
		t.Error("the image was pulled again")
	}
	// A redeploy of a service with no live version is refused.
	g2 := newRRig(t)
	g2.start(false)
	g2.services()
	_, err := g2.invoke(rigWrite, "services.redeploy", m{"service": "p/web"})
	wantKind(t, err, actions.ErrConflict)
}

// A restart keeps the container, its address and its route: container_restart with
// stop_grace_s, then the health gate; no route call at all.
func TestRestartKeepsTheAddress(t *testing.T) {
	g := newRRig(t)
	g.start(false)
	g.services()
	d1 := g.liveVia(func(s *Snapshot) { s.StopGraceS = 7 })
	c := g.containerOf(d1)
	g.expose(g.svc, 9000)
	applies := g.px.n()
	var grace int
	g.ops.restartFn = func(c *fcont, gr int) error { grace = gr; c.running = true; return nil }

	out, err := g.invoke(rigWrite, "services.restart", m{"service": "p/web"})
	if err != nil {
		t.Fatal(err)
	}
	ro := out.(RestartOut)
	if ro.Status != "queued" || ro.Job < 1 {
		t.Fatalf("%+v", ro)
	}
	if s := g.wait(ro.Job); s != jobs.StateSucceeded {
		t.Fatalf("job %s", s)
	}
	if grace != 7 || g.ops.count("restart") != 1 {
		t.Errorf("grace %d, restarts %d", grace, g.ops.count("restart"))
	}
	again := g.containerOf(d1)
	if again == nil || again.id != c.id || again.ip != "10.250.0.2" || !again.running {
		t.Errorf("the container changed: %+v", again)
	}
	if g.px.n() != applies {
		t.Errorf("a restart touched the routes: %d apply calls, was %d", g.px.n(), applies)
	}
	if g.activeDeploy(g.svc) != d1 {
		t.Error("the active deploy changed")
	}
	if st, ok := g.routes.State().Get(c.id); !ok || st.State != StateRunning || st.IP != "10.250.0.2" {
		t.Errorf("state %+v", st)
	}
	if g.ops.count("create") != 1 || g.ops.count("remove") != 0 {
		t.Errorf("a restart recreated or removed a container: %v", g.ops.names())
	}
}

// A restart never claims success before the app is healthy again.
func TestRestartFailsWhenTheAppDoesNotComeBack(t *testing.T) {
	g := newRRig(t)
	g.start(false)
	g.services()
	d1 := g.liveVia()
	c := g.containerOf(d1)
	g.ops.restartFn = func(c *fcont, _ int) error { c.running, c.exit = false, 3; return nil }
	out, err := g.invoke(rigWrite, "services.restart", m{"service": "p/web"})
	if err != nil {
		t.Fatal(err)
	}
	job := out.(RestartOut).Job
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	var msg string
	g.st.R.QueryRow(`SELECT error FROM jobs WHERE id = ?`, job).Scan(&msg)
	if !strings.Contains(msg, "not healthy") || !strings.Contains(msg, "exited with code 3") {
		t.Errorf("error %q", msg)
	}
	if st, _ := g.routes.State().Get(c.id); st.State == StateRunning {
		t.Errorf("the state map says it runs: %+v", st)
	}
}

func TestRestartRefusesWithoutALiveVersionOrContainer(t *testing.T) {
	g := newRRig(t)
	g.start(false)
	g.services()
	_, err := g.invoke(rigWrite, "services.restart", m{"service": "p/web"})
	wantKind(t, err, actions.ErrConflict)
	d1 := g.liveVia()
	gone := g.containerOf(d1).id
	g.ops.mu.Lock()
	delete(g.ops.cs, gone)
	g.ops.mu.Unlock()
	out, err := g.invoke(rigWrite, "services.restart", m{"service": "p/web"})
	if err != nil {
		t.Fatal(err)
	}
	job := out.(RestartOut).Job
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	var msg string
	g.st.R.QueryRow(`SELECT error FROM jobs WHERE id = ?`, job).Scan(&msg)
	if !strings.Contains(msg, "Redeploy") {
		t.Errorf("error %q", msg)
	}
	if g.ops.count("restart") != 0 {
		t.Error("restarted a container that is not there")
	}
}

// Restart, redeploy and rollback are write-scope actions; the run actions are
// JobOnly; a restart runs as the actor that queued it.
func TestRestartRedeployRollbackScopesAndJobOnly(t *testing.T) {
	g := newRRig(t)
	g.start(false)
	g.services()
	g.liveVia()
	for _, a := range []struct {
		name string
		in   any
	}{{"services.restart", m{"service": "p/web"}}, {"services.redeploy", m{"service": "p/web"}}, {"services.rollback", m{"service": "p/web", "to": 1}}} {
		if _, err := g.invoke(rigRead, a.name, a.in); !errors.Is(err, actions.ErrDenied) {
			t.Errorf("%s as a read token: %v", a.name, err)
		}
	}
	admin := actions.UserSessionActor("1", "sess-a", actions.ScopeAdmin, time.Now().Add(time.Hour))
	if _, err := g.reg.Invoke(context.Background(), admin, "jobs.run_restart", []byte(`{"job":1}`), actions.Options{}); !errors.Is(err, actions.ErrDenied) {
		t.Errorf("a direct jobs.run_restart: %v", err)
	}
	// The restart ran as the actor that queued it: its audit row carries the token.
	out, err := g.invoke(rigWrite, "services.restart", m{"service": "p/web"})
	if err != nil {
		t.Fatal(err)
	}
	g.wait(out.(RestartOut).Job)
	if n := g.auditRows(`action = 'jobs.run_restart' AND actor_kind = 'token' AND executor = 'system'`); n < 1 {
		t.Errorf("no jobs.run_restart audit row as the token with executor system")
	}
}

// None of them touches the routes beyond the one table swap of a switch, and none
// asks for a retry of failed ports.
func TestRedeployRollbackAndRestartCauseNoReloadsOnTheirOwn(t *testing.T) {
	g := newRRig(t)
	g.start(false)
	g.services()
	g.liveVia()
	g.liveVia()
	g.expose(g.svc, 9000)
	base := g.px.n()
	out := g.deployOut(g.invoke(rigWrite, "services.redeploy", m{"service": "p/web"}))
	g.wait(out.Job)
	out = g.deployOut(g.invoke(rigWrite, "services.rollback", m{"service": "p/web", "to": 1}))
	g.wait(out.Job)
	ro, _ := g.invoke(rigWrite, "services.restart", m{"service": "p/web"})
	g.wait(ro.(RestartOut).Job)
	if got := g.px.n() - base; got != 2 {
		t.Errorf("%d route applies for a redeploy and a rollback (one switch each) and a restart (none), want 2", got)
	}
	g.px.mu.Lock()
	defer g.px.mu.Unlock()
	for i, r := range g.px.retries {
		if r {
			t.Errorf("apply %d asked for a retry of failed ports", i)
		}
	}
}

// A cancel in the middle of container_restart does not leave the app dying (the
// restart runs to its end on its own context), and a good restart clears a stale
// previous_version_down flag.
func TestCancelDuringARestartDoesNotCutItAndASuccessfulRestartClearsTheFlag(t *testing.T) {
	g := newRRig(t)
	g.start(false)
	g.services()
	d1 := g.liveVia()
	mustExec(t, g.st, `UPDATE services SET needs_attention = ? WHERE id = ?`, AttentionPreviousDown, g.svc)
	jobCh := make(chan int64, 1)
	g.ops.restartFn = func(c *fcont, _ int) error {
		c.running = true
		j := <-jobCh
		_ = g.sched.Cancel(context.Background(), j, "cancelled by the user")
		return nil
	}
	out, err := g.invoke(rigWrite, "services.restart", m{"service": "p/web"})
	if err != nil {
		t.Fatal(err)
	}
	job := out.(RestartOut).Job
	jobCh <- job
	if s := g.wait(job); s != jobs.StateCancelled {
		t.Fatalf("job %s", s)
	}
	if c := g.containerOf(d1); c == nil || !c.running {
		t.Error("the container is not running")
	}
	g.ops.mu.Lock()
	if len(g.ops.restartAfter) != 1 || g.ops.restartAfter[0] != nil {
		t.Errorf("the restart ran on the cancellable context: %v", g.ops.restartAfter)
	}
	g.ops.mu.Unlock()
	// A restart that is not cancelled clears the flag.
	g.ops.mu.Lock()
	g.ops.restartFn = nil
	g.ops.mu.Unlock()
	out, err = g.invoke(rigWrite, "services.restart", m{"service": "p/web"})
	if err != nil {
		t.Fatal(err)
	}
	if s := g.wait(out.(RestartOut).Job); s != jobs.StateSucceeded {
		t.Fatalf("job %s", s)
	}
	var attention *string
	g.st.R.QueryRow(`SELECT needs_attention FROM services WHERE id = ?`, g.svc).Scan(&attention)
	if attention != nil {
		t.Errorf("needs_attention %q after a good restart", *attention)
	}
}

// A write token cannot roll back to a version from another repository, or to one
// with another container port on an exposed service; an admin token can.
func TestRollbackNeedsAdminForAnotherRepositoryOrPort(t *testing.T) {
	g := newRRig(t)
	g.start(false)
	g.services()
	adminTok := actions.TokenActor("3", actions.ScopeAdmin)
	g.liveVia(func(s *Snapshot) { s.ImageRef = "redis:7" })                            // #1: a repository since retired
	g.liveVia(func(s *Snapshot) { s.ContainerPort = 9090; s.Health.Kind = "running" }) // #2: another port
	g.liveVia()                                                                        // #3: the current one
	mustExec(t, g.st, `UPDATE services SET public_port = 9000, container_port = 8080 WHERE id = ?`, g.svc)

	_, err := g.invoke(rigWrite, "services.rollback", m{"service": "p/web", "to": 1})
	wantKind(t, err, actions.ErrDenied)
	if !strings.Contains(err.Error(), "another image repository") {
		t.Errorf("%v", err)
	}
	_, err = g.invoke(rigWrite, "services.rollback", m{"service": "p/web", "to": 2})
	wantKind(t, err, actions.ErrDenied)
	if !strings.Contains(err.Error(), "container port") {
		t.Errorf("%v", err)
	}
	if _, err := g.invoke(adminTok, "services.rollback", m{"service": "p/web", "to": 1}); err != nil {
		t.Errorf("an admin token: %v", err)
	}
}
