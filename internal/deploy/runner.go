package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// ActorRefresher re-checks, at run time, the actor that queued a job against the
// database: *auth.Store (RefreshJobActor).
type ActorRefresher interface {
	RefreshJobActor(ctx context.Context, a actions.Actor, enqueuedAt time.Time) (actions.Actor, error)
}

// RunnerConfig is what the deploy runner needs.
type RunnerConfig struct {
	DB      *store.Store
	Jobs    *jobs.Store
	Inv     *actions.SystemInvoker
	Refresh ActorRefresher
	Locks   *ServiceLocks
	Log     *slog.Logger
}

// Runner runs the jobs of the scheduler (jobs.RunFunc). Every run, and every
// retry attempt, starts the same way (plan 0004 Revision 7): rebuild the actor
// that queued the job from its stored blob, refresh it against the database, and
// only then invoke the JobOnly action for the job's kind as that actor. A job is
// never run on an error from either step.
type Runner struct {
	cfg RunnerConfig
	// exec is what jobs.run_deploy does once its policy passed; tests replace it.
	exec func(ctx context.Context, c actions.Call, job int64) (RunDeployOut, error)
}

// NewRunner returns a Runner; it panics on a missing dependency (a wiring bug).
func NewRunner(cfg RunnerConfig) *Runner {
	if cfg.DB == nil || cfg.Jobs == nil || cfg.Inv == nil || cfg.Refresh == nil || cfg.Locks == nil {
		panic("deploy: NewRunner needs DB, Jobs, Inv, Refresh and Locks")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	r := &Runner{cfg: cfg}
	r.exec = r.runDeploy
	return r
}

// RunDeployIn is the input of jobs.run_deploy.
type RunDeployIn struct {
	Job int64 `json:"job"`
}

// Validate implements actions.Validator.
func (in RunDeployIn) Validate() error {
	if in.Job < 1 {
		return errors.New("job must be a job id")
	}
	return nil
}

// RunDeployOut is what jobs.run_deploy returns (the audit outcome's output).
type RunDeployOut struct {
	Deploy int    `json:"deploy"`
	Status string `json:"status"`
}

// Run is the jobs.RunFunc. The action it invokes is registered by
// RegisterJobActions; its policy is the queuing actor's, now.
func (r *Runner) Run(ctx context.Context, j jobs.Job) error {
	var action string
	switch j.Kind {
	case jobs.KindDeploy:
		action = "jobs.run_deploy"
	default:
		// Restart and delete are step 9.
		return fmt.Errorf("deploy: jobs of kind %q are not supported yet", j.Kind)
	}
	actor, err := r.actorFor(ctx, j)
	if err != nil {
		return err
	}
	in, _ := json.Marshal(RunDeployIn{Job: j.ID})
	_, err = r.cfg.Inv.InvokeAs(ctx, actor, action, in, actions.Options{Confirm: j.Confirm, RequestID: jobs.RequestID(j.ID, j.Attempt)})
	return err
}

// actorFor rebuilds and refreshes the job's actor. It is called before EVERY run
// and every retry attempt; an error from either step is returned and nothing runs.
func (r *Runner) actorFor(ctx context.Context, j jobs.Job) (actions.Actor, error) {
	actor, err := j.RebuildActor(r.cfg.Inv)
	if err != nil {
		r.cfg.Log.Warn("a job's actor could not be rebuilt; the job does not run", "job", j.ID, "err", err.Error())
		return actions.Actor{}, fmt.Errorf("denied: the job's actor could not be rebuilt")
	}
	actor, err = r.cfg.Refresh.RefreshJobActor(ctx, actor, j.EnqueuedAt)
	if err != nil {
		return actions.Actor{}, err
	}
	return actor, nil
}

// RegisterJobActions registers the JobOnly actions the runner invokes.
func RegisterJobActions(reg *actions.Registry, r *Runner) {
	actions.Register(reg, actions.Def[RunDeployIn, RunDeployOut]{
		Name: "jobs.run_deploy", Summary: "Run a queued deploy (only the job runner can invoke this, as the actor that queued it).",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, JobOnly: true,
		Target: func(in RunDeployIn) string { return fmt.Sprintf("job:%d", in.Job) },
		Execute: func(ctx context.Context, c actions.Call, in RunDeployIn) (RunDeployOut, error) {
			return r.exec(ctx, c, in.Job)
		},
	})
}

func (r *Runner) runDeploy(ctx context.Context, _ actions.Call, jobID int64) (RunDeployOut, error) {
	return RunDeployOut{}, errors.New("deploys are not available yet")
}
