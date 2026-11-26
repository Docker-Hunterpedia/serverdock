package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// This file is the post-health half of a blue-green deploy (plan 0004 step 8b):
// switch, commit, drain, stop_old and done. It is the only runner file that
// touches the routes (the source-scan test keeps them out of runner.go and
// rundeploy.go), and it follows the commit order of the plan to the letter:
//
//	switch   an override sends the proxy's route for this service to the candidate
//	         (a table swap in the agent: no Caddy load, Revision 11);
//	commit   ONE transaction makes the candidate the service's active deploy; the
//	         override is cleared in the same step under the route lock;
//	drain    wait exactly drain_s;
//	stop_old stop and remove the previous container, only now.
//
// The previous container is never stopped, restarted or removed before the commit,
// on any path (stopOld refuses to run unless rn.committed). Nothing here is
// cancellable: every phase runs on a context cut off from the job's (a cancel, a
// supersede, a session revoke or the jobs.timeout_s watchdog cannot reach it),
// each with its own bound. The one thing that ends the drain early is a shutdown
// (plan 0004, Shutdown: a deploy in drain at SIGTERM is left as it is and the boot
// pass settles it as succeeded). The switch is refused, before it begins, when the
// job was cancelled (enterSwitch).

// The bounds of the phases (plan 0004, "Deadlines and contexts").
const (
	// SwitchBound bounds the switch, including the wait for the route lock (one
	// in-flight apply) and proxy_apply (20 s).
	SwitchBound = 30 * time.Second
	// CommitBound bounds the wait for the route lock and the commit transaction. The
	// apply that follows a committed change has Routes' own bound (ApplyBound): it
	// normally sends nothing, because the commit does not change the routes.
	CommitBound = 10 * time.Second // the commit transaction; the lock wait before it has SwitchBound
	// StopExtra is added to stop_grace_s for the stop of the previous container.
	StopExtra = 15 * time.Second
	// RemoveBound bounds the removal of the previous container, and the list that
	// finds it.
	RemoveBound = 30 * time.Second
	// recordBound bounds each write after the commit.
	recordBound = 10 * time.Second
)

// RouteControl is what the switch and the commit ask of the routes: *Routes. It
// has no proxy_apply and no plain ApplyRoutes: the switch is SetOverride, the
// commit is CommitRoutes (the no-retry apply with the override cleared), and a
// failure re-applies the database's routes with ClearOverride.
type RouteControl interface {
	SetOverride(ctx context.Context, service int64, o Override) error
	ReplaceOverride(ctx context.Context, service int64, o Override) error
	ClearOverride(ctx context.Context, service int64) error
	CommitRoutes(ctx context.Context, service int64, commit func() error) error
	// DeleteRoutes runs a service delete's database delete as the mutate of the
	// no-retry apply (delete.go).
	DeleteRoutes(ctx context.Context, service int64, mutate func() error) error
}

func (rn *run) trace(event string) {
	if rn.r.trace != nil {
		rn.r.trace(event)
	}
}

func (rn *run) warn(msg string) { rn.warnings = append(rn.warnings, msg) }

// afterHealth takes a candidate that passed its health gate through switch,
// commit, drain and stop_old. ctx is cut off from the job's cancellation. It
// returns nil when the deploy was committed (rn.committed) and ran to its end, a
// failure when it ended before the commit (the old version keeps serving), or a
// shutdown failure when the drain was cut short after the commit.
func (rn *run) afterHealth(ctx context.Context) *failure {
	pausePoint("switch:before")
	if f := rn.enterSwitch(ctx); f != nil {
		return f
	}
	if f := rn.switchTraffic(ctx); f != nil {
		return f
	}
	pausePoint("switch:after")
	pausePoint("commit:before")
	if f := rn.commit(ctx); f != nil {
		return f
	}
	pausePoint("commit:after")
	return rn.afterCommit(ctx)
}

// ---- entering the switch ----

