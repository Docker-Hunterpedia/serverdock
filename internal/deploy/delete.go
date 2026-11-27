package deploy

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// This file is services.delete (queuing) and jobs.run_service_delete (running),
// plan 0004 step 9.
//
// Queuing (one transaction, jobs.Store.EnqueueDelete): every queued deploy or
// restart job of the service is cancelled, a running one is flagged (its deploy
// before the switch ends cancelled; one past its commit skips the rest of its drain
// and stops the old container with its normal grace), and the delete job is
// inserted. The job then waits for the service lock like any job.
//
// Running, every step idempotent so an interrupted delete is simply run again: stop
// and remove every container labelled with THIS service and THIS install (the agent
// refuses any other: a look-alike with another instance label is never touched, and
// the list marks it unowned), delete DATA/deploy-logs/<service id> (a validated
// path), then delete the rows (and the project if it was its last service) as the
// mutate of ApplyRoutes under the route lock: the route goes with the rows. Deleting
// an EXPOSED service changes the set of public ports, so that apply reloads Caddy:
// one of the documented reload cases, not a zero-drop one.

// ConfirmAgainMessage is why a delete job fails when its fresh-2FA window passed
// while it waited (a long stop grace plus queueing behind other work).
const ConfirmAgainMessage = "Confirming again is needed: the 5-minute window passed while the service was stopping"

// AttentionDeleteInterrupted is services.needs_attention of a service whose delete
// did not finish.
const AttentionDeleteInterrupted = "delete_interrupted"

// AttentionDeleteConfirmAgain is services.needs_attention of a service whose delete
// job was refused at run time for want of fresh two-factor confirmation: nothing was
// deleted, the queued deploys of the service were cancelled when the delete was
// queued, and the user must delete again. It is not delete_interrupted (the
// reconciler skips those services; this one is whole). It is set only on a service
// with no other reason, and cleared (1) when a new delete is queued and (2) when a
// deploy of the service commits (the service is serving a version again).
const AttentionDeleteConfirmAgain = "delete_confirm_again"

// DeleteOut is the output of services.delete: the delete was queued, not done.
type DeleteOut struct {
	Service string `json:"service" doc:"<project>/<service>"`
	Job     int64  `json:"job"`
	Status  string `json:"status" doc:"queued: the delete runs when the service's running work has ended; queued deploys and restarts of it were cancelled"`
	// Cancelled is how many queued deploys and restarts the request cancelled;
	// Flagged how many running ones it asked to stop.
	Cancelled int `json:"cancelled,omitempty"`
	Flagged   int `json:"flagged,omitempty"`
}

// RunDeleteIn is the input of jobs.run_service_delete. Service is the target the
// Confirm must equal.
type RunDeleteIn struct {
	Job     int64  `json:"job"`
	Service string `json:"service" doc:"<project>/<service>"`
}

// Validate implements actions.Validator.
func (in RunDeleteIn) Validate() error {
	if in.Job < 1 {
		return errors.New("job must be a job id")
	}
	_, _, err := splitRef(in.Service)
	return err
}

// RunDeleteOut is what jobs.run_service_delete returns (the audit outcome's output).
type RunDeleteOut struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	// Containers is how many containers were removed.
	Containers int `json:"containers"`
}

