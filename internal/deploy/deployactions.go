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
	Enqueue(ctx context.Context, n jobs.NewJob) (int64, error)
	EnqueueDelete(ctx context.Context, n jobs.NewDelete) (jobs.Deleted, error)
	CancelDeploy(ctx context.Context, id int64, reason string, forbidden []string) (string, error)
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
	// In the enqueue's transaction: the service must still have the image this
	// request read (an admin's services.set_source in between wins: a write token
	// cannot undo it), and a new tag becomes the service's desired image.
	setImage := func(ctx context.Context, tx *sql.Tx) error {
		var cur string
		if err := tx.QueryRowContext(ctx, `SELECT image_ref FROM services WHERE id = ?`, r.id).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return actions.NotFound("the service no longer exists")
			}
			return err
		}
		if cur != r.imageRef {
			return actions.Conflict("the service's image was changed while this deploy was being queued; try again")
		}
		if image == r.imageRef {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE services SET image_ref = ?, updated_at = ? WHERE id = ? AND image_ref = ?`, image, d.stamp(), r.id, r.imageRef)
		return err
	}
	out, err := d.Jobs.EnqueueDeploy(ctx, jobs.NewDeploy{
		ServiceID: r.id, Kind: "deploy", Snapshot: raw, Settings: snap.Settings(),
		Actor: jobs.ActorOf(c.Actor), InTx: setImage,
	})
	switch {
	case errors.Is(err, jobs.ErrServiceDeleting):
		return DeployOut{}, actions.Conflict("the service is being deleted")
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

// RollbackIn is the input of services.rollback.
type RollbackIn struct {
	Service string `json:"service" doc:"<project>/<service>"`
	To      int    `json:"to" doc:"the number of an earlier deploy that was once live"`
}

// Validate implements actions.Validator.
func (in RollbackIn) Validate() error {
	if _, _, err := splitRef(in.Service); err != nil {
		return err
	}
	if in.To < 1 {
		return errors.New("to must be a deploy number (1 or more)")
	}
	return nil
}

// sourceDeploy is a deploy row whose snapshot a redeploy or rollback copies.
type sourceDeploy struct {
	id                  int64
	number              int
	snapshot            string
	imageID, repoDigest sql.NullString
	committed           sql.NullString
}

func (d *svcDeps) sourceByNumber(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, service int64, number int) (sourceDeploy, error) {
	var s sourceDeploy
	err := q.QueryRowContext(ctx, `SELECT id, number, snapshot_json, image_id, repo_digest, committed_at FROM deploys WHERE service_id = ? AND number = ?`,
		service, number).Scan(&s.id, &s.number, &s.snapshot, &s.imageID, &s.repoDigest, &s.committed)
	return s, err
}

// pinned is a copy of snapshot raw that runs the image of the source deploy: its
// image id, and its repo digest (when it is a valid digest of the snapshot's
// repository) for the case the local image was removed.
func pinned(raw string, imageID, digest sql.NullString) (Snapshot, error) {
	snap, err := ParseSnapshot(raw)
	if err != nil {
		return Snapshot{}, actions.Conflict("that deploy's settings can no longer be read: %v", err)
	}
	if !imageID.Valid || !imageIDShape(imageID.String) {
		return Snapshot{}, actions.Conflict("that deploy has no recorded image, so it cannot be run again")
	}
	snap.ImageID = imageID.String
	if digest.Valid && digest.String != "" {
		with := snap
		with.RepoDigest = digest.String
		if with.Validate() == nil {
			snap = with
		}
	}
	return snap, snap.Validate()
}

// queueFrom enqueues a deploy of kind redeploy or rollback with snapshot snap. The
// service's desired image is NOT changed (a later plain deploy still follows it).
// check runs in the enqueue's transaction, so a service that changed under the
// request refuses it.
func (d *svcDeps) queueFrom(ctx context.Context, c actions.Call, r svcRow, kind string, source int64, snap Snapshot, check func(ctx context.Context, tx *sql.Tx) error) (DeployOut, error) {
	if d.Jobs == nil {
		return DeployOut{}, actions.Unavailable("deploys are not available on this server")
	}
	raw, err := snap.Marshal()
	if err != nil {
		return DeployOut{}, err
	}
	out, err := d.Jobs.EnqueueDeploy(ctx, jobs.NewDeploy{
		ServiceID: r.id, Kind: kind, SourceDeployID: source, Snapshot: raw, Settings: snap.Settings(),
		Actor: jobs.ActorOf(c.Actor), InTx: check,
	})
	switch {
	case errors.Is(err, jobs.ErrServiceDeleting):
		return DeployOut{}, actions.Conflict("the service is being deleted")
	case errors.Is(err, jobs.ErrActorSessionGone):
		return DeployOut{}, &actions.DeniedError{Reason: SessionGoneMessage}
	case errors.Is(err, jobs.ErrInvalid):
		return DeployOut{}, actions.InvalidInput("%v", strings.TrimPrefix(err.Error(), "jobs: invalid request: "))
	case err != nil:
		return DeployOut{}, err
	}
	d.ensureRoutes(ctx)
	return DeployOut{Service: r.project + "/" + r.name, Deploy: out.Number, Job: out.JobID, Status: jobs.StateQueued, Superseded: len(out.Superseded)}, nil
}

// stillActive is the transaction check of a redeploy: the live version is still the
// one whose snapshot was copied.
func stillActive(service, deploy int64) func(ctx context.Context, tx *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		var cur sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT active_deploy_id FROM services WHERE id = ?`, service).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return actions.NotFound("the service no longer exists")
			}
			return err
		}
		if !cur.Valid || cur.Int64 != deploy {
			return actions.Conflict("the live version changed while this request was being queued; try again")
		}
		return nil
	}
}