// enterSwitch is the point of no return for a cancel: one write transaction that
// reads the job's cancel flag and, only when it is clear, moves the deploy into
// the switch stage. deploys.cancel's write checks the stage and sets the flag in
// one write transaction too, and writes are serialised, so either the cancel lands
// first (this write sees the flag and refuses to switch: the candidate is removed
// and the old version is kept) or this write lands first (the cancel sees the
// stage and answers 409 not_cancellable). A job context that has ended without a
// flag (a shutdown) refuses the switch as well.
func (rn *run) enterSwitch(ctx context.Context) *failure {
	if rn.jobCtx.Err() != nil {
		return rn.refusedSwitch(rn.ctxFailure0(rn.jobCtx))
	}
	wctx, cancel := context.WithTimeout(ctx, recordBound)
	defer cancel()
	var flagged bool
	var reason string
	err := rn.r.cfg.DB.WriteTx(wctx, func(tx *sql.Tx) error {
		var cancelled int
		var why sql.NullString
		if err := tx.QueryRowContext(wctx, `SELECT cancel_requested, cancel_reason FROM jobs WHERE id = ?`, rn.job.ID).Scan(&cancelled, &why); err != nil {
			return err
		}
		if cancelled != 0 {
			flagged, reason = true, why.String
			return nil
		}
		if _, err := tx.ExecContext(wctx, `UPDATE deploys SET stage = ? WHERE id = ?`, StageSwitch, rn.deploy); err != nil {
			return err
		}
		_, err := tx.ExecContext(wctx, `UPDATE deploy_stages SET state = 'running', started_at = ? WHERE deploy_id = ? AND stage = ?`, rn.now(), rn.deploy, StageSwitch)
		return err
	})
	if err != nil {
		return rn.dbFailure(ctx, err)
	}
	if flagged {
		return rn.refusedSwitch(rn.cancelFailureFor(reason))
	}
	rn.stage = StageSwitch
	return nil
}

// refusedSwitch finishes a deploy whose switch was refused: nothing was routed to
// the candidate, so finish removes it and the previous version, untouched, keeps
// serving.
func (rn *run) refusedSwitch(f *failure) *failure {
	f.appSafe = rn.appSafe(nil)
	return f
}

// cancelFailureFor is the failure of a job whose cancel flag was set with reason.
func (rn *run) cancelFailureFor(reason string) *failure {
	if reason == "superseded" {
		return &failure{code: CodeSuperseded, status: StatusSuperseded, msg: "superseded by a newer deploy", by: rn.newerDeploy()}
	}
	if reason == "" {
		reason = "the deploy was cancelled"
	}
	return &failure{code: CodeCancelled, status: StatusCancelled, msg: agent.CleanText(reason, 200)}
}

// ---- the switch ----

// cleanupBound is the bound of a re-apply or a removal on a fresh context: 30 s,
// or 5 s when the process is shutting down.
func (rn *run) cleanupBound() time.Duration {
	if rn.shuttingDown() {
		return rn.r.cfg.ShutdownCleanupBound
	}
	return rn.r.cfg.CleanupBound
}

// shuttingDown: the process is stopping. The job context's cancel cause is only
// the FIRST cause (a supersede or revoke earlier hides a later ErrShutdown), so the
// scheduler's own stop signal is checked as well.
func (rn *run) shuttingDown() bool {
	if errors.Is(context.Cause(rn.jobCtx), jobs.ErrShutdown) {
		return true
	}
	if st := rn.stopChan(); st != nil {
		select {
		case <-st:
			return true
		default:
		}
	}
	return false
}

func (rn *run) stopChan() <-chan struct{} {
	if rn.r.cfg.Stopping == nil {
		return nil
	}
	return rn.r.cfg.Stopping()
}

// unwindSwitch is finish's hook for a deploy that ends before its commit after the
// switch was applied (a panic, an early return): the routes are put back first (once;
// revert records that it ran), and the candidate is removed only if the proxy
// confirmed it.
func (rn *run) unwindSwitch(f *failure) *failure {
	if rn.reverted {
		return f
	}
	return rn.revert(f)
}

