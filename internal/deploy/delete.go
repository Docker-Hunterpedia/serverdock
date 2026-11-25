package deploy

import (
	"context"
	"database/sql"
	"errors"
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

	// Everything from here is idempotent. A failure leaves the service flagged so the
	// UI says the delete did not finish.
	fail := func(msg string) (RunDeleteOut, error) {
		r.setAttention(p.Service, AttentionDeleteInterrupted)
		return out, errors.New(msg)
	}
	grace := 10
	var exists bool
	if err := r.cfg.DB.R.QueryRowContext(ctx, `SELECT stop_grace_s FROM services WHERE id = ?`, p.Service).Scan(&grace); err == nil {
		exists = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, errors.New("the service could not be read")
	}

	// 1. Containers: labelled with this service and owned by this install (the list
	// marks anything else unowned, and the agent refuses it again on stop and remove).
	list, err := r.cfg.Ops.ContainerList(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return out, context.Cause(ctx)
		}
		r.cfg.Tracker.State().MarkUnknown(p.Service)
		return fail("Docker could not list the containers: " + opText(err, "no answer"))
	}
	svc := strconv.FormatInt(p.Service, 10)
	for _, c := range list.Containers {
		if c.Unowned || c.Labels.Service != svc {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, time.Duration(grace)*time.Second+StopExtra)
		err := r.cfg.Ops.ContainerStop(sctx, c.ID, grace)
		cancel()
		if err != nil && !agentops.IsCode(err, agent.CodeNotFound) {
			if ctx.Err() != nil {
				return out, context.Cause(ctx)
			}
			return fail("a container of the service could not be stopped: " + opText(err, "Docker did not answer"))
		}
		if !agentops.IsCode(err, agent.CodeNotFound) {
			rctx, rcancel := context.WithTimeout(ctx, RemoveBound)
			err = r.cfg.Ops.ContainerRemove(rctx, c.ID)
			rcancel()
			if err != nil && !agentops.IsCode(err, agent.CodeNotFound) {
				if ctx.Err() != nil {
					return out, context.Cause(ctx)
				}
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

	// 3. The rows, as the mutate of ApplyRoutes: the route goes with them. Not a
	// retry of other services' failed ports (a failed port is not this delete's).
	if exists {
		err = r.cfg.Routes.DeleteRoutes(ctx, func() error { return r.deleteRows(ctx, p.Service) })
		var me *MutateError
		switch {
		case err == nil, errors.Is(err, ErrRoutesNotReady):
		case errors.As(err, &me):
			return fail("the service's records could not be deleted: " + agent.CleanText(me.Err.Error(), 160))
		default:
			// Deleted; the proxy did not confirm (Routes is marked stale: services.get says so).
			r.cfg.Log.Warn("the proxy did not confirm the routes after a service delete", "service", p.Service, "err", err.Error())
		}
	}
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
