package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// adminSession is a signed-in admin (its session row exists, as enqueue requires)
// whose fresh-2FA window ends at fresh.
func (g *rrig) adminSession(public string, fresh time.Time) actions.Actor {
	g.t.Helper()
	mustExec(g.t, g.st, `INSERT OR IGNORE INTO users(id, username, scope, created_at) VALUES (1, 'admin', 'admin', 1)`)
	mustExec(g.t, g.st, `INSERT INTO sessions(id_hash, public_id, user_id, created_at, last_seen_at, idle_expires_at, abs_expires_at, mfa_fresh_until, ip_key, user_agent)
		VALUES (?, ?, 1, 1, 1, 9999999999, 9999999999, 0, 'ip', 'ua')`, []byte(public), public)
	return actions.UserSessionActor("1", public, actions.ScopeAdmin, fresh)
}

func (g *rrig) deleteAs(a actions.Actor, ref, confirm string) (DeleteOut, error) {
	g.t.Helper()
	raw, _ := json.Marshal(map[string]string{"service": ref})
	res, err := g.reg.Invoke(context.Background(), a, "services.delete", raw, actions.Options{Confirm: confirm})
	if err != nil {
		return DeleteOut{}, err
	}
	return res.Out.(DeleteOut), nil
}

func (g *rrig) rows(q string, args ...any) int {
	g.t.Helper()
	var n int
	if err := g.st.R.QueryRow(q, args...).Scan(&n); err != nil {
		g.t.Fatal(err)
	}
	return n
}

func (g *rrig) jobError(id int64) string {
	var e *string
	g.st.R.QueryRow(`SELECT error FROM jobs WHERE id = ?`, id).Scan(&e)
	if e == nil {
		return ""
	}
	return *e
}

func withLogs(root string) rigOpt { return func(c *RunnerConfig) { c.LogRoot = root } }

// Every token is denied (nothing runs), a user needs fresh 2FA and the exact Confirm.
func TestDeleteTokensAreDeniedAndAUserNeedsFresh2FAAndTheExactConfirm(t *testing.T) {
	g := newRRig(t)
	g.liveViaStore()
	g.start(true)
	g.services()
	before := g.rows(`SELECT COUNT(*) FROM jobs`)
	for _, tok := range []actions.Actor{rigRead, rigWrite, actions.TokenActor("3", actions.ScopeAdmin)} {
		_, err := g.deleteAs(tok, "p/web", "p/web")
		wantKind(t, err, actions.ErrDenied)
	}
	stale := g.adminSession("sess-stale", time.Now().Add(-time.Minute))
	_, err := g.deleteAs(stale, "p/web", "p/web")
	var de *actions.DeniedError
	if !errors.As(err, &de) || de.Code != "mfa_required" {
		t.Fatalf("a stale user: %v", err)
	}
	fresh := g.adminSession("sess-fresh", time.Now().Add(time.Minute))
	for _, wrong := range []string{"", "web", "p/WEB-", "p/web "} {
		_, err = g.deleteAs(fresh, "p/web", wrong)
		if !errors.As(err, &de) || de.Code != "confirm" {
			t.Fatalf("confirm %q: %v", wrong, err)
		}
	}
	if g.rows(`SELECT COUNT(*) FROM jobs`) != before || g.rows(`SELECT COUNT(*) FROM services`) != 1 {
		t.Error("a refused delete changed something")
	}
	out, err := g.deleteAs(fresh, "p/web", "p/web")
	if err != nil || out.Status != "queued" || out.Job < 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if k := g.rows(`SELECT COUNT(*) FROM jobs WHERE id = ? AND kind = 'service_delete' AND confirm = 'p/web' AND actor_session = 'sess-fresh'`, out.Job); k != 1 {
		t.Error("the delete job does not carry its actor and Confirm")
	}
	// jobs.run_service_delete is JobOnly.
	raw, _ := json.Marshal(RunDeleteIn{Job: out.Job, Service: "p/web"})
	if _, err := g.reg.Invoke(context.Background(), fresh, "jobs.run_service_delete", raw, actions.Options{Confirm: "p/web"}); !errors.Is(err, actions.ErrDenied) {
		t.Errorf("a direct jobs.run_service_delete: %v", err)
	}
}