// stopCandidate stops the candidate: with no grace if the routes never pointed at
// it, with stop_grace_s (its own bound) if they did, since it may have served
// requests. At shutdown the grace is dropped.
func (rn *run) stopCandidate(ctx context.Context, id string) error {
	if !rn.switched || rn.shuttingDown() {
		return rn.r.cfg.Ops.ContainerStop(ctx, id, 0)
	}
	grace := rn.snap.StopGraceS
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(grace)*time.Second+StopExtra)
	defer cancel()
	return rn.r.cfg.Ops.ContainerStop(sctx, id, grace)
}

// routeErrText is what the user is told about a route failure: never the proxy's
// own text (the details are in the log).
func routeErrText(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the proxy did not answer in time"
	case errors.Is(err, context.Canceled):
		return "the proxy was not reached"
	}
	return "the proxy could not apply the new routes"
}

// switchTraffic points the service's route at the candidate: SetOverride, with the
// switch bound (route lock wait included). Any error is a failed switch: the
// override is cleared and the old routes re-applied on a fresh context.
func (rn *run) switchTraffic(ctx context.Context) *failure {
	sctx, cancel := context.WithTimeout(ctx, rn.r.cfg.SwitchBound)
	defer cancel()
	if f := rn.ensureTracked(sctx); f != nil {
		return f
	}
	rn.trace("switch")
	rn.switched = true // from here the routes may name the candidate: finish unwinds them on any exit
	err := rn.r.cfg.Routes.SetOverride(sctx, rn.svc.id, Override{Kind: OverrideUpstream, Deploy: rn.deploy, IP: rn.ip, Port: rn.snap.ContainerPort})
	switch {
	case err == nil:
		rn.switchDetail = fmt.Sprintf("routes now point at the new version (%s)", rn.ip)
	case errors.Is(err, ErrRoutesNotReady):
		// Nothing can be applied yet (the proxy is off on this server, or the first
		// container list has not finished): the routes follow the database when it
		// can, and the database says this version after the commit.
		rn.switchDetail = "no routes were applied (the proxy is not ready); they follow this version when it is"
	default:
		rn.r.cfg.Log.Warn("the switch failed", "deploy", rn.deploy, "err", err.Error())
		return rn.revert(&failure{code: CodeSwitchFailed, msg: "the proxy could not switch traffic to the new version: " + routeErrText(err)})
	}
	return nil
}

// ensureTracked inspects the candidate once more and makes sure the state map
// shows it running at its address before it is routed to (an upstream override for a container the map
// does not hold would serve the "not running" page instead of the new version).
// The runner recorded it at start; a map that was replaced since (a resync) is
// refreshed here from the runner's own inspect. Nothing is routed yet, so a
// failure here ends the deploy like any pre-switch failure.
func (rn *run) ensureTracked(ctx context.Context) *failure {
	// Always a fresh inspect (one call): the candidate is checked running right
	// before it is routed to and committed, not trusted from the map.
	if f := rn.confirm(ctx); f != nil {
		f.msg = "before switching traffic: " + f.msg
		return f
	}
	if c, ok := rn.r.cfg.Tracker.State().Get(rn.cid); !ok || c.State != StateRunning {
		// Never switch to (and commit) a candidate that is not running.
		return &failure{code: CodeExited, appSafe: rn.appSafe(nil), msg: "the new version stopped before traffic was switched to it"}
	}
	return nil
}

