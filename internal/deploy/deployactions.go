package deploy

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// This file is the queuing side of a deploy (plan 0004 step 8a): services.deploy,
// the deploy that services.create can start, and deploys.cancel. The runner
// (runner.go) is the other side; nothing here touches Docker.

// SessionGoneMessage is what a caller whose session was signed out between the
// authorisation and the enqueue is told (jobs.ErrActorSessionGone).
const SessionGoneMessage = "your session was signed out; sign in again"

// DeployQueue is the scheduler's queuing side: *jobs.Scheduler.
type DeployQueue interface {
	EnqueueDeploy(ctx context.Context, n jobs.NewDeploy) (jobs.Enqueued, error)
	Cancel(ctx context.Context, id int64, reason string) error
}

// DeployOut is the output of services.deploy (and the "deploy" part of
// services.create with deploy:true): the deploy was queued, not run.
type DeployOut struct {
	Service string `json:"service" doc:"<project>/<service>"`
	Deploy  int    `json:"deploy" doc:"the deploy's number within the service"`
	Job     int64  `json:"job"`
	Status  string `json:"status" doc:"queued: the deploy runs when the scheduler picks it; poll deploys.get"`
	// Superseded is how many queued deploys of this service this one replaced.
	Superseded int `json:"superseded,omitempty"`
}

// ServiceDeployIn is the input of services.deploy.
type ServiceDeployIn struct {
	Service  string  `json:"service" doc:"<project>/<service>"`
	ImageRef *string `json:"image_ref,omitempty" doc:"another tag or digest of the service's current repository (changing the repository needs services.set_source)"`
}

// Validate implements actions.Validator.
func (in ServiceDeployIn) Validate() error {
	if _, _, err := splitRef(in.Service); err != nil {
		return err
	}
	if in.ImageRef != nil {
		if _, err := policy.ParseRef(*in.ImageRef); err != nil {
			return errors.New("image_ref: " + err.Error())
		}
	}
	return nil
}

// CancelIn is the input of deploys.cancel.
type CancelIn struct {
	Service string `json:"service" doc:"<project>/<service>"`
	Number  int    `json:"number" doc:"the deploy's number"`
}

// Validate implements actions.Validator.
func (in CancelIn) Validate() error {
	if _, _, err := splitRef(in.Service); err != nil {
		return err
	}
	if in.Number < 1 {
		return errors.New("number must be 1 or more")
	}
	return nil
}

// CancelOut is the output of deploys.cancel.
type CancelOut struct {
	Service string `json:"service"`
	Number  int    `json:"number"`
	// Status is "cancelled" (a queued deploy, ended now) or "cancelling" (a running
	// one, which stops at its next cancellable point).
	Status string `json:"status"`
}

func (d *svcDeps) snapshotOf(r svcRow, image string) Snapshot {
	s := Snapshot{
		V: SnapshotVersion, ImageRef: image,
		Health:   SnapHealth{Kind: r.healthKind, Path: r.healthPath.String, TimeoutS: r.timeoutS, Passes: r.passes},
		Strategy: r.strategy, DrainS: r.drainS, StopGraceS: r.graceS,
	}
	if r.containerPort.Valid {
		s.ContainerPort = int(r.containerPort.Int64)
	}
	return s
}