// The delete removes this service's containers and nothing else: a look-alike with
// another instance label (the list marks it unowned) and another service's container
// survive untouched. Logs, rows, and the project with its last service go.
func TestDeleteRemovesOnlyThisServicesContainersLogsAndRows(t *testing.T) {
	root := t.TempDir()
	g := newRRig(t, withLogs(root))
	api := g.service("api")
	g.start(false)
	g.services()
	web := g.svc
	dWeb := g.liveVia()
	dApi, jobApi := g.deploy(api)
	if s := g.wait(jobApi); s != jobs.StateSucceeded {
		t.Fatalf("api: %s", s)
	}
	lookAlike := strings.Repeat("c", 64)
	g.ops.mu.Lock()
	g.ops.extra = append(g.ops.extra, agentops.ListedContainer{ID: lookAlike, Name: "sd-other", Unowned: true,
		Labels: agentops.ListedLabels{Service: strconv.FormatInt(web, 10), Deploy: strconv.FormatInt(dWeb, 10)}})
	g.ops.mu.Unlock()
	for _, id := range []int64{web, api} {
		dir := filepath.Join(root, strconv.FormatInt(id, 10))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "1.log.gz"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	webContainer, apiContainer := g.containerOf(dWeb).id, g.containerOf(dApi).id
	sess := g.adminSession("sess-d", time.Now().Add(time.Hour))

	out, err := g.deleteAs(sess, "p/web", "p/web")
	if err != nil {
		t.Fatal(err)
	}
	if s := g.wait(out.Job); s != jobs.StateSucceeded {
		t.Fatalf("delete job %s: %s", s, g.jobError(out.Job))
	}
	if g.ops.find(webContainer) != nil {
		t.Error("the service's container is still there")
	}
	if c := g.ops.find(apiContainer); c == nil || !c.running {
		t.Error("another service's container was touched")
	}
	for _, c := range g.ops.names() {
		if strings.Contains(c, "cccccccc") {
			t.Errorf("the look-alike was addressed: %s", c)
		}
	}
	if g.rows(`SELECT COUNT(*) FROM services WHERE id = ?`, web) != 0 || g.rows(`SELECT COUNT(*) FROM deploys WHERE service_id = ?`, web) != 0 ||
		g.rows(`SELECT COUNT(*) FROM deploy_stages WHERE deploy_id = ?`, dWeb) != 0 {
		t.Error("the service's rows are still there")
	}
	if g.rows(`SELECT COUNT(*) FROM projects`) != 1 || g.rows(`SELECT COUNT(*) FROM deploys WHERE service_id = ?`, api) == 0 {
		t.Error("the project or the other service's rows went with it")
	}
	if _, err := os.Stat(filepath.Join(root, strconv.FormatInt(web, 10))); !os.IsNotExist(err) {
		t.Errorf("the deploy-log directory is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, strconv.FormatInt(api, 10), "1.log.gz")); err != nil {
		t.Errorf("another service's logs were touched: %v", err)
	}
	if _, ok := g.routes.State().Get(webContainer); ok {
		t.Error("the state map still holds the container")
	}
	if g.auditRows(`action = 'jobs.run_service_delete' AND actor_kind = 'user' AND executor = 'system'`) < 1 {
		t.Error("no audit row of the run as the user with executor system")
	}

	// The last service takes its project with it.
	out, err = g.deleteAs(sess, "p/api", "p/api")
	if err != nil {
		t.Fatal(err)
	}
	if s := g.wait(out.Job); s != jobs.StateSucceeded {
		t.Fatalf("delete job %s: %s", s, g.jobError(out.Job))
	}
	if g.rows(`SELECT COUNT(*) FROM projects`) != 0 || g.rows(`SELECT COUNT(*) FROM services`) != 0 {
		t.Error("the project or the service is still there")
	}
}

// Queuing the delete cancels the service's queued work and flags its running deploy
// (before its switch it ends cancelled and its candidate goes); then the delete runs.
func TestQueuingTheDeleteCancelsQueuedWorkAndFlagsTheRunningDeploy(t *testing.T) {
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
	g.services()
	d, deployJob := g.deploy(g.svc, func(s *Snapshot) { s.Health.TimeoutS = 600 })
	<-inHealth
	rs, err := g.invoke(rigWrite, "services.restart", m{"service": "p/web"})
	if err != nil {
		t.Fatal(err)
	}
	restartJob := rs.(RestartOut).Job
	sess := g.adminSession("sess-d", time.Now().Add(time.Hour))
	out, err := g.deleteAs(sess, "p/web", "p/web")
	if err != nil {
		t.Fatal(err)
	}
	if out.Cancelled != 1 || out.Flagged != 1 {
		t.Errorf("%+v", out)
	}
	if s := g.wait(deployJob); s != jobs.StateCancelled {
		t.Fatalf("the running deploy's job is %s", s)
	}
	// (The deploy's row is gone once the delete has run: read the job's flag.)
	var reason string
	g.st.R.QueryRow(`SELECT COALESCE(cancel_reason, '') FROM jobs WHERE id = ?`, deployJob).Scan(&reason)
	if reason != jobs.ReasonDeleted {
		t.Errorf("the running deploy's job was flagged %q", reason)
	}
	_ = d
	if g.jobState(restartJob) != jobs.StateCancelled {
		t.Errorf("the queued restart is %s", g.jobState(restartJob))
	}
	if s := g.wait(out.Job); s != jobs.StateSucceeded {
		t.Fatalf("delete job %s: %s", s, g.jobError(out.Job))
	}
	if !g.noContainers() || g.rows(`SELECT COUNT(*) FROM services`) != 0 {
		t.Errorf("containers left: %v", g.ops.names())
	}
}

// A running deploy that is past its commit skips the rest of its drain and stops the
// old container with its normal grace; then the delete runs.
func TestRunningDeployPastItsCommitSkipsTheRestOfItsDrain(t *testing.T) {
	g := newRRig(t, func(c *RunnerConfig) { c.DrainUnit = time.Second })
	g.live(g.svc, 1, "10.250.0.2")
	var graces []int
	g.ops.stopGraceFn = func(grace int) { graces = append(graces, grace) }
	g.start(false)
	g.services()
	d, deployJob := g.deploy(g.svc, func(s *Snapshot) { s.StopGraceS = 7 }) // a 10 s drain
	g.until("the drain", func() bool { return g.row(d).stage == StageDrain })
	t0 := time.Now()
	sess := g.adminSession("sess-d", time.Now().Add(time.Hour))
	out, err := g.deleteAs(sess, "p/web", "p/web")
	if err != nil {
		t.Fatal(err)
	}
	if s := g.wait(deployJob); s != jobs.StateSucceeded {
		t.Fatalf("deploy job %s", s)
	}
	if took := time.Since(t0); took > 4*time.Second {
		t.Errorf("the drain was not cut short: %v after the delete was queued", took)
	}
	r := g.row(d)
	if r.status != "succeeded" || !strings.Contains(r.reason, "cut short") {
		t.Errorf("%+v", r)
	}
	if s := g.wait(out.Job); s != jobs.StateSucceeded {
		t.Fatalf("delete job %s: %s", s, g.jobError(out.Job))
	}
	// The old container was stopped with the deploy's grace (7), then the delete stopped the new one (service grace).
	if len(graces) != 2 || graces[0] != 7 {
		t.Errorf("stop graces %v, want the old container's 7 first", graces)
	}
	if !g.noContainers() || g.rows(`SELECT COUNT(*) FROM services`) != 0 {
		t.Errorf("containers left: %v", g.ops.names())
	}
}

// The delete waited past the request's 5-minute fresh-2FA window (a long stop grace
// of the deploy it cut short): the run-time policy check refuses it, the job fails
// with "confirm again", and nothing is deleted.
func TestDeleteAfterTheFreshWindowExpiresAsksToConfirmAgain(t *testing.T) {
	g := newRRig(t, func(c *RunnerConfig) { c.DrainUnit = time.Second })
	var offset atomic.Int64
	g.clk = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	g.live(g.svc, 1, "10.250.0.2")
	g.ops.stopGraceFn = func(int) { offset.Store(int64(6 * time.Minute)) } // the old container's stop takes "6 minutes"
	g.start(false)
	g.services()
	d, deployJob := g.deploy(g.svc, func(s *Snapshot) { s.StopGraceS = 300 })
	g.until("the drain", func() bool { return g.row(d).stage == StageDrain })
	sess := g.adminSession("sess-d", g.clk().Add(5*time.Minute)) // the window of the request
	out, err := g.deleteAs(sess, "p/web", "p/web")
	if err != nil {
		t.Fatal(err)
	}
	g.wait(deployJob)
	if s := g.wait(out.Job); s != jobs.StateFailed {
		t.Fatalf("delete job %s", s)
	}
	if got := g.jobError(out.Job); got != ConfirmAgainMessage {
		t.Errorf("job error %q", got)
	}
	if g.rows(`SELECT COUNT(*) FROM services`) != 1 || g.containerOf(d) == nil || !g.containerOf(d).running {
		t.Error("something was deleted although the delete was refused")
	}
	var attention *string
	g.st.R.QueryRow(`SELECT needs_attention FROM services WHERE id = ?`, g.svc).Scan(&attention)
	if attention != nil {
		t.Errorf("needs_attention %q after a refusal that changed nothing", *attention)
	}
	if g.auditRows(`action = 'jobs.run_service_delete' AND phase = 'denied' AND executor = 'system'`) < 1 {
		t.Error("the refusal was not audited")
	}
	// Delete again, with a fresh window: it goes through.
	offset.Store(0)
	g.clk = func() time.Time { return time.Now() }
	again := g.adminSession("sess-e", time.Now().Add(time.Hour))
	out2, err := g.deleteAs(again, "p/web", "p/web")
	if err != nil {
		t.Fatal(err)
	}
	if s := g.wait(out2.Job); s != jobs.StateSucceeded {
		t.Fatalf("second delete %s: %s", s, g.jobError(out2.Job))
	}
}

// Deleting an exposed service: the "not running" page first (a table swap), then one
// route list with the port gone (the proxy reloads on that: a documented reload case).
func TestDeleteOfAnExposedServiceRemovesItsRouteInOneApply(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	g.expose(g.svc, 9000)
	g.start(false)
	g.services()
	before := g.px.n()
	out, err := g.deleteAs(g.adminSession("sess-d", time.Now().Add(time.Hour)), "p/web", "p/web")
	if err != nil {
		t.Fatal(err)
	}
	if s := g.wait(out.Job); s != jobs.StateSucceeded {
		t.Fatalf("delete job %s: %s", s, g.jobError(out.Job))
	}
	if g.px.n() != before+2 || len(g.lastRoutes()) != 0 {
		t.Errorf("%d applies after the delete (want 2: the page, then the port gone), last routes %+v", g.px.n()-before, g.lastRoutes())
	}
	if first := g.routeLists()[before]; len(first) != 1 || first[0].Page != agentops.PageNotRunning {
		t.Errorf("the first apply of the delete is %+v, want the not-running page", first)
	}
	g.px.mu.Lock()
	defer g.px.mu.Unlock()
	if g.px.retries[len(g.px.retries)-1] {
		t.Error("the delete asked the proxy to retry failed ports")
	}
}

// A container that cannot be removed fails the job, keeps the rows and flags the
// service; deleting again finishes (every step is idempotent).
func TestDeleteThatCannotRemoveAContainerFlagsTheServiceAndRunsAgain(t *testing.T) {
	g := newRRig(t)
	d := g.live(g.svc, 1, "10.250.0.2")
	g.start(false)
	g.services()
	sess := g.adminSession("sess-d", time.Now().Add(time.Hour))
	g.ops.mu.Lock()
	g.ops.removeErr = opErr("docker", "boom")
	g.ops.mu.Unlock()
	out, _ := g.deleteAs(sess, "p/web", "p/web")
	if s := g.wait(out.Job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	if !strings.Contains(g.jobError(out.Job), "could not be removed") {
		t.Errorf("error %q", g.jobError(out.Job))
	}
	var attention string
	g.st.R.QueryRow(`SELECT needs_attention FROM services WHERE id = ?`, g.svc).Scan(&attention)
	if attention != AttentionDeleteInterrupted || g.rows(`SELECT COUNT(*) FROM services`) != 1 {
		t.Errorf("attention %q", attention)
	}
	// A later-step failure also takes the not-running override away again.
	if g.routes.overrideCount() != 0 {
		t.Error("a failed delete left its not-running override behind")
	}
	_ = d
	g.ops.mu.Lock()
	g.ops.removeErr = nil
	g.ops.mu.Unlock()
	out, err := g.deleteAs(sess, "p/web", "p/web")
	if err != nil {
		t.Fatal(err)
	}
	if s := g.wait(out.Job); s != jobs.StateSucceeded {
		t.Fatalf("job %s: %s", s, g.jobError(out.Job))
	}
	if g.rows(`SELECT COUNT(*) FROM services`) != 0 {
		t.Error("the service is still there")
	}
}

// A delete cut off by a restart is flagged for attention by the boot pass.
func TestInterruptedDeleteNeedsAttention(t *testing.T) {
	g := newRRig(t)
	sess := g.adminSession("sess-d", time.Now().Add(time.Hour))
	out, err := g.js.EnqueueDelete(context.Background(), jobs.NewDelete{ServiceID: g.svc, Target: "p/web", Actor: jobs.ActorOf(sess), StopGraceS: 10})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, g.st, `UPDATE jobs SET state = 'running', boot_id = 'old', started_at = 'x' WHERE id = ?`, out.JobID)
	if n, err := ResolveInterrupted(context.Background(), g.st, "new", time.Now()); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	var attention string
	g.st.R.QueryRow(`SELECT needs_attention FROM services WHERE id = ?`, g.svc).Scan(&attention)
	if attention != AttentionDeleteInterrupted {
		t.Errorf("needs_attention %q", attention)
	}
	if g.jobState(out.JobID) != jobs.StateFailed {
		t.Errorf("job %s", g.jobState(out.JobID))
	}
}

// While a delete is pending nothing new is queued for the service, and it cannot be
// deleted twice.
func TestWorkIsRefusedWhileADeleteIsPending(t *testing.T) {
	g := newRRig(t)
	g.liveViaStore()
	g.start(true) // the gate stays closed: the delete stays queued
	g.services()
	sess := g.adminSession("sess-d", time.Now().Add(time.Hour))
	if _, err := g.deleteAs(sess, "p/web", "p/web"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"services.deploy", "services.redeploy", "services.restart"} {
		if _, err := g.invoke(rigWrite, name, m{"service": "p/web"}); !errors.Is(err, actions.ErrConflict) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := g.invoke(rigWrite, "services.rollback", m{"service": "p/web", "to": 1}); !errors.Is(err, actions.ErrConflict) && !errors.Is(err, actions.ErrInvalidInput) {
		t.Errorf("rollback: %v", err)
	}
	if _, err := g.deleteAs(sess, "p/web", "p/web"); !errors.Is(err, actions.ErrConflict) {
		t.Errorf("a second delete: %v", err)
	}
}

// liveViaStore makes a live deploy without running one (a committed row).
func (g *rrig) liveViaStore() { g.live(g.svc, 1, "10.250.0.2") }

// The deploy-log path is validated: an absolute root, a positive id, one level
// under the root; a symlink in its place is removed, never followed.
func TestDeleteLogDirIsValidated(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []struct {
		root string
		id   int64
	}{{"", 1}, {"relative/logs", 1}, {root, 0}, {root, -3}} {
		if _, err := safeLogDir(bad.root, bad.id); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	dir, err := safeLogDir(root+"/../"+filepath.Base(root)+"/", 42)
	if err != nil || dir != filepath.Join(root, "42") {
		t.Errorf("%q %v", dir, err)
	}
	// A symlink where the directory should be: the link goes, its target stays.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "7")); err != nil {
		t.Fatal(err)
	}
	if err := removeLogs(root, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("the target of a symlink was deleted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "7")); !os.IsNotExist(err) {
		t.Errorf("the link is still there: %v", err)
	}
	// A missing directory and an empty root are fine (nothing to delete).
	// A symlink INSIDE the directory that points outside is not followed either.
	inner := filepath.Join(root, "8")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(inner, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := removeLogs(root, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("a symlink inside the directory was followed: %v", err)
	}
	if _, err := os.Lstat(inner); !os.IsNotExist(err) {
		t.Errorf("the directory is still there: %v", err)
	}
	if err := removeLogs(root, 99); err != nil {
		t.Error(err)
	}
	if err := removeLogs("", 99); err != nil {
		t.Error(err)
	}
}

// The route is taken away FIRST, and the proxy must confirm it: with a proxy that
// cannot apply, the delete fails before any container is touched, the service is
// flagged, and an address of the service can never be handed out while a stale proxy
// might still route to it.
func TestDeleteTakesTheRouteAwayFirstAndNeedsItConfirmed(t *testing.T) {
	g := newRRig(t)
	d := g.live(g.svc, 1, "10.250.0.2")
	g.expose(g.svc, 9000)
	g.start(false)
	g.services()
	sess := g.adminSession("sess-d", time.Now().Add(time.Hour))

	// 1. The proxy fails the "not running" page: nothing is removed.
	g.px.failIf = func(routes []agentops.Route) error {
		if len(routes) == 1 && routes[0].Page == agentops.PageNotRunning {
			return errors.New("proxy down")
		}
		return nil
	}
	out, err := g.deleteAs(sess, "p/web", "p/web")
	if err != nil {
		t.Fatal(err)
	}
	if s := g.wait(out.Job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	if !strings.Contains(g.jobError(out.Job), "nothing was deleted") {
		t.Errorf("error %q", g.jobError(out.Job))
	}
	if g.ops.count("stop") != 0 || g.ops.count("remove") != 0 || g.containerOf(d) == nil {
		t.Errorf("a container was touched before the route was confirmed gone: %v", g.ops.names())
	}
	var attention string
	g.st.R.QueryRow(`SELECT needs_attention FROM services WHERE id = ?`, g.svc).Scan(&attention)
	if attention != AttentionDeleteInterrupted || g.rows(`SELECT COUNT(*) FROM services`) != 1 {
		t.Errorf("attention %q", attention)
	}
	// The override of the failed delete is gone and the route is the old upstream again.
	if g.routes.overrideCount() != 0 {
		t.Error("a failed delete left its not-running override behind")
	}
	if last := g.lastRoutes(); len(last) != 1 || last[0].Upstream != "10.250.0.2:8080" {
		t.Errorf("the route after the failed delete: %+v", last)
	}
	// The container still exists, so its address stays held: a new deploy never gets it.
	extra := addDeploy(t, g.st, g.svc, 9, "queued", nil)
	addr, err := g.runner.cfg.Alloc.Allocate(context.Background(), extra)
	if err != nil || addr.String() == "10.250.0.2" {
		t.Errorf("Allocate gave %v %v while the deleted service's address may still be routed", addr, err)
	}
	mustExec(t, g.st, `UPDATE deploys SET status = 'failed', ip = NULL WHERE id = ?`, extra)

	// 2. A proxy that works: the first route list after the delete is the page, before any stop.
	g.px.mu.Lock()
	g.px.failIf = nil
	g.px.mu.Unlock()
	var order []string
	g.px.onApply = func(routes []agentops.Route) {
		if len(routes) == 1 && routes[0].Page == agentops.PageNotRunning {
			order = append(order, "page")
		}
	}
	g.ops.onCall = func(op, _ string) {
		if op == "stop" || op == "remove" {
			order = append(order, op)
		}
	}
	out, err = g.deleteAs(sess, "p/web", "p/web")
	if err != nil {
		t.Fatal(err)
	}
	if s := g.wait(out.Job); s != jobs.StateSucceeded {
		t.Fatalf("job %s: %s", s, g.jobError(out.Job))
	}
	if len(order) < 3 || order[0] != "page" {
		t.Errorf("order %v: the route page must come before any container call", order)
	}
	if g.routes.overrideCount() != 0 {
		t.Error("an override is left behind after the delete")
	}
}

// A session revoke (the job's context cancelled) in the middle of a delete does not
// leave it half done: it runs on to its end.
func TestRevokeDuringADeleteNeverLeavesItHalfDone(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	g.start(false)
	g.services()
	sess := g.adminSession("sess-d", time.Now().Add(time.Hour))
	jobCh := make(chan int64, 1)
	g.ops.stopGraceFn = func(int) {
		select {
		case j := <-jobCh:
			jobCh <- j
			_ = g.sched.Cancel(context.Background(), j, "the session that queued this job was revoked")
		case <-time.After(2 * time.Second):
		}
	}
	out, err := g.deleteAs(sess, "p/web", "p/web")
	if err != nil {
		t.Fatal(err)
	}
	jobCh <- out.Job
	g.wait(out.Job)
	gone := g.rows(`SELECT COUNT(*) FROM services`) == 0 && g.noContainers()
	var attention *string
	g.st.R.QueryRow(`SELECT needs_attention FROM services`).Scan(&attention)
	if !gone && (attention == nil || *attention != AttentionDeleteInterrupted) {
		t.Fatalf("half done and unflagged: services %d, containers %v, job %s", g.rows(`SELECT COUNT(*) FROM services`), g.ops.names(), g.jobError(out.Job))
	}
	if !gone {
		t.Errorf("the delete did not run to its end after the revoke: %s", g.jobError(out.Job))
	}
}
