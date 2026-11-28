package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

func (g *rrig) auditRows(where string, args ...any) int {
	g.t.Helper()
	var n int
	if err := g.st.R.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE `+where, args...).Scan(&n); err != nil {
		g.t.Fatal(err)
	}
	return n
}

// ipFree proves a deploy's address is free: the unique index over live deploys
// takes a new running deploy with that address.
func (g *rrig) ipFree(ip string) bool {
	g.t.Helper()
	_, err := g.st.W.Exec(`INSERT INTO deploys(service_id, number, kind, snapshot_json, ip, status, actor_kind, actor_id, created_at)
		VALUES (?, 900, 'deploy', '{}', ?, 'running', 'user', '1', 'x')`, g.svc, ip)
	if err == nil {
		_, _ = g.st.W.Exec(`DELETE FROM deploys WHERE number = 900 AND service_id = ?`, g.svc)
	}
	return err == nil
}

// A job refused at run time (its token revoked or expired, a credential changed, its
// scope narrowed to read) never runs, on its first attempt or on a retry attempt, and
// its deploy row ends "denied" with the fixed reason: it is not left queued or
// running. The refusal leaves exactly one audit row (a denied row with the job's
// actor and executor = system).
func TestDeniedAtRunTimeEndsTheDeployRowOnEveryAttempt(t *testing.T) {
	deny := func(reason string) func(*fakeRefresh) {
		return func(f *fakeRefresh) { f.err = &actions.DeniedError{Reason: reason} }
	}
	cases := map[string]func(*fakeRefresh){
		"revoked token":     deny("the API token that queued this job was revoked or has expired"),
		"expired token":     deny("the API token that queued this job was revoked or has expired"),
		"credential change": deny("the password or authenticator changed after this job was queued"),
		"scope narrowed":    func(f *fakeRefresh) { f.scope = actions.ScopeRead },
	}
	for name, setup := range cases {
		for _, attempt := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/attempt %d", name, attempt), func(t *testing.T) {
				g := newRRig(t)
				setup(g.refresh)
				if attempt == 2 {
					g.refresh.errFrom, g.refresh.scopeFrom = 2, 2
					var n atomic.Int32
					g.ops.pullFn = func(context.Context, string, func(agentops.PullProgress)) (agentops.PullResult, error) {
						if n.Add(1) == 1 {
							return agentops.PullResult{}, opErr(agent.CodeDocker, "toomanyrequests: 429")
						}
						return agentops.PullResult{ImageID: testImageID}, nil
					}
				}
				g.start(false)
				d, job := g.deploy(g.svc)
				if s := g.wait(job); s != jobs.StateFailed {
					t.Fatalf("job %s", s)
				}
				r := g.row(d)
				if r.status != "failed" || r.code != "denied" || r.reason != jobs.DeniedReason {
					t.Fatalf("deploy row after a denial: %+v", r)
				}
				if got := g.auditRows(`action = 'jobs.run_deploy' AND phase = 'denied' AND executor = 'system' AND actor_kind = 'token' AND actor_id = '1'`); got != 1 {
					t.Errorf("%d denied audit rows, want exactly 1", got)
				}
				if attempt == 1 && (g.ops.count("pull") != 0 || g.auditRows(`action = 'jobs.run_deploy' AND phase = 'intent'`) != 0) {
					t.Error("a refused job ran")
				}
				if attempt == 2 && g.ops.count("pull") != 1 {
					t.Errorf("pulls %d: the retry attempt ran", g.ops.count("pull"))
				}
				for s, st := range g.stages(d) {
					if st == StageRunning || st == StagePending {
						t.Errorf("stage %s is %s on an ended deploy", s, st)
					}
				}
			})
		}
	}
}

// A transient failure whose retry loses the race against a cancel that landed while
// the runner was finishing: the job ends cancelled, and the deploy row ends with
// it (not "running, waiting to retry"), freeing its address.
func TestTransientFailurePlusCancelDuringFinishLeavesAFinalRow(t *testing.T) {
	g := newRRig(t)
	g.ops.pullFn = func(context.Context, string, func(agentops.PullProgress)) (agentops.PullResult, error) {
		// The address is held, and the cancel is recorded in the row (not yet in
		// memory): the runner decides to retry; the scheduler's requeue refuses.
		mustExecIgnore(g, `UPDATE deploys SET ip = '10.250.0.9'`)
		mustExecIgnore(g, `UPDATE jobs SET cancel_requested = 1, cancel_reason = 'cancelled by the user'`)
		return agentops.PullResult{}, opErr(agent.CodeDocker, "toomanyrequests: 429")
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateCancelled {
		t.Fatalf("job %s", s)
	}
	r := g.row(d)
	if r.status != "cancelled" || r.code != "cancelled" || !strings.Contains(r.reason, "cancelled by the user") {
		t.Fatalf("%+v", r)
	}
	if !g.ipFree("10.250.0.9") {
		t.Error("the ended deploy still holds its address")
	}
}

func mustExecIgnore(g *rrig, q string) {
	// The row is written by the test's own connection while the runner runs.
	if _, err := g.st.W.Exec(q); err != nil {
		g.t.Error(err)
	}
}

// A panic in a stage removes the candidate on a fresh context and ends the row.
func TestPanicInAStageRemovesTheCandidateAndEndsTheRow(t *testing.T) {
	g := newRRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	g.runner.trace = func(ev string) { // after the candidate was started and healthy, before any route changed
		if ev == "switch" {
			panic("boom")
		}
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	r := g.row(d)
	if r.status != "failed" || r.code != CodeInternal || strings.Contains(r.reason, "boom") {
		t.Fatalf("%+v", r)
	}
	if g.containerOf(d) != nil {
		t.Error("the candidate was left running")
	}
	for _, e := range g.ops.ctxErrs["remove"] {
		if e != nil {
			t.Errorf("the clean-up used a dead context: %v", e)
		}
	}
	// Panic earlier (inside the pull): no candidate yet, the row still ends.
	g2 := newRRig(t)
	g2.ops.pullFn = func(context.Context, string, func(agentops.PullProgress)) (agentops.PullResult, error) { panic("boom") }
	g2.start(false)
	d2, job2 := g2.deploy(g2.svc)
	g2.wait(job2)
	if r := g2.row(d2); r.status != "failed" || r.code != CodeInternal {
		t.Fatalf("%+v", r)
	}
}

// recordFinal failing (its transaction rolls back) still leaves a final row: the
// job's own end ends the deploy (the queue's backstop).
func TestRecordFinalFailureStillLeavesAFinalRow(t *testing.T) {
	g := newRRig(t)
	g.ops.pullFn = func(context.Context, string, func(agentops.PullProgress)) (agentops.PullResult, error) {
		return agentops.PullResult{}, opErr(agent.CodeDocker, "manifest unknown")
	}
	// The runner's end-of-deploy write names the failed stage with this text; the
	// queue's backstop writes another one.
	mustExec(t, g.st, `CREATE TRIGGER fail_final BEFORE UPDATE ON deploy_stages WHEN NEW.detail LIKE 'the image could not be pulled%' BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateFailed {
		t.Fatalf("job %s", s)
	}
	r := g.row(d)
	if r.status != "failed" || r.code != "job_failed" || !strings.Contains(r.reason, "manifest unknown") || r.appSafe == "" {
		t.Fatalf("%+v", r)
	}
	for s, st := range g.stages(d) {
		if st == StageRunning || st == StagePending {
			t.Errorf("stage %s is %s", s, st)
		}
	}
}