// queueDeploy enqueues a deploy of service row r with image (the reference the
// snapshot records) as c.Actor, in one transaction that also records the new
// image reference as the service's desired one (so a later plain deploy does not
// go back to the old tag). The caller has checked the image against the policy.
func (d *svcDeps) queueDeploy(ctx context.Context, c actions.Call, r svcRow, image string) (DeployOut, error) {
	if d.Jobs == nil {
		return DeployOut{}, actions.Unavailable("deploys are not available on this server")
	}
	snap := d.snapshotOf(r, image)
	if err := snap.Validate(); err != nil {
		return DeployOut{}, actions.InvalidInput("%v", err)
	}
	raw, err := snap.Marshal()
	if err != nil {
		return DeployOut{}, err
	}
	setImage := func(ctx context.Context, tx *sql.Tx) error {
		if image == r.imageRef {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE services SET image_ref = ?, updated_at = ? WHERE id = ?`, image, d.stamp(), r.id)
		return err
	}
	out, err := d.Jobs.EnqueueDeploy(ctx, jobs.NewDeploy{
		ServiceID: r.id, Kind: "deploy", Snapshot: raw, Settings: snap.Settings(),
		Actor: jobs.ActorOf(c.Actor), InTx: setImage,
	})
	switch {
	case errors.Is(err, jobs.ErrActorSessionGone):
		return DeployOut{}, &actions.DeniedError{Reason: SessionGoneMessage}
	case errors.Is(err, jobs.ErrInvalid):
		return DeployOut{}, actions.InvalidInput("%v", strings.TrimPrefix(err.Error(), "jobs: invalid request: "))
	case err != nil:
		return DeployOut{}, err
	}
	// Docker may have been down at start: let this request open the boot gate
	// (bounded, single-flight) so the scheduler can start the job.
	d.ensureRoutes(ctx)
	return DeployOut{Service: r.project + "/" + r.name, Deploy: out.Number, Job: out.JobID, Status: jobs.StateQueued, Superseded: len(out.Superseded)}, nil
}

func (d *svcDeps) deploy(ctx context.Context, c actions.Call, in ServiceDeployIn) (DeployOut, error) {
	cur, err := d.load(ctx, d.DB.R, in.Service)
	if err != nil {
		return DeployOut{}, err
	}
	image := cur.imageRef
	if in.ImageRef != nil {
		pull, err := d.checkImage(*in.ImageRef)
		if err != nil {
			return DeployOut{}, err
		}
		// The write-token rule (QM3a-4): a tag or digest of the repository the
		// service already deploys, never another repository or registry.
		if !policy.SameRepository(cur.imageRef, pull) {
			return DeployOut{}, actions.InvalidInput("changing the image repository needs services.set_source")
		}
		image = pull
	}
	return d.queueDeploy(ctx, c, cur, image)
}

// cancel ends a queued deploy at once and flags a running one. A deploy that is
// already finished is a conflict; one inside switch or commit cannot be cancelled
// (409 not_cancellable, wired with the switch in step 8b).
func (d *svcDeps) cancel(ctx context.Context, in CancelIn) (CancelOut, error) {
	project, name, err := splitRef(in.Service)
	if err != nil {
		return CancelOut{}, actions.InvalidInput("%v", err)
	}
	var (
		jobID  sql.NullInt64
		status string
		stage  sql.NullString
	)
	err = d.DB.R.QueryRowContext(ctx, `SELECT d.job_id, d.status, d.stage FROM deploys d JOIN services s ON s.id = d.service_id
		JOIN projects p ON p.id = s.project_id WHERE p.name = ? AND s.name = ? AND d.number = ?`, project, name, in.Number).Scan(&jobID, &status, &stage)
	if errors.Is(err, sql.ErrNoRows) {
		return CancelOut{}, actions.NotFound("no deploy #%d of %s/%s", in.Number, project, name)
	}
	if err != nil {
		return CancelOut{}, err
	}
	if status != "queued" && status != "running" {
		return CancelOut{}, actions.Conflict("deploy #%d has already finished (%s)", in.Number, status)
	}
	if !jobID.Valid {
		return CancelOut{}, actions.Conflict("deploy #%d has no job to cancel", in.Number)
	}
	if status == "running" && stage.Valid && !CancellableStage(stage.String) {
		return CancelOut{}, actions.Conflict("deploy #%d is switching traffic and cannot be cancelled now (not_cancellable)", in.Number)
	}
	if d.Jobs == nil {
		return CancelOut{}, actions.Unavailable("deploys are not available on this server")
	}
	if err := d.Jobs.Cancel(ctx, jobID.Int64, "cancelled"); err != nil {
		if errors.Is(err, jobs.ErrNotCancellable) {
			return CancelOut{}, actions.Conflict("deploy #%d has already finished", in.Number)
		}
		return CancelOut{}, err
	}
	out := CancelOut{Service: in.Service, Number: in.Number, Status: "cancelling"}
	if status == "queued" {
		out.Status = "cancelled"
	}
	return out, nil
}

func registerDeployActions(r *actions.Registry, d *svcDeps) {
	actions.Register(r, actions.Def[ServiceDeployIn, DeployOut]{
		Name: "services.deploy", Summary: "Queue a deploy of a service, optionally of another tag or digest of its current repository. A newer deploy replaces older queued ones.",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, Target: func(in ServiceDeployIn) string { return in.Service },
		Execute: func(ctx context.Context, c actions.Call, in ServiceDeployIn) (DeployOut, error) {
			return d.deploy(ctx, c, in)
		},
	})
	actions.Register(r, actions.Def[CancelIn, CancelOut]{
		Name: "deploys.cancel", Summary: "Cancel a queued deploy, or ask a running one to stop at its next safe point (not while it is switching traffic).",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, Target: func(in CancelIn) string { return in.Service },
		Execute: func(ctx context.Context, _ actions.Call, in CancelIn) (CancelOut, error) { return d.cancel(ctx, in) },
	})
}