// revert puts the routes back to what the database says (the old version, or the
// "not running" page for a first deploy): the override is cleared and the routes
// re-applied on a FRESH context (30 s), so the failed call's own bound cannot stop
// the repair. When the proxy confirms it, app_safe is yes if the old version is
// seen running (else unknown) and finish removes the candidate. When it does not
// confirm, the proxy may still point at the candidate, so the candidate is KEPT
// (finish does not remove it; the next resync does) and app_safe is unknown: the
// old container is alive either way, so the app is up whichever one the proxy
// serves.
func (rn *run) revert(f *failure) *failure {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(rn.jobCtx), rn.cleanupBound())
	defer cancel()
	rn.trace("revert")
	rn.reverted = true
	var err error
	if rn.gap {
		// Stop-first: the previous container is stopped, so the routes go back to the
		// maintenance page (not to a stopped container) until restoreOld has started it
		// again and the gate has passed. Still a table swap.
		err = rn.r.cfg.Routes.ReplaceOverride(fctx, rn.svc.id, Override{Kind: OverrideMaintenance})
	} else {
		err = rn.r.cfg.Routes.ClearOverride(fctx, rn.svc.id)
	}
	if err != nil && !errors.Is(err, ErrRoutesNotReady) {
		rn.r.cfg.Log.Warn("the previous routes could not be re-applied; the new container is kept", "deploy", rn.deploy, "err", err.Error())
		f.keep, f.appSafe = true, AppSafeUnknown
		f.msg += "; ServerDock could not confirm that the previous routes are back, so the new container is kept until it can check"
		return f
	}
	f.appSafe = rn.appSafe(nil)
	if rn.gap {
		f.msg += "; the maintenance page is back"
	} else if rn.svc.hasActive {
		f.msg += "; the previous version keeps serving"
	} else {
		f.msg += "; there is no previous version, so nothing is being served"
	}
	return f
}

// ---- the commit ----

// commitTx is the commit: one transaction. services.active_deploy_id moves from
// the version this deploy replaces (compare-and-set: a service changed under the
// deploy refuses it) to the candidate; the deploy gets committed_at and its
// stage; the switch and commit stages end, and the drain stage starts, in the same
// transaction, so no write sits between the switch and the commit and no write
// after the commit is needed to know where the deploy is. It runs as the mutate of
// CommitRoutes, under the route lock. It sets rn.committed the moment the
// transaction has committed.
func (rn *run) commitTx(ctx context.Context) error {
	rn.trace("commit:begin")
	var old any
	if rn.svc.hasActive {
		old = rn.svc.activeDeploy
	}
	now := rn.now()
	err := rn.r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE services SET active_deploy_id = ?, needs_attention = CASE WHEN needs_attention IN (?, ?) THEN NULL ELSE needs_attention END
			WHERE id = ? AND active_deploy_id IS ?`, rn.deploy, AttentionPreviousDown, AttentionVersionsKept, rn.svc.id, old)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("the service changed while this deploy was running")
		}
		next := StageDrain
		switch {
		case !rn.svc.hasActive:
			next = StageDone // nothing to drain or stop
		case rn.stopFirst:
			next = StageStopOld // the previous container is already stopped: no drain stage
		}
		res, err = tx.ExecContext(ctx, `UPDATE deploys SET committed_at = ?, stage = ? WHERE id = ? AND status = 'running'`, now, next, rn.deploy)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("the deploy had already ended")
		}
		for _, s := range []struct{ stage, state, detail string }{
			{StageSwitch, StageOK, rn.switchDetail},
			{StageCommit, StageOK, "the new version is the live version"},
		} {
			if _, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = ?, started_at = COALESCE(started_at, ?), finished_at = ?, detail = ? WHERE deploy_id = ? AND stage = ?`,
				s.state, now, now, agent.CleanText(s.detail, agent.MaxDetailText), rn.deploy, s.stage); err != nil {
				return err
			}
		}
		if rn.svc.hasActive {
			_, err = tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'running', started_at = ? WHERE deploy_id = ? AND stage = ?`, now, rn.deploy, next)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'skipped', detail = 'no previous version' WHERE deploy_id = ? AND stage IN (?, ?)`, rn.deploy, StageDrain, StageStopOld)
		}
		return err
	})
	if err == nil {
		rn.committed = true
		rn.trace("commit")
	}
	return err
}

