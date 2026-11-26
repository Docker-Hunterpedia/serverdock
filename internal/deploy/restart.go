package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// This file is services.restart (queuing) and jobs.run_restart (running), plan 0004
// step 9. A restart is container_restart on the live version's container with
// stop_grace_s: a short outage that keeps the container, its address and its route
// (no route call at all, so no Caddy load), followed by the same health gate a
// deploy uses. Honest: the job succeeds only when the app passed it.

// RestartOut is the output of services.restart: the restart was queued, not done.
type RestartOut struct {
	Service string `json:"service" doc:"<project>/<service>"`
	Job     int64  `json:"job"`
	Status  string `json:"status" doc:"queued: the restart runs when the scheduler picks it; it causes a short outage (use Redeploy to avoid one)"`
}

// RunRestartIn is the input of jobs.run_restart.
type RunRestartIn struct {
	Job int64 `json:"job"`
}

// Validate implements actions.Validator.
func (in RunRestartIn) Validate() error {
	if in.Job < 1 {
		return errors.New("job must be a job id")
	}
	return nil
}

// RunRestartOut is what jobs.run_restart returns (the audit outcome's output).
type RunRestartOut struct {
	Service string `json:"service"`
	Status  string `json:"status"`
}

type restartPayload struct {
	Service int64 `json:"service"`
}

// restart queues a restart of the service's live version.
func (d *svcDeps) restart(ctx context.Context, c actions.Call, in ServiceRefIn) (RestartOut, error) {
	if d.Jobs == nil {
		return RestartOut{}, actions.Unavailable("restarts are not available on this server")
	}
	r, err := d.load(ctx, d.DB.R, in.Service)
	if err != nil {
		return RestartOut{}, err
	}
	if !r.activeDeploy.Valid {
		return RestartOut{}, actions.Conflict("%s has no live version to restart; deploy it first", in.Service)
	}
	if r.attention.Valid && r.attention.String == AttentionVersionsKept {
		// The stopped previous container would be started beside the kept candidate.
		return RestartOut{}, actions.Conflict("%s", versionsKeptMessage)
	}
	// timeout_s is the recorded budget (the stop plus the health gate); like every
	// non-deploy job it is not enforced: each step has its own bound.
	payload, _ := json.Marshal(restartPayload{Service: r.id})
	id, err := d.Jobs.Enqueue(ctx, jobs.NewJob{
		Kind: jobs.KindRestart, LockKey: jobs.LockKey(r.id), Actor: jobs.ActorOf(c.Actor), Payload: payload,
		MaxAttempts: 1, TimeoutS: jobs.RestartTimeoutSeconds(r.graceS, r.timeoutS),
	})
	switch {
	case errors.Is(err, jobs.ErrServiceDeleting):
		return RestartOut{}, actions.Conflict("the service is being deleted")
	case errors.Is(err, jobs.ErrActorSessionGone):
		return RestartOut{}, &actions.DeniedError{Reason: SessionGoneMessage}
	case errors.Is(err, jobs.ErrInvalid):
		return RestartOut{}, actions.InvalidInput("%v", strings.TrimPrefix(err.Error(), "jobs: invalid request: "))
	case err != nil:
		return RestartOut{}, err
	}
	d.ensureRoutes(ctx)
	return RestartOut{Service: r.project + "/" + r.name, Job: id, Status: jobs.StateQueued}, nil
}