// redeploy runs the live version's image again without a rebuild: a new deploy
// whose snapshot copies the live deploy's, pinned to its image id, with a new
// address and the service's current strategy. It recreates a container that went
// missing, and does not touch the service's desired image.
func (d *svcDeps) redeploy(ctx context.Context, c actions.Call, in ServiceRefIn) (DeployOut, error) {
	r, err := d.load(ctx, d.DB.R, in.Service)
	if err != nil {
		return DeployOut{}, err
	}
	if !r.activeDeploy.Valid {
		return DeployOut{}, actions.Conflict("%s has no live version to redeploy; deploy it first", in.Service)
	}
	src, err := d.sourceByNumber(ctx, d.DB.R, r.id, int(r.activeNumber.Int64))
	if err != nil {
		return DeployOut{}, err
	}
	snap, err := pinned(src.snapshot, src.imageID, src.repoDigest)
	if err != nil {
		return DeployOut{}, err
	}
	snap.Strategy = r.strategy
	if err := snap.Validate(); err != nil {
		return DeployOut{}, actions.InvalidInput("%v", err)
	}
	return d.queueFrom(ctx, c, r, "redeploy", src.id, snap, stillActive(r.id, src.id))
}

// rollback runs an earlier version again: a new deploy of kind rollback with the
// snapshot of deploy `to`, pinned to its image id. Only deploys that were once live
// (committed_at set) are targets: a deploy that failed or never switched is refused.
func (d *svcDeps) rollback(ctx context.Context, c actions.Call, in RollbackIn) (DeployOut, error) {
	r, err := d.load(ctx, d.DB.R, in.Service)
	if err != nil {
		return DeployOut{}, err
	}
	src, err := d.sourceByNumber(ctx, d.DB.R, r.id, in.To)
	if errors.Is(err, sql.ErrNoRows) {
		return DeployOut{}, actions.NotFound("no deploy #%d of %s", in.To, in.Service)
	}
	if err != nil {
		return DeployOut{}, err
	}
	if !src.committed.Valid {
		return DeployOut{}, actions.InvalidInput("deploy #%d was never live, so it cannot be rolled back to (only versions that served traffic can)", in.To)
	}
	if r.activeDeploy.Valid && r.activeDeploy.Int64 == src.id {
		return DeployOut{}, actions.Conflict("deploy #%d is already the live version; use Redeploy to run it again", in.To)
	}
	snap, err := pinned(src.snapshot, src.imageID, src.repoDigest)
	if err != nil {
		return DeployOut{}, err
	}
	return d.queueFrom(ctx, c, r, "rollback", src.id, snap, func(ctx context.Context, tx *sql.Tx) error {
		var committed sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT committed_at FROM deploys WHERE id = ? AND service_id = ?`, src.id, r.id).Scan(&committed); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return actions.NotFound("the service or the deploy no longer exists")
			}
			return err
		}
		if !committed.Valid {
			return actions.InvalidInput("deploy #%d was never live, so it cannot be rolled back to", in.To)
		}
		return nil
	})
}

// cancel ends a queued deploy at once and flags a running one. A deploy that is
// already finished is a conflict; one inside switch or commit cannot be cancelled
// (409 not_cancellable: the runner writes the stage in the same transaction that checks the cancel flag, see enterSwitch).
func (d *svcDeps) cancel(ctx context.Context, in CancelIn) (CancelOut, error) {
	project, name, err := splitRef(in.Service)
	if err != nil {
		return CancelOut{}, actions.InvalidInput("%v", err)
	}
	var (
		jobID  sql.NullInt64
		status string
	)
	err = d.DB.R.QueryRowContext(ctx, `SELECT d.job_id, d.status FROM deploys d JOIN services s ON s.id = d.service_id
		JOIN projects p ON p.id = s.project_id WHERE p.name = ? AND s.name = ? AND d.number = ?`, project, name, in.Number).Scan(&jobID, &status)
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
	if d.Jobs == nil {
		return CancelOut{}, actions.Unavailable("deploys are not available on this server")
	}
	// The stage check is made again, atomically with the flag, by the store.
	state, err := d.Jobs.CancelDeploy(ctx, jobID.Int64, "cancelled", nonCancellableStages)
	switch {
	case errors.Is(err, jobs.ErrStageNotCancellable):
		return CancelOut{}, actions.Conflict("deploy #%d is switching traffic and cannot be cancelled now (not_cancellable)", in.Number)
	case errors.Is(err, jobs.ErrNotCancellable):
		return CancelOut{}, actions.Conflict("deploy #%d has already finished", in.Number)
	case err != nil:
		return CancelOut{}, err
	}
	out := CancelOut{Service: in.Service, Number: in.Number, Status: "cancelling"}
	if state == jobs.StateCancelled { // the state the store reports, not the one read before
		out.Status = "cancelled"
	}
	return out, nil
}

// nonCancellableStages are the stages a cancel cannot reach.
var nonCancellableStages = []string{StageSwitch, StageCommit, StageDrain, StageStopOld, StageDone}

func registerDeployActions(r *actions.Registry, d *svcDeps) {
	actions.Register(r, actions.Def[ServiceDeployIn, DeployOut]{
		Name: "services.deploy", Summary: "Queue a deploy of a service, optionally of another tag or digest of its current repository. A newer deploy replaces older queued ones.",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, Target: func(in ServiceDeployIn) string { return in.Service },
		Execute: func(ctx context.Context, c actions.Call, in ServiceDeployIn) (DeployOut, error) {
			return d.deploy(ctx, c, in)
		},
	})
	refTarget := func(in ServiceRefIn) string { return in.Service }
	actions.Register(r, actions.Def[ServiceRefIn, DeployOut]{
		Name: "services.redeploy", Summary: "Run the live version's image again without rebuilding: a new deploy with a new address and the service's strategy. It also recreates a container that went missing.",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, Target: refTarget,
		Execute: func(ctx context.Context, c actions.Call, in ServiceRefIn) (DeployOut, error) {
			return d.redeploy(ctx, c, in)
		},
	})
	actions.Register(r, actions.Def[RollbackIn, DeployOut]{
		Name: "services.rollback", Summary: "Run an earlier version again (only a deploy that was once live), pinned to its image.",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, Target: func(in RollbackIn) string { return in.Service },
		Execute: func(ctx context.Context, c actions.Call, in RollbackIn) (DeployOut, error) {
			return d.rollback(ctx, c, in)
		},
	})
	actions.Register(r, actions.Def[ServiceRefIn, DeleteOut]{
		Name: "services.delete", Summary: "Delete a service: cancel its queued work, stop and remove its containers, delete its deploy logs and records (and its project if it was the last service). Needs fresh two-factor confirmation and the exact <project>/<service> as Confirm; API tokens are always denied.",
		Risk: actions.RiskDestructive, Scope: actions.ScopeAdmin, Target: refTarget,
		Execute: func(ctx context.Context, c actions.Call, in ServiceRefIn) (DeleteOut, error) {
			return d.delete(ctx, c, in)
		},
	})
	actions.Register(r, actions.Def[ServiceRefIn, RestartOut]{
		Name: "services.restart", Summary: "Restart the live version's container. It causes a short outage (Redeploy avoids one); the address and route stay.",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, Target: refTarget,
		Execute: func(ctx context.Context, c actions.Call, in ServiceRefIn) (RestartOut, error) {
			return d.restart(ctx, c, in)
		},
	})
	actions.Register(r, actions.Def[CancelIn, CancelOut]{
		Name: "deploys.cancel", Summary: "Cancel a queued deploy, or ask a running one to stop at its next safe point (not while it is switching traffic).",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, Target: func(in CancelIn) string { return in.Service },
		Execute: func(ctx context.Context, _ actions.Call, in CancelIn) (CancelOut, error) { return d.cancel(ctx, in) },
	})
}