func (d *svcDeps) delete(ctx context.Context, c actions.Call, in ServiceRefIn) (DeleteOut, error) {
	if d.Jobs == nil {
		return DeleteOut{}, actions.Unavailable("deletes are not available on this server")
	}
	r, err := d.load(ctx, d.DB.R, in.Service)
	if err != nil {
		return DeleteOut{}, err
	}
	target := r.project + "/" + r.name
	out, err := d.Jobs.EnqueueDelete(ctx, jobs.NewDelete{ServiceID: r.id, Target: target, Actor: jobs.ActorOf(c.Actor), StopGraceS: r.graceS})
	switch {
	case errors.Is(err, jobs.ErrServiceDeleting):
		return DeleteOut{}, actions.Conflict("%s is already being deleted", target)
	case errors.Is(err, jobs.ErrActorSessionGone):
		return DeleteOut{}, &actions.DeniedError{Reason: SessionGoneMessage}
	case errors.Is(err, jobs.ErrInvalid):
		return DeleteOut{}, actions.InvalidInput("%v", strings.TrimPrefix(err.Error(), "jobs: invalid request: "))
	case err != nil:
		return DeleteOut{}, err
	}
	// A new delete answers the old "confirm again" (best effort: it is only a hint).
	if err := d.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE services SET needs_attention = NULL WHERE id = ? AND needs_attention = ?`, r.id, AttentionDeleteConfirmAgain)
		return err
	}); err != nil {
		d.Log.Warn("the confirm-again flag could not be cleared", "service", r.id, "err", err.Error())
	}
	return DeleteOut{Service: target, Job: out.JobID, Status: jobs.StateQueued, Cancelled: len(out.Cancelled), Flagged: len(out.CancelRunning)}, nil
}

// confirmAgainError is the error of a delete job that the policy refused for want
// of fresh two-factor confirmation: it reads as the plain sentence and is a denial.
type confirmAgainError struct{}

func (confirmAgainError) Error() string        { return ConfirmAgainMessage }
func (confirmAgainError) Is(target error) bool { return target == actions.ErrDenied }

// safeLogDir is the deploy-log directory of a service, validated: the root must be
// an absolute path, the service id a positive number, and the result exactly one
// level under the root (no traversal is possible: the name is a decimal id, and this
// checks it anyway).
func safeLogDir(root string, service int64) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("the deploy-log root is not an absolute path")
	}
	if service < 1 {
		return "", errors.New("not a service id")
	}
	root = filepath.Clean(root)
	dir := filepath.Join(root, strconv.FormatInt(service, 10))
	if filepath.Dir(dir) != root || filepath.Base(dir) != strconv.FormatInt(service, 10) {
		return "", errors.New("the deploy-log directory is outside its root")
	}
	return dir, nil
}

// removeLogs deletes a service's deploy-log directory (a symlink in its place is
// removed as a link, never followed).
func removeLogs(root string, service int64) error {
	if root == "" {
		return nil
	}
	dir, err := safeLogDir(root, service)
	if err != nil {
		return err
	}
	// The root itself must be a plain directory: through a link RemoveAll would delete
	// whatever the link points into.
	fi, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("the deploy-log root is not a plain directory")
	}
	return os.RemoveAll(dir)
}

// runDelete is jobs.run_service_delete's Execute.
func (r *Runner) runDelete(ctx context.Context, _ actions.Call, jobID int64) (RunDeleteOut, error) {
	if r.cfg.Ops == nil || r.cfg.Tracker == nil || r.cfg.Routes == nil {
		return RunDeleteOut{}, errors.New("deletes are not wired on this server")
	}
	j, err := r.cfg.Jobs.Get(ctx, jobID)
	if err != nil {
		return RunDeleteOut{}, errors.New("the job could not be read")
	}
	var p struct {
		Service int64  `json:"service"`
		Target  string `json:"target"`
	}
	if err := agent.DecodeArgs(j.Payload, &p); err != nil || p.Service < 1 || p.Target == "" {
		return RunDeleteOut{}, errors.New("the job has no service")
	}
	out := RunDeleteOut{Service: p.Target}
	unlock, err := r.cfg.Locks.Lock(ctx, p.Service)
	if err != nil {
		return out, context.Cause(ctx)
	}
	defer unlock()
	// One last look at a cancel (a session revoke, a supersede-like flag) before anything
	// is touched: a cancelled delete that has not started changes nothing. From here on
	// the delete runs to its end on a context cut off from the job's: every step has its
	// own bound and is idempotent, and a delete stopped halfway would leave a service
	// with no container and no flag. Anything that fails flags the service.
	if ctx.Err() != nil {
		return out, context.Cause(ctx)
	}
	ctx = context.WithoutCancel(ctx)
	fail := func(msg string) (RunDeleteOut, error) {
		r.cfg.Tracker.State().MarkDeleteFailed(p.Service) // also in memory, in case the flag cannot be written
		r.setAttention(p.Service, AttentionDeleteInterrupted)
		// The "not running" override of step 0 must not outlive a failed delete: the
		// service's rows and its active deploy's address are still held, so the routes
		// can safely follow the database and the state map again (best effort, on a
		// fresh bounded context).
		cctx, ccancel := context.WithTimeout(ctx, SwitchBound)
		if err := r.cfg.Routes.ClearOverride(cctx, p.Service); err != nil && !errors.Is(err, ErrRoutesNotReady) {
			r.cfg.Log.Warn("the routes could not be re-applied after a failed delete", "service", p.Service, "err", err.Error())
		}
		ccancel()
		return out, errors.New(msg)
	}
	step := func(d time.Duration) (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, d) }

	grace := 10
	var exists bool
	qctx, qcancel := step(recordBound)
	err = r.cfg.DB.R.QueryRowContext(qctx, `SELECT stop_grace_s FROM services WHERE id = ?`, p.Service).Scan(&grace)
	qcancel()
	switch {
	case err == nil:
		exists = true
	case !errors.Is(err, sql.ErrNoRows):
		return fail("the service could not be read")
	}

	// 0. The route goes FIRST: the service's route is set to the "not running" page and
	// the apply must be confirmed (nothing routed counts as confirmed) before any
	// container is touched. A stale proxy can then only show a static page, never an
	// upstream to an address the allocator is about to hand out again.
	if exists {
		rctx, rcancel := step(SwitchBound)
		err = r.cfg.Routes.ReplaceOverride(rctx, p.Service, Override{Kind: OverrideNotRunning}) // the map first, then the apply
		rcancel()
		if err != nil && !errors.Is(err, ErrRoutesNotReady) {
			r.cfg.Log.Warn("the route of a service being deleted could not be taken away", "service", p.Service, "err", err.Error())
			return fail("the proxy could not confirm that the service's route was removed, so nothing was deleted; delete again to retry: " + routeErrText(err))
		}
	}

	// 1. Containers: labelled with this service and owned by this install (the list
	// marks anything else unowned, and the agent refuses it again on stop and remove).
	lctx, lcancel := step(RemoveBound)
	list, err := r.cfg.Ops.ContainerList(lctx)
	lcancel()
	if err != nil {
		r.cfg.Tracker.State().MarkUnknown(p.Service)
		return fail("Docker could not list the containers: " + opText(err, "no answer"))
	}
	svc := strconv.FormatInt(p.Service, 10)
	for _, c := range list.Containers {
		if c.Unowned || c.Labels.Service != svc {
			continue
		}
		sctx, cancel := step(time.Duration(grace)*time.Second + StopExtra)
		err := r.cfg.Ops.ContainerStop(sctx, c.ID, grace)
		cancel()
		if err != nil && !agentops.IsCode(err, agent.CodeNotFound) {
			return fail("a container of the service could not be stopped: " + opText(err, "Docker did not answer"))
		}
		if !agentops.IsCode(err, agent.CodeNotFound) {
			rctx, rcancel := step(RemoveBound)
			err = r.cfg.Ops.ContainerRemove(rctx, c.ID)
			rcancel()
			if err != nil && !agentops.IsCode(err, agent.CodeNotFound) {
				return fail("a container of the service could not be removed: " + opText(err, "Docker did not answer"))
			}
		}
		r.cfg.Tracker.Untrack(c.ID)
		out.Containers++
	}
	r.cfg.Tracker.State().ClearUnknown(p.Service)

	// 2. The deploy logs (container output is not kept: the logs go too).
	if err := removeLogs(r.cfg.LogRoot, p.Service); err != nil {
		r.cfg.Log.Warn("the deploy logs of a deleted service could not be removed", "service", p.Service, "err", err.Error())
		return fail("the deploy logs could not be removed: " + agent.CleanText(err.Error(), 160))
	}

	// 3. The rows, as the mutate of ApplyRoutes: the route entry goes with them. Not a
	// retry of other services' failed ports (a failed port is not this delete's).
	if exists {
		dctx, dcancel := step(SwitchBound + ApplyBound)
		err = r.cfg.Routes.DeleteRoutes(dctx, p.Service, func() error { return r.deleteRows(dctx, p.Service) })
		dcancel()
		var me *MutateError
		switch {
		case err == nil, errors.Is(err, ErrRoutesNotReady):
		case errors.As(err, &me):
			return fail("the service's records could not be deleted: " + agent.CleanText(me.Err.Error(), 160))
		default:
			// Deleted, and the route already showed only a static page; the proxy did
			// not confirm the final list (Routes is marked stale: services.get says so).
			r.cfg.Log.Warn("the proxy did not confirm the routes after a service delete", "service", p.Service, "err", err.Error())
		}
	}
	r.cfg.Tracker.State().ClearDeleteFailed(p.Service)
	out.Status = "deleted"
	return out, nil
}

// deleteRows deletes the service (its deploys and stages go with it) and, when it
// was the last one, its project. One transaction.
func (r *Runner) deleteRows(ctx context.Context, service int64) error {
	return r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var project int64
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM services WHERE id = ?`, service).Scan(&project); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id = ?`, service); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ? AND NOT EXISTS (SELECT 1 FROM services WHERE project_id = ?)`, project, project)
		return err
	})
}

// setAttention records needs_attention on a service, best effort.
func (r *Runner) setAttention(service int64, code string) {
	ctx, cancel := context.WithTimeout(context.Background(), recordBound)
	defer cancel()
	if err := r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE services SET needs_attention = ? WHERE id = ?`, code, service)
		return err
	}); err != nil {
		r.cfg.Log.Error("a service's attention flag could not be written", "service", service, "err", err.Error())
	}
}

// flagConfirmAgain records AttentionDeleteConfirmAgain on a service that has no other
// reason (best effort).
func (r *Runner) flagConfirmAgain(service int64) {
	ctx, cancel := context.WithTimeout(context.Background(), recordBound)
	defer cancel()
	if err := r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE services SET needs_attention = ? WHERE id = ? AND needs_attention IS NULL`, AttentionDeleteConfirmAgain, service)
		return err
	}); err != nil {
		r.cfg.Log.Error("a service's attention flag could not be written", "service", service, "err", err.Error())
	}
}