// runRestart is jobs.run_restart's Execute.
func (r *Runner) runRestart(ctx context.Context, _ actions.Call, jobID int64) (RunRestartOut, error) {
	if r.cfg.Ops == nil || r.cfg.Tracker == nil {
		return RunRestartOut{}, errors.New("restarts are not wired on this server")
	}
	j, err := r.cfg.Jobs.Get(ctx, jobID)
	if err != nil {
		return RunRestartOut{}, errors.New("the job could not be read")
	}
	var p restartPayload
	if err := agent.DecodeArgs(j.Payload, &p); err != nil || p.Service < 1 {
		return RunRestartOut{}, errors.New("the job has no service")
	}
	unlock, err := r.cfg.Locks.Lock(ctx, p.Service)
	if err != nil {
		return RunRestartOut{}, context.Cause(ctx)
	}
	defer unlock()
	if serviceVersionsKept(ctx, r.cfg.DB.R, p.Service) {
		// Queued before a stop-first deploy left two versions: starting the stopped
		// container now would run both. Nothing is touched.
		return RunRestartOut{}, errors.New(versionsKeptMessage)
	}

	var (
		project, name, raw string
		active             int64
		ip                 sql.NullString
	)
	err = r.cfg.DB.R.QueryRowContext(ctx, `SELECT p.name, s.name, s.active_deploy_id, d.ip, d.snapshot_json FROM services s
		JOIN projects p ON p.id = s.project_id JOIN deploys d ON d.id = s.active_deploy_id WHERE s.id = ?`, p.Service).Scan(&project, &name, &active, &ip, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRestartOut{}, errors.New("the service has no live version to restart (it was deleted or never deployed)")
	}
	if err != nil {
		return RunRestartOut{}, errors.New("the service could not be read")
	}
	snap, err := ParseSnapshot(raw)
	if err != nil {
		return RunRestartOut{}, errors.New("the live version's settings could not be read: " + agent.CleanText(err.Error(), 200))
	}
	out := RunRestartOut{Service: project + "/" + name}
	rn := &run{r: r, job: j, jobCtx: ctx, snap: snap, attempt: j.Attempt,
		svc: svcInfo{id: p.Service, project: project, name: name, hasActive: true, activeDeploy: active}}

	// The live version's container(s): a fresh list, each inspected and trusted only
	// when it is ours and carries this service and its live deploy.
	ids, lerr := rn.oldContainers(ctx, active)
	if lerr != nil {
		return out, errors.New("Docker could not look for the container: " + opText(lerr, "no answer"))
	}
	o := &oldVersion{ip: ip.String, port: snap.ContainerPort, health: snap.Health}
	for _, id := range ids {
		st, err := r.cfg.Ops.ContainerInspect(ctx, id)
		rn.op(ctx, err)
		if err != nil {
			if agentops.IsCode(err, agent.CodeNotFound) {
				continue
			}
			return out, errors.New("Docker could not inspect the container: " + opText(err, "no answer"))
		}
		if rn.oldOurs(st, id) {
			o.ids = append(o.ids, id)
		}
	}
	if len(o.ids) == 0 {
		return out, errors.New("the live version's container is missing; use Redeploy to create it again")
	}
	rn.old = o

	grace := snap.StopGraceS
	for _, id := range o.ids {
		// Not cancellable: a cancel in the middle would leave the app SIGTERMed and
		// dying. It runs to its own bound; the cancel is read right after.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(grace)*time.Second+StopExtra)
		err := r.cfg.Ops.ContainerRestart(sctx, id, grace)
		rn.op(sctx, err)
		cancel()
		if err == nil && ctx.Err() != nil {
			rn.trackOld(context.WithoutCancel(ctx))
			return out, context.Cause(ctx)
		}
		if err != nil {
			rn.trackOld(context.WithoutCancel(ctx))
			if ctx.Err() != nil {
				return out, context.Cause(ctx)
			}
			return out, errors.New("the container could not be restarted: " + opText(err, "Docker did not answer"))
		}
	}
	reason, unverified := rn.gateOld(ctx)
	if unverified {
		reason = "ServerDock was stopping before it could check the app"
	}
	if reason != "" {
		rn.trackOld(context.WithoutCancel(ctx))
		if ctx.Err() != nil {
			return out, context.Cause(ctx)
		}
		return out, fmt.Errorf("the app was restarted but is not healthy: %s", reason)
	}
	rn.trackOld(ctx)
	rn.soft("the service's attention flag", func(ctx context.Context, tx *sql.Tx) error {
		// The previous version is serving again: a flag that said it was down is stale.
		_, err := tx.ExecContext(ctx, `UPDATE services SET needs_attention = NULL WHERE id = ? AND needs_attention = ?`, p.Service, AttentionPreviousDown)
		return err
	})
	out.Status = "restarted"
	return out, nil
}
