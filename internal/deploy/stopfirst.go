package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// This file is the stop-first strategy (plan 0004 step 9, "Stop-first"): for apps
// that must never run twice. The stages are pull, check, maintenance,
// stop_old_first, start, health, switch, commit, stop_old:
//
//	maintenance     the route shows "Deploying, back in a moment" (503, Retry-After)
//	stop_old_first  the previous container is stopped (kept, not removed)
//	start, health   the candidate, exactly as in blue-green
//	switch, commit  exactly as in blue-green (switch.go), no drain
//	stop_old        the already stopped previous container is removed
//
// Any failure or cancel after the maintenance page went up takes it down again, and
// any failure or cancel after the stop starts the previous container again
// (restoreOld, from finish): remove the candidate, start the old one, run its
// health gate, clear the override. Routes are only ever touched through switch.go
// (setMaintenance, routesBack, revert): the whole path is table swaps, never a
// Caddy load.

// RestartExtra is added to health_timeout_s for "restart the old container": the
// start, the health gate and the route back (plan 0004, "Deadlines and contexts").
const RestartExtra = 30 * time.Second

// Codes of the stop-first stages.
const (
	CodeMaintenanceFailed = "maintenance_failed" // the proxy could not show the maintenance page
	CodeStopFailed        = "stop_failed"        // the previous container could not be stopped
)

// AttentionPreviousDown is services.needs_attention after a stop-first deploy
// failed and the previous version did not come back: the service shows ◆ "Needs
// attention" until a deploy commits.
const AttentionPreviousDown = "previous_version_down"

// AttentionVersionsKept is services.needs_attention when a stop-first deploy ended
// with its candidate kept (the proxy could not confirm the routes, or the commit's
// outcome is unknown) after the previous container was stopped: ServerDock did not
// start the previous container again (two versions must never run), so the service
// waits for the resync (step 10) to settle which one serves.
const AttentionVersionsKept = "versions_kept"

// oldVersion is the version a stop-first deploy replaces: its containers that were
// running when the gap began, and what its health gate needs.
type oldVersion struct {
	ip     string
	port   int
	health SnapHealth
	ids    []string
}