// commit runs commitTx as the mutate of CommitRoutes (no retry of failed ports,
// no reload) on its own bound. A deploy that is committed stays committed
// whatever else fails here.
func (rn *run) commit(ctx context.Context) *failure {
	// The wait for the route lock (one in-flight apply of any service) has the
	// switch's allowance; the transaction itself keeps the commit bound.
	cctx, cancel := context.WithTimeout(ctx, rn.r.cfg.SwitchBound)
	defer cancel()
	rn.trace("commit:lock")
	err := rn.r.cfg.Routes.CommitRoutes(cctx, rn.svc.id, func() error {
		tctx, tcancel := context.WithTimeout(context.WithoutCancel(cctx), rn.r.cfg.CommitBound) // the lock wait must not eat the transaction's time
		defer tcancel()
		return rn.commitTx(tctx)
	})
	if rn.committed {
		if err != nil && !errors.Is(err, ErrRoutesNotReady) {
			// Committed, but the send after it failed (the routes are marked stale: the
			// proxy may not match the database until an apply succeeds).
			rn.r.cfg.Log.Warn("the proxy did not confirm the routes after the commit", "deploy", rn.deploy, "err", err.Error())
			rn.warn("the proxy did not confirm the final routes; use Try again if the app does not answer")
		}
		return nil
	}

	// Not committed: the transaction failed, or the wait for the route lock ended
	// before it ran. Look at the database before trusting that (a COMMIT can fail
	// after it took effect).
	rn.stage = StageCommit
	rn.r.cfg.Log.Warn("the commit failed", "deploy", rn.deploy, "err", orNilText(err))
	switch rn.committedInDB() {
	case committedYes:
		rn.committed = true
		rn.warn("the database reported an error for the commit, but the commit is recorded")
		if cerr := rn.clearOverride(); cerr != nil { // the commit's own step that clears it did not run
			rn.r.cfg.Log.Warn("the override could not be cleared after a commit that was recorded", "deploy", rn.deploy, "err", cerr.Error())
		}
		return nil
	case committedUnknown:
		f := &failure{code: CodeCommitFailed, keep: true, appSafe: AppSafeUnknown,
			msg: "ServerDock could not tell whether the new version was recorded as the live version, so both versions are kept until it can check"}
		if cerr := rn.clearOverride(); cerr != nil {
			rn.r.cfg.Log.Warn("the routes could not be re-applied after an unknown commit", "deploy", rn.deploy, "err", cerr.Error())
		}
		return f
	}
	rn.softStage(StageSwitch, StageOK, "traffic was switched, then the commit failed and the routes were put back")
	return rn.revert(&failure{code: CodeCommitFailed, msg: "the new version could not be recorded as the live version"})
}

func orNilText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// setMaintenance puts the service's route on the maintenance page ("Deploying,
// back in a moment", 503, Retry-After: 10): the stop-first gap. A table swap, no
// Caddy load. rn.gap is set BEFORE the call, so any outcome (a failure that left
// the page up included) is taken down again by finish.
func (rn *run) setMaintenance(ctx context.Context) error {
	rn.gap = true
	rn.trace("maintenance")
	return rn.r.cfg.Routes.SetOverride(ctx, rn.svc.id, Override{Kind: OverrideMaintenance})
}

// routesBack ends the stop-first gap: the override is cleared and the routes follow
// the database again (the previous version, which restoreOld has started and
// checked, or the "not running" page when it did not come back).
func (rn *run) routesBack(ctx context.Context) error {
	rn.gap = false
	return rn.r.cfg.Routes.ClearOverride(ctx, rn.svc.id)
}

// clearOverride clears the service's override and re-applies, on a fresh context.
func (rn *run) clearOverride() error {
	rn.reverted = true
	fctx, cancel := context.WithTimeout(context.WithoutCancel(rn.jobCtx), rn.cleanupBound())
	defer cancel()
	return rn.r.cfg.Routes.ClearOverride(fctx, rn.svc.id)
}

type commitState int

const (
	committedNo commitState = iota
	committedYes
	committedUnknown
)

// committedInDB reads services.active_deploy_id on a fresh context.
func (rn *run) committedInDB() commitState {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(rn.jobCtx), recordBound)
	defer cancel()
	var active sql.NullInt64
	if err := rn.r.cfg.DB.R.QueryRowContext(ctx, `SELECT active_deploy_id FROM services WHERE id = ?`, rn.svc.id).Scan(&active); err != nil {
		return committedUnknown
	}
	if active.Valid && active.Int64 == rn.deploy {
		return committedYes
	}
	return committedNo
}