// Every way a stage's context can end is the job's doing, not Docker's: none of
// them leaves the service "unknown". (The real hub returns context.Cause.)
func TestContextEndingCausesNeverMarkTheServiceUnknown(t *testing.T) {
	type tc struct {
		name  string
		cfg   func(*RunnerConfig)
		block func(g *rrig) func(ctx context.Context, _ string, _ func(agentops.PullProgress)) (agentops.PullResult, error)
		end   func(g *rrig, job int64)
	}
	hang := func(g *rrig) func(ctx context.Context, _ string, _ func(agentops.PullProgress)) (agentops.PullResult, error) {
		return func(ctx context.Context, _ string, _ func(agentops.PullProgress)) (agentops.PullResult, error) {
			<-ctx.Done()
			return agentops.PullResult{}, context.Cause(ctx)
		}
	}
	for _, c := range []tc{
		{"cancel", nil, hang, func(g *rrig, job int64) { _ = g.sched.Cancel(context.Background(), job, "cancelled by the user") }},
		{"supersede", nil, hang, func(g *rrig, job int64) { g.deploy(g.svc) }},
		{"shutdown", nil, hang, func(g *rrig, job int64) { g.stop() }},
		{"pre-switch bound", func(c *RunnerConfig) { c.PreSwitchBound = 50 * time.Millisecond }, hang, func(*rrig, int64) {}},
		{"watchdog", func(c *RunnerConfig) { c.Watchdog = 50 * time.Millisecond }, hang, func(*rrig, int64) {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var opts []rigOpt
			if c.cfg != nil {
				opts = append(opts, c.cfg)
			}
			g := newRRig(t, opts...)
			g.live(g.svc, 1, "10.250.0.2")
			entered := make(chan struct{}, 4)
			inner := c.block(g)
			g.ops.pullFn = func(ctx context.Context, ref string, on func(agentops.PullProgress)) (agentops.PullResult, error) {
				select {
				case entered <- struct{}{}:
				default:
				}
				return inner(ctx, ref, on)
			}
			g.start(false)
			d, job := g.deploy(g.svc)
			<-entered
			c.end(g, job)
			if c.name != "shutdown" {
				g.wait(job)
				g.until("the row to end", func() bool { s := g.row(d).status; return s != "queued" && s != "running" })
			} else {
				g.until("the row to end", func() bool { return g.row(d).status == "failed" })
			}
			if g.routes.State().Unknown(g.svc) {
				t.Errorf("%s: the service is stuck unknown", c.name)
			}
		})
	}
	// A health timeout (DeadlineExceeded of the gate's own context) too.
	g := newRRig(t)
	g.ops.healthFn = func(ctx context.Context, _ agentops.HealthArgs) (agentops.HealthResult, error) {
		return agentops.HealthResult{OK: false}, nil
	}
	g.start(false)
	d, job := g.deploy(g.svc, func(s *Snapshot) { s.Health.TimeoutS = 5 })
	g.wait(job)
	if r := g.row(d); r.code != CodeHealthTimeout || g.routes.State().Unknown(g.svc) {
		t.Fatalf("%+v unknown=%v", r, g.routes.State().Unknown(g.svc))
	}
}