// skipStage marks a stage that does not apply to this deploy.
func (rn *run) skipStage(stage, detail string) {
	rn.soft("a stage record", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'skipped', detail = ? WHERE deploy_id = ? AND stage = ?`,
			agent.CleanText(detail, agent.MaxDetailText), rn.deploy, stage)
		return err
	})
}

// maintenance is stage 3: the page goes up. A deploy with nothing running to stop
// (a first deploy, or a previous container that is already down) has no gap: both
// stop-first stages are skipped and it goes on like a blue-green deploy.
func (rn *run) maintenance(ctx context.Context) *failure {
	if !rn.svc.hasActive {
		rn.skipStage(StageMaintenance, "no previous version")
		rn.skipStage(StageStopOldFirst, "no previous version")
		return nil
	}
	if f := rn.enter(ctx, StageMaintenance); f != nil {
		return f
	}
	old, f := rn.findOld(ctx)
	if f != nil {
		return f
	}
	if len(old.ids) == 0 {
		rn.skipStage(StageMaintenance, "the previous version is not running, so there is no gap")
		rn.skipStage(StageStopOldFirst, "the previous version is not running")
		return nil
	}
	rn.old = old
	detail := "the maintenance page is up (503, Retry-After 10)"
	if err := rn.setMaintenance(ctx); err != nil {
		switch {
		case errors.Is(err, ErrRoutesNotReady):
			detail = "no routes were applied (the proxy is not ready)"
		case ctx.Err() != nil:
			return rn.ctxFailure(ctx)
		default:
			rn.r.cfg.Log.Warn("the maintenance page could not be put up", "deploy", rn.deploy, "err", err.Error())
			return &failure{code: CodeMaintenanceFailed, msg: "the proxy could not show the maintenance page: " + routeErrText(err)}
		}
	}
	return rn.leave(ctx, StageMaintenance, detail)
}

// findOld looks for the previous version's running containers (a fresh list, each
// one inspected and trusted only when it is ours and carries this service and
// deploy). A Docker failure here changes nothing yet, so it simply fails the deploy.
func (rn *run) findOld(ctx context.Context) (*oldVersion, *failure) {
	ids, err := rn.oldContainers(ctx, rn.svc.activeDeploy)
	if err != nil {
		if ctx.Err() != nil {
			return nil, rn.ctxFailure(ctx)
		}
		return nil, &failure{code: CodeDocker, msg: "Docker could not look for the running version: " + opText(err, "no answer")}
	}
	o := &oldVersion{port: rn.snap.ContainerPort, health: rn.snap.Health}
	var raw string
	var ip sql.NullString
	if err := rn.r.cfg.DB.R.QueryRowContext(ctx, `SELECT ip, snapshot_json FROM deploys WHERE id = ?`, rn.svc.activeDeploy).Scan(&ip, &raw); err == nil {
		o.ip = ip.String
		if s, perr := ParseSnapshot(raw); perr == nil {
			o.port, o.health = s.ContainerPort, s.Health // what the old container was built for
		}
	}
	for _, id := range ids {
		st, err := rn.r.cfg.Ops.ContainerInspect(ctx, id)
		rn.op(ctx, err)
		if err != nil {
			if ctx.Err() != nil {
				return nil, rn.ctxFailure(ctx)
			}
			if agentops.IsCode(err, agent.CodeNotFound) {
				continue
			}
			return nil, &failure{code: CodeDocker, msg: "Docker could not inspect the running version: " + opText(err, "no answer")}
		}
		if !rn.oldOurs(st, id) || !(st.Running || st.Restarting) {
			continue
		}
		o.ids = append(o.ids, id)
		if o.ip == "" {
			o.ip = st.ConfiguredIP
		}
	}
	return o, nil
}

// oldOurs is the trust rule for the previous version's container: owned, and its
// labels name this service and the deploy that is live.
func (rn *run) oldOurs(st agentops.ContainerState, id string) bool {
	return st.Owned && st.ID == id &&
		st.Labels[agentops.LabelService] == strconv.FormatInt(rn.svc.id, 10) &&
		st.Labels[agentops.LabelDeploy] == strconv.FormatInt(rn.svc.activeDeploy, 10)
}

// stopOldFirst is stage 4: stop (not remove) the previous container. It is
// cancellable: a cancel at any point leaves finish to start it again.
func (rn *run) stopOldFirst(ctx context.Context) *failure {
	if rn.old == nil {
		return nil // no gap (skipped by maintenance)
	}
	if f := rn.enter(ctx, StageStopOldFirst); f != nil {
		return f
	}
	grace := rn.snap.StopGraceS
	for _, id := range rn.old.ids {
		rn.stopSent = true // from here finish starts it again on any failure
		// The stop is NOT cancellable: a cancel in the middle of the grace would leave
		// the app SIGTERMed and dying while ServerDock believes it still runs. It runs
		// to its own bound (the grace + 15 s; 5 s at shutdown) and the cancel is read
		// right after. Known limit: a SIGTERM to ServerDock during a long stop can outrun
		// the 10 s process grace; the job is then left running, the boot pass settles it
		// and the boot resync (step 10, rule 3) starts the stopped previous container.
		bound := time.Duration(grace)*time.Second + StopExtra
		if rn.shuttingDown() {
			bound = rn.r.cfg.ShutdownCleanupBound
		}
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
		err := rn.r.cfg.Ops.ContainerStop(sctx, id, grace)
		rn.op(sctx, err)
		cancel()
		if err != nil && !agentops.IsCode(err, agent.CodeNotFound) {
			if ctx.Err() != nil {
				return rn.ctxFailure(ctx)
			}
			return &failure{code: CodeStopFailed, msg: "the previous version could not be stopped: " + opText(err, "Docker did not answer")}
		}
	}
	rn.trackOld(context.WithoutCancel(ctx))
	if ctx.Err() != nil { // a cancel that arrived during the stop: the stop finished, the previous version comes back
		return rn.ctxFailure(ctx)
	}
	return rn.leave(ctx, StageStopOldFirst, "the previous container is stopped and kept until the new version is live")
}

// trackOld refreshes the state map with what Docker says about the previous
// containers now (stopped after the stop, running after the restart), so the
// service's state and the routes are honest.
func (rn *run) trackOld(ctx context.Context) {
	if rn.old == nil {
		return
	}
	for _, id := range rn.old.ids {
		st, err := rn.r.cfg.Ops.ContainerInspect(ctx, id)
		rn.op(ctx, err)
		if err != nil {
			continue
		}
		if c, ok := ContainerFromInspect(st); ok && rn.oldOurs(st, id) {
			rn.r.cfg.Tracker.Track(c)
		}
	}
}

// restoreOld ends a stop-first deploy that did not commit: the maintenance page
// comes down and, if the previous container was stopped, it is started again and
// must pass its health gate before the routes go back to it. It runs from finish on
// a FRESH context bounded by health_timeout_s + 30 s (5 s at shutdown), after the
// candidate was removed. app_safe is yes when the previous version is serving
// again, no (and the service needs attention) when it is not, unknown when the
// proxy could not confirm the routes.
func (rn *run) restoreOld(f *failure) *failure {
	bound := time.Duration(rn.snap.Health.TimeoutS)*time.Second + RestartExtra
	if rn.shuttingDown() {
		bound = rn.r.cfg.ShutdownCleanupBound
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(rn.jobCtx), bound)
	defer cancel()
	rn.trace("restore_old")

	reason, unverified, deleting := "", false, false
	switch {
	case rn.stopSent && rn.deleteRequested():
		// A delete is queued: the service is going away, so do not wait behind a full
		// restore. The page comes down; the delete removes everything.
		deleting = true
	case rn.stopSent:
		reason, unverified = rn.startOld(ctx)
		if reason != "" && ctx.Err() != nil && rn.shuttingDown() {
			// The process is stopping and the bound ran out: neither proven nor failed.
			reason, unverified = "", true
		}
	}
	routesOK := true
	if err := rn.routesBack(ctx); err != nil && !errors.Is(err, ErrRoutesNotReady) {
		routesOK = false
		rn.r.cfg.Log.Warn("the routes could not be put back after a stop-first failure", "deploy", rn.deploy, "err", err.Error())
	}
	switch {
	case deleting:
		f.appSafe = AppSafeNo
		rn.setAttention(AttentionPreviousDown) // honest if the delete then fails: the previous version is down
		f.msg += "; the service is being deleted, so the previous version was not started again"
	case reason != "":
		f.appSafe = AppSafeNo
		f.msg += "; the previous version did not come back: " + reason
		rn.setAttention(AttentionPreviousDown)
	case unverified:
		f.appSafe = AppSafeUnknown
		f.msg += "; the previous version was started again but ServerDock stopped before it could check it"
	case !routesOK:
		f.appSafe = AppSafeUnknown
		f.keep = true
		f.msg += "; ServerDock could not confirm that the routes are back"
	case rn.stopSent:
		f.appSafe = AppSafeYes
		f.msg += "; the previous version is serving again (it passed its health check)"
		rn.setAttention("")
	default:
		// Never stopped: it kept serving behind the page.
		if f.appSafe == "" || f.appSafe == AppSafeUnknown {
			f.appSafe = rn.appSafe(nil)
		}
	}
	return f
}

// leaveKept is finish's answer for a stop-first deploy whose candidate is kept: the
// previous container is NOT started (the candidate may be serving, or the commit may
// have succeeded), the override is dropped from memory (the next apply follows the
// database), and the service is flagged when the previous version is down. The
// resync (step 10) settles it.
func (rn *run) leaveKept(f *failure) *failure {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(rn.jobCtx), rn.cleanupBound())
	defer cancel()
	_ = rn.routesBack(ctx) // best effort: the override is deleted first, whatever the apply does
	f.appSafe = AppSafeUnknown
	if rn.stopSent {
		f.msg += "; the previous version is stopped and was not started again: the new version is kept because ServerDock could not confirm the routes or the commit, and two versions must never run"
		rn.setAttention(AttentionVersionsKept)
	}
	return f
}

// deleteRequested: a service delete asked this job to stop.
func (rn *run) deleteRequested() bool {
	var cc jobs.CancelCause
	if errors.As(context.Cause(rn.jobCtx), &cc) && cc.Reason == jobs.ReasonDeleted {
		return true
	}
	if ch := rn.deleteChan(); ch != nil {
		select {
		case <-ch:
			return true
		default:
		}
	}
	return false
}

// setAttention sets (code) or clears ("") the service's needs_attention for this
// feature; it never overwrites another reason.
func (rn *run) setAttention(code string) {
	rn.soft("the service's attention flag", func(ctx context.Context, tx *sql.Tx) error {
		if code == "" {
			_, err := tx.ExecContext(ctx, `UPDATE services SET needs_attention = NULL WHERE id = ? AND needs_attention IN (?, ?)`, rn.svc.id, AttentionPreviousDown, AttentionVersionsKept)
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE services SET needs_attention = ? WHERE id = ? AND (needs_attention IS NULL OR needs_attention = ?)`, code, rn.svc.id, AttentionDeleteConfirmAgain)
		return err
	})
}