// softStage writes a stage row's state after a failure, best effort.
func (rn *run) softStage(stage, state, detail string) {
	rn.soft("a stage record", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = ?, finished_at = ?, detail = ? WHERE deploy_id = ? AND stage = ?`,
			state, rn.now(), agent.CleanText(detail, agent.MaxDetailText), rn.deploy, stage)
		return err
	})
}

// soft is a write that must never fail the deploy: after the commit a failed write
// is logged and the deploy is marked "record incomplete" (plan 0004, failure
// rules); before it, a failed best-effort stage record is only logged.
func (rn *run) soft(what string, fn func(ctx context.Context, tx *sql.Tx) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(rn.jobCtx), recordBound)
	defer cancel()
	if err := rn.r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		if rn.committed {
			rn.incomplete = true
		}
		rn.r.cfg.Log.Error("a record could not be written (the deploy is not failed by it)", "what", what, "deploy", rn.deploy, "err", err.Error())
	}
}

// ---- after the commit: drain, stop_old, done ----

// afterCommit drains the previous version and stops it. ctx is cut off from the
// job's. Nothing here can fail the deploy: the new version is live.
func (rn *run) afterCommit(ctx context.Context) *failure {
	if !rn.svc.hasActive {
		return nil // a first deploy: no previous version to drain or stop (the commit skipped both stages)
	}
	old := rn.svc.activeDeploy

	if !rn.stopFirst { // stop-first has no drain: the previous container stopped before the new one started
		pausePoint("drain:before")
		rn.trace("drain")
		shutdown, cut := rn.drain()
		if shutdown {
			return &failure{code: CodeShutdown, msg: "ServerDock was stopped while the previous version was draining"}
		}
		detail := fmt.Sprintf("waited %d seconds for open requests to finish", rn.snap.DrainS)
		if cut {
			detail = "cut short: the service is being deleted"
			rn.warn("the drain was cut short because the service is being deleted")
		}
		rn.soft("the drain stage", func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'ok', finished_at = ?, detail = ? WHERE deploy_id = ? AND stage = ?`,
				rn.now(), detail, rn.deploy, StageDrain)
			return err
		})
		pausePoint("drain:after")
	}

	pausePoint("stop_old:before")
	rn.soft("the stop_old stage", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE deploys SET stage = ? WHERE id = ?`, StageStopOld, rn.deploy); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'running', started_at = ? WHERE deploy_id = ? AND stage = ?`, rn.now(), rn.deploy, StageStopOld)
		return err
	})
	detail := rn.stopOld(ctx, old)
	rn.soft("the stop_old stage", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'ok', finished_at = ?, detail = ? WHERE deploy_id = ? AND stage = ?`,
			rn.now(), agent.CleanText(detail, agent.MaxDetailText), rn.deploy, StageStopOld)
		return err
	})
	pausePoint("stop_old:after")
	return nil
}

// drain waits exactly drain_s (Caddy's reload grace is 10 s, and drain_s is never
// below it). It is a bounded wait during a deploy: one timer, no goroutine. A
// shutdown ends it early (shutdown: the deploy is left as it is, committed, and the
// boot pass settles it); so does a queued service delete (cut: the rest of the wait
// is skipped and the previous container is stopped with its normal grace, then the
// delete runs). A supersede, a cancel or a session revoke closes the job's context
// but cannot reach the drain.
func (rn *run) drain() (shutdown, cut bool) {
	t := time.NewTimer(time.Duration(rn.snap.DrainS) * rn.r.cfg.DrainUnit)
	defer t.Stop()
	done := rn.jobCtx.Done()
	stop := rn.stopChan()  // a shutdown even when an earlier cancel hid ErrShutdown
	del := rn.deleteChan() // a delete even when an earlier cancel hid its reason
	for {
		select {
		case <-t.C:
			return false, false
		case <-stop:
			return true, false
		case <-del:
			return false, true
		case <-done:
			var cc jobs.CancelCause
			switch {
			case errors.Is(context.Cause(rn.jobCtx), jobs.ErrShutdown):
				return true, false
			case errors.As(context.Cause(rn.jobCtx), &cc) && cc.Reason == jobs.ReasonDeleted:
				return false, true
			}
			done = nil // another cancel: not cancellable now, keep waiting
		}
	}
}

func (rn *run) deleteChan() <-chan struct{} {
	if rn.r.cfg.DeleteSignal == nil {
		return nil
	}
	return rn.r.cfg.DeleteSignal(rn.job.ID)
}

// oldContainers finds the containers of the previous deploy: a fresh container_list
// (any attempt), falling back to the state map when Docker does not answer. Never
// the candidate, never a container the agent says is not ours.
func (rn *run) oldContainers(ctx context.Context, old int64) ([]string, error) {
	lctx, cancel := context.WithTimeout(ctx, RemoveBound)
	defer cancel()
	svc, dep := strconv.FormatInt(rn.svc.id, 10), strconv.FormatInt(old, 10)
	list, err := rn.r.cfg.Ops.ContainerList(lctx)
	rn.op(lctx, err)
	var ids []string
	if err == nil {
		for _, c := range list.Containers {
			if !c.Unowned && c.ID != rn.cid && c.Labels.Service == svc && c.Labels.Deploy == dep {
				ids = append(ids, c.ID)
			}
		}
		return ids, nil
	}
	for _, c := range rn.r.cfg.Tracker.State().snapshot() {
		if c.Service == rn.svc.id && c.Deploy == old && c.ID != rn.cid {
			ids = append(ids, c.ID)
		}
	}
	return ids, err
}

// stopOld stops and removes the previous version's container(s), now that the
// commit is behind us. A failure is a warning, never a failed deploy: the next
// resync removes what is left. It returns the stage's detail.
func (rn *run) stopOld(ctx context.Context, old int64) string {
	if !rn.committed || old < 1 || old == rn.deploy {
		return "nothing was stopped" // never before the commit, never the candidate
	}
	rn.trace("stop_old")
	ids, lerr := rn.oldContainers(ctx, old)
	if len(ids) == 0 {
		if lerr != nil {
			msg := "the previous container could not be looked up: " + opText(lerr, "Docker did not answer") + "; the next resync removes it"
			rn.warn(msg)
			return msg
		}
		return "the previous container was already gone"
	}
	ops := rn.r.cfg.Ops
	grace := rn.snap.StopGraceS
	var notes []string
	for _, id := range ids {
		if rn.r.cfg.KeepLog != nil {
			kctx, kcancel := context.WithTimeout(ctx, recordBound)
			if err := rn.r.cfg.KeepLog(kctx, old, id); err != nil {
				notes = append(notes, "log not kept: "+agent.CleanText(err.Error(), 120))
			}
			kcancel()
		}
		sctx, scancel := context.WithTimeout(ctx, time.Duration(grace)*time.Second+StopExtra)
		stopErr := ops.ContainerStop(sctx, id, grace)
		rn.op(sctx, stopErr)
		scancel()
		gone := agentops.IsCode(stopErr, agent.CodeNotFound)
		if stopErr != nil && !gone {
			msg := "the previous container could not be stopped: " + opText(stopErr, "Docker did not answer") + "; the next resync removes it"
			rn.warn(msg)
			notes = append(notes, msg)
			continue
		}
		if gone {
			rn.r.cfg.Tracker.Untrack(id)
			continue
		}
		rctx, rcancel := context.WithTimeout(ctx, RemoveBound)
		rmErr := ops.ContainerRemove(rctx, id)
		rn.op(rctx, rmErr)
		rcancel()
		if rmErr != nil && !agentops.IsCode(rmErr, agent.CodeNotFound) {
			msg := "the previous container could not be removed: " + opText(rmErr, "Docker did not answer") + "; the next resync removes it"
			rn.warn(msg)
			notes = append(notes, msg)
			continue
		}
		rn.r.cfg.Tracker.Untrack(id)
	}
	if len(notes) == 0 {
		return "the previous container was stopped and removed"
	}
	return strings.Join(notes, "; ")
}

// ---- done ----

// finishCommitted ends a deploy that was committed: it is succeeded, whatever
// happened after the commit. A shutdown that cut the drain short is the one
// exception: the row is left as it is and the error leaves the job running for the
// next boot's pass, which settles a committed deploy as succeeded (the previous
// container is then removed by the resync). Anything else (a panic after the
// commit included) becomes a warning on the succeeded deploy.
func (rn *run) finishCommitted(f *failure) error {
	if f != nil && f.code == CodeShutdown {
		// Left as it is: the deploy is committed and live; the previous container is
		// removed at the next start. Write that honestly, once, on the shutdown bound,
		// so neither the queue's backstop nor the boot pass has to guess. A failed
		// write is covered by both.
		rn.recordShutdownDuringDrain()
		return nil
	}
	if f != nil {
		rn.warn("ServerDock hit an internal error after switching traffic; the new version is live and the previous container may still need removing")
		if rn.switched { // a panic inside the commit's mutate leaves the override behind
			if err := rn.clearOverride(); err != nil {
				rn.r.cfg.Log.Warn("the override could not be cleared after an internal error", "deploy", rn.deploy, "err", err.Error())
			}
		}
	}
	rn.recordSucceeded()
	return nil
}

// recordSucceeded writes the end of a committed deploy on a fresh context, twice
// if the first write fails. If both fail the deploy shows "running" until its job
// ends succeeded, and then the queue settles it (jobs.settleCommittedDeployTx) with
// the "record incomplete" reason.
func (rn *run) recordSucceeded() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(rn.jobCtx), recordBound)
	defer cancel()
	warnings := append([]string(nil), rn.warnings...)
	if rn.incomplete {
		warnings = append(warnings, "record incomplete: some details of this deploy could not be written")
	}
	var code, reason any
	if len(warnings) > 0 {
		code, reason = "warning", agent.CleanText(strings.Join(warnings, "; "), agent.MaxReasonText)
	}
	var err error
	for try := 0; try < 2; try++ {
		err = rn.r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			ts := rn.now()
			if _, err := tx.ExecContext(ctx, `UPDATE deploys SET status = 'succeeded', stage = ?, reason_code = ?, reason = ?, finished_at = ?
				WHERE id = ? AND status IN ('queued', 'running')`, StageDone, code, reason, ts, rn.deploy); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'ok', finished_at = ? WHERE deploy_id = ? AND state = 'running'`, ts, rn.deploy); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'skipped' WHERE deploy_id = ? AND state = 'pending'`, rn.deploy)
			return err
		})
		if err == nil {
			break
		}
	}
	if err != nil {
		rn.r.cfg.Log.Error("the end of a committed deploy could not be recorded (the queue settles it as succeeded, record incomplete)", "deploy", rn.deploy, "err", err.Error())
		return
	}
	if _, err := PruneDeploys(ctx, rn.r.cfg.DB, rn.svc.id); err != nil {
		rn.r.cfg.Log.Warn("old deploys could not be pruned", "err", err.Error())
	}
	rn.pruneLogs(ctx)
}

// recordShutdownDuringDrain ends a committed deploy that a shutdown cut short in
// the drain: succeeded, with the reason that says what was left undone.
func (rn *run) recordShutdownDuringDrain() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(rn.jobCtx), rn.r.cfg.ShutdownCleanupBound)
	defer cancel()
	const reason = "ServerDock was stopped during the drain; the previous container is removed at the next start"
	err := rn.r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		ts := rn.now()
		if _, err := tx.ExecContext(ctx, `UPDATE deploys SET status = 'succeeded', stage = ?, reason_code = 'warning', reason = ?, finished_at = ?
			WHERE id = ? AND status IN ('queued', 'running')`, StageDone, reason, ts, rn.deploy); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'ok', finished_at = ?, detail = 'cut short by the shutdown' WHERE deploy_id = ? AND state = 'running'`, ts, rn.deploy); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'skipped' WHERE deploy_id = ? AND state = 'pending'`, rn.deploy)
		return err
	})
	if err != nil {
		rn.r.cfg.Log.Error("the end of a deploy cut short by the shutdown could not be written (the queue or the boot pass settles it)", "deploy", rn.deploy, "err", err.Error())
	}
}