func TestOnlyRealDockerTroubleCountsAsTrouble(t *testing.T) {
	for err, want := range map[error]bool{
		&agent.OpError{Code: agent.CodeDocker, Msg: "x"}:   true,
		&agent.OpError{Code: agent.CodeTimeout, Msg: "x"}:  true,
		fmt.Errorf("op: %w", hub.ErrOpTimeout):             true,
		ErrAgentDown:                                       true,
		&agent.OpError{Code: agent.CodePolicy, Msg: "x"}:   false,
		&agent.OpError{Code: agent.CodeNotFound, Msg: "x"}: false,
		&agent.OpError{Code: agent.CodeConflict, Msg: "x"}: false,
		jobs.CancelCause{Reason: "superseded"}:             false,
		jobs.ErrShutdown:                                   false,
		errWatchdog:                                        false,
		errPreSwitch:                                       false,
		context.DeadlineExceeded:                           false,
		context.Canceled:                                   false,
		errors.New("database is locked"):                   false,
	} {
		if got := isDockerTrouble(err); got != want {
			t.Errorf("isDockerTrouble(%v) = %v, want %v", err, got, want)
		}
	}
}

// A successful clean-up proves Docker answers: the service is no longer unknown.
func TestSuccessfulCleanUpClearsUnknown(t *testing.T) {
	g := newRRig(t)
	g.ops.startFn = func(c *fcont) { c.running, c.exit = false, 1 }
	g.ops.createFn = func(ctx context.Context, name string, spec dockerapi.CreateSpec, ip string) (agentops.CreateResult, error, bool) {
		g.routes.State().MarkUnknown(g.svc) // as an earlier failed op would have
		return agentops.CreateResult{}, nil, false
	}
	g.start(false)
	_, job := g.deploy(g.svc)
	g.wait(job)
	if g.routes.State().Unknown(g.svc) {
		t.Error("the service is unknown although the create, the inspect and the removal all succeeded")
	}
}

// A run whose deploy had already ended (the queue wrote its end state) records
// nothing: a "superseded" is not rewritten as "cancelled".
func TestARunOverAnEndedDeployWritesNothing(t *testing.T) {
	g := newRRig(t)
	d, job := g.deploy(g.svc)
	mustExec(t, g.st, `UPDATE deploys SET status = 'superseded', reason_code = 'superseded', reason = 'by the queue', superseded_by = 99 WHERE id = ?`, d)
	j, ok, err := g.js.Pick(context.Background(), "b")
	if err != nil || !ok || j.ID != job {
		t.Fatal(err)
	}
	if _, err := g.runner.runDeploy(context.Background(), actions.Call{}, job); err == nil {
		t.Error("no error for an ended deploy")
	}
	if r := g.row(d); r.status != "superseded" || r.code != "superseded" || r.reason != "by the queue" || r.supersededBy != 99 {
		t.Fatalf("the queue's end state was rewritten: %+v", r)
	}
	if g.ops.count("pull") != 0 {
		t.Error("the ended deploy ran")
	}
}