// startOld starts the previous container(s) if they are down, then runs the same
// health gate on them. It returns why the previous version did not come back ("" if
// it did). unverified is true when ServerDock ran out of time because the process is
// stopping, with the container running: neither proven nor failed.
func (rn *run) startOld(ctx context.Context) (reason string, unverified bool) {
	o := rn.old
	ops := rn.r.cfg.Ops
	defer func() {
		// Whatever happened, the state map shows what Docker says now.
		rn.trackOld(ctx)
	}()
	for _, id := range o.ids {
		st, err := ops.ContainerInspect(ctx, id)
		rn.op(ctx, err)
		switch {
		case agentops.IsCode(err, agent.CodeNotFound):
			return "its container no longer exists", false
		case err != nil:
			return "Docker did not answer: " + opText(err, "no answer"), false
		case !rn.oldOurs(st, id):
			return "its container is not the one that was stopped", false
		}
		if st.Running || st.Restarting {
			continue
		}
		err = ops.ContainerStart(ctx, id)
		rn.op(ctx, err)
		if err != nil {
			return "it could not be started: " + opText(err, "Docker did not answer"), false
		}
	}
	return rn.gateOld(ctx)
}

// gateOld is the health gate of the previous version's containers (rn.old): every
// check inspects them (fail fast on an exit, a crash loop or an OOM kill) and
// probes the app, until health.passes in a row pass or health_timeout_s runs out.
// It returns why the version is not healthy ("" if it is).
func (rn *run) gateOld(ctx context.Context) (reason string, unverified bool) {
	o := rn.old
	ops := rn.r.cfg.Ops
	h := o.health
	hctx, hcancel := context.WithTimeout(ctx, time.Duration(h.TimeoutS)*time.Second)
	defer hcancel()
	target := net.JoinHostPort(o.ip, strconv.Itoa(o.port))
	passes, lastErr := 0, ""
	for {
		running := true
		for _, id := range o.ids {
			st, err := ops.ContainerInspect(hctx, id)
			rn.op(hctx, err)
			if err != nil {
				if hctx.Err() == nil && agentops.IsCode(err, agent.CodeNotFound) {
					return "its container disappeared", false
				}
				lastErr, running = "Docker did not answer the inspect", false
				continue
			}
			switch {
			case st.OOMKilled:
				return "it was killed for using too much memory", false
			case st.Restarting:
				return fmt.Sprintf("it keeps crashing (it restarted after exit code %d)", st.ExitCode), false
			case !st.Running:
				return fmt.Sprintf("it exited with code %d", st.ExitCode), false
			}
		}
		ok := false
		if running {
			if h.Kind == "running" {
				ok = true
			} else {
				var pf *failure
				if ok, pf = rn.probe(hctx, ctx, target, h, &lastErr); pf != nil {
					return pf.msg, false
				}
			}
		}
		if ok {
			if passes++; passes >= h.Passes {
				return "", false
			}
		} else {
			passes = 0
		}
		t := time.NewTimer(rn.r.cfg.HealthInterval)
		select {
		case <-hctx.Done():
			t.Stop()
			if rn.shuttingDown() {
				return "", true
			}
			msg := fmt.Sprintf("it was not healthy within %d seconds", h.TimeoutS)
			if lastErr != "" {
				msg += " (" + lastErr + ")"
			}
			return agent.CleanText(msg, 300), false
		case <-t.C:
		}
	}
}

// versionsKept reports whether the service is flagged versions_kept right now (a
// failed stop-first deploy left its candidate running beside the stopped previous
// version). It is read at RUN time under the service lock, because the flag is set
// by a job that may have run after this one was queued. It fails closed: a database
// error is an error, never "not flagged" (the caller refuses to go on).
func (rn *run) versionsKept(ctx context.Context) (bool, error) {
	return serviceVersionsKept(ctx, rn.r.cfg.DB.R, rn.svc.id)
}

func serviceVersionsKept(ctx context.Context, q rowQuerier, service int64) (bool, error) {
	var attention sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT needs_attention FROM services WHERE id = ?`, service).Scan(&attention); err != nil {
		return false, err
	}
	return attention.Valid && attention.String == AttentionVersionsKept, nil
}