// A candidate is forgotten by the state map only after it was removed.
func TestCandidateIsUntrackedOnlyAfterItsRemovalSucceeded(t *testing.T) {
	g := newRRig(t)
	g.ops.removeErr = opErr(agent.CodeDocker, "the Docker request failed")
	var seen int
	g.ops.startFn = func(c *fcont) { c.running, c.exit = false, 1 } // the candidate fails its gate and must be removed
	g.start(false)
	_, job := g.deploy(g.svc)
	g.wait(job)
	seen = len(g.routes.State().snapshot())
	if seen != 1 {
		t.Errorf("%d containers tracked after a failed removal, want the candidate kept", seen)
	}
	if !g.routes.State().Unknown(g.svc) {
		t.Error("a failed removal (Docker trouble) must leave the service unknown")
	}
}

// One failed inspect during the gate is one failed check: the gate keeps looping.
func TestASingleInspectErrorDoesNotFailTheHealthGate(t *testing.T) {
	g := newRRig(t)
	var n atomic.Int32
	g.ops.inspectFn = func(ctx context.Context, id string) (agentops.ContainerState, error, bool) {
		// The first inspect (after start) is the runner's confirm; fail the second, the first gate check.
		if n.Add(1) == 2 {
			return agentops.ContainerState{}, opErr(agent.CodeDocker, "the Docker request failed"), true
		}
		return agentops.ContainerState{}, nil, false
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if r := g.row(d); r.status != StatusSucceeded {
		t.Fatalf("one failed inspect ended the gate: %+v", r)
	}
	if g.ops.count("health") < 1 {
		t.Error("the gate never recovered to probe")
	}
}

// app_safe is "yes" only when the state map shows the live version running.
func TestAppSafeIsUnknownWhenTheLiveVersionIsNotSeenRunning(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	g.routes.Untrack(g.containerOf(old).id) // the state map no longer shows it
	g.ops.pullFn = func(context.Context, string, func(agentops.PullProgress)) (agentops.PullResult, error) {
		return agentops.PullResult{}, opErr(agent.CodeDiskFull, "x")
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if r := g.row(d); r.appSafe != AppSafeUnknown {
		t.Fatalf("%+v", r)
	}
}

// The old version's container is never stopped, restarted or removed by a deploy
// that is superseded during its health gate.
func TestSupersededCandidateNeverTouchesTheOldContainer(t *testing.T) {
	g := newRRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	inHealth := make(chan struct{}, 1)
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		select {
		case inHealth <- struct{}{}:
		default:
		}
		return agentops.HealthResult{OK: false}, nil
	}
	g.start(false)
	_, j1 := g.deploy(g.svc, func(s *Snapshot) { s.Health.TimeoutS = 600 })
	<-inHealth
	g.ops.mu.Lock()
	g.ops.healthFn = nil
	// The newer deploy fails at its pull, so only the superseded one is under test.
	g.ops.pullFn = func(context.Context, string, func(agentops.PullProgress)) (agentops.PullResult, error) {
		return agentops.PullResult{}, opErr(agent.CodeDiskFull, "x")
	}
	g.ops.mu.Unlock()
	_, j2 := g.deploy(g.svc)
	g.wait(j1)
	g.wait(j2)
	for _, c := range g.ops.names() {
		if strings.HasSuffix(c, ":"+oldID[:8]) && !strings.HasPrefix(c, "inspect:") && !strings.HasPrefix(c, "list:") {
			t.Errorf("the old version's container was touched: %s", c)
		}
	}
	if c := g.ops.find(oldID); c == nil || !c.running {
		t.Errorf("the old container: %+v", c)
	}
}

// A job whose stored actor cannot be rebuilt (a bad blob, or one that disagrees with
// the row's kind, id or session: a tamper signal) never runs, is audited as one
// denied row with a fixed reason (nothing from the blob), and its deploy ends
// "denied" through the sentinel, not through the error's text.
func TestUnreadableActorEndsTheDeployDeniedWithOneAuditRow(t *testing.T) {
	for name, mod := range map[string]string{
		"garbage blob":    `UPDATE jobs SET actor_blob = '{"secret-looking-text'`,
		"system kind":     `UPDATE jobs SET actor_blob = '{"v":1,"k":"system","id":"x"}'`,
		"kind mismatch":   `UPDATE jobs SET actor_kind = 'user'`,
		"id mismatch":     `UPDATE jobs SET actor_id = '999'`,
		"session swapped": `UPDATE jobs SET actor_session = 'sess-other'`,
	} {
		t.Run(name, func(t *testing.T) {
			g := newRRig(t)
			g.start(true) // the gate is closed while the row is altered
			d, job := g.deploy(g.svc)
			mustExec(t, g.st, mod)
			g.sched.OpenGate()
			if s := g.wait(job); s != jobs.StateFailed {
				t.Fatalf("job %s", s)
			}
			r := g.row(d)
			if r.status != "failed" || r.code != "denied" || r.reason != jobs.DeniedReason {
				t.Fatalf("%+v", r)
			}
			if g.ops.count("pull") != 0 || g.refresh.n() != 0 {
				t.Error("a job with an unreadable actor ran or was refreshed")
			}
			if got := g.auditRows(`action = 'jobs.run_deploy' AND phase = 'denied' AND executor = 'system' AND error NOT LIKE '%secret-looking%'`); got != 1 {
				t.Errorf("%d denied audit rows, want exactly 1", got)
			}
			if got := g.auditRows(`error LIKE '%secret-looking%' OR args_json LIKE '%secret-looking%'`); got != 0 {
				t.Error("the blob's text reached the audit log")
			}
		})
	}
}

// A transient failure that loses the race against a cancel is recorded as the cancel
// (or supersede), not as the pull error.
func TestTransientFailureThatLosesToACancelReadsCancelled(t *testing.T) {
	g := newRRig(t)
	g.ops.pullFn = func(ctx context.Context, _ string, _ func(agentops.PullProgress)) (agentops.PullResult, error) {
		var job int64
		if err := g.st.R.QueryRow(`SELECT id FROM jobs LIMIT 1`).Scan(&job); err != nil {
			t.Error(err)
		}
		_ = g.sched.Cancel(context.Background(), job, "cancelled by the user")
		<-ctx.Done() // the cancel has reached the job's context; the pull still reports a transient error
		return agentops.PullResult{}, opErr(agent.CodeDocker, "toomanyrequests: 429")
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateCancelled {
		t.Fatalf("job %s", s)
	}
	if r := g.row(d); r.status != "cancelled" || r.code != CodeCancelled {
		t.Fatalf("%+v", r)
	}
}

// A panic's clean-up runs while the service lock is still held: a second deploy of
// the service cannot start its own work in between.
func TestPanicCleanUpRunsUnderTheServiceLock(t *testing.T) {
	g := newRRig(t)
	var heldAtCleanUp atomic.Bool
	g.runner.trace = func(ev string) {
		if ev == "switch" {
			panic("boom")
		}
	}
	g.ops.mu.Lock()
	g.ops.removeHook = func() {
		if unlock, ok := g.runner.cfg.Locks.TryLock(g.svc); ok {
			unlock()
		} else {
			heldAtCleanUp.Store(true)
		}
	}
	g.ops.mu.Unlock()
	g.start(false)
	_, job := g.deploy(g.svc)
	g.wait(job)
	if !heldAtCleanUp.Load() {
		t.Error("the service lock was already released when the panic's clean-up ran")
	}
}

// A candidate removed by hand during the gate fails the gate at once, with a clear
// reason, instead of waiting out the timeout as "Docker did not answer".
func TestCandidateRemovedByHandFailsTheGateFast(t *testing.T) {
	g := newRRig(t)
	var n atomic.Int32
	g.ops.inspectFn = func(ctx context.Context, id string) (agentops.ContainerState, error, bool) {
		if n.Add(1) >= 2 { // the first inspect is the runner's confirm
			return agentops.ContainerState{}, opErr(agent.CodeNotFound, "not found"), true
		}
		return agentops.ContainerState{}, nil, false
	}
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		return agentops.HealthResult{OK: false, Status: 503}, nil
	}
	g.start(false)
	t0 := time.Now()
	d, job := g.deploy(g.svc, func(s *Snapshot) { s.Health.TimeoutS = 600 })
	g.wait(job)
	r := g.row(d)
	if r.code != CodeStartFailed || !strings.Contains(r.reason, "removed") || strings.Contains(r.reason, "did not answer") || time.Since(t0) > 5*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(t0))
	}
}

// The health timeout reports the app's last answer, never an inspect that failed only
// because the gate's own deadline had run out.
func TestHealthTimeoutReasonIsTheAppsLastFailureNotTheDeadline(t *testing.T) {
	g := newRRig(t)
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		return agentops.HealthResult{OK: false, Status: 503}, nil
	}
	// Every inspect after the runner's own fails once the gate's context has ended,
	// as the real hub's would: it returns the context's cause.
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	r := g.row(d)
	if r.code != CodeHealthTimeout || !strings.Contains(r.reason, "HTTP 503") || strings.Contains(r.reason, "did not answer") {
		t.Fatalf("%+v", r)
	}
}
