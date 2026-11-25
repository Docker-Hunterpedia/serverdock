package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
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

	// The deploy pipeline's dependencies (all required to run a deploy; the actor
	// hook above works without them).
	Ops      RunnerOps
	Alloc    AddressAllocator
	Tracker  StateTracker
	Routes   RouteControl // the route calls of the switch and the commit (step 8b); *Routes
	Net      NetControl
	AppNet   SubnetSource
	Instance string // the install id: it names the app network
	Policy   policy.Options

	// Tunables (zero means the default; tests shorten them).
	Now                  func() time.Time
	HealthInterval       time.Duration // 1 s between health checks
	StallTimeout         time.Duration // the pull's stall limit (0: the agent's 120 s)
	PreSwitchBound       time.Duration // pull + check + start: 10 min
	Watchdog             time.Duration // overrides jobs.timeout_s (tests only)
	CleanupBound         time.Duration // candidate removal and the end-of-deploy writes: 30 s
	ShutdownCleanupBound time.Duration // the same at shutdown: 5 s

	// Step 8b (switch.go). Zero means the plan's value; tests shorten them.
	SwitchBound time.Duration // the switch, route lock wait included: 30 s
	CommitBound time.Duration // the commit, route lock wait included: 10 s
	DrainUnit   time.Duration // one second of drain_s: 1 s
	// KeepLog is the deploy-log hook of step 11: it is called with the previous
	// container's id before that container is stopped, to keep the tail of its
	// output in its deploy's log. Nil until step 11. A failure never fails the
	// deploy: the stage detail says "log not kept: <reason>".
	// Stopping, if set, returns a channel closed when the process is shutting down
	// (*jobs.Scheduler.Stopping). It is independent of the job's cancel cause, which
	// keeps only the first cause (a supersede hides a later shutdown).
	Stopping func() <-chan struct{}
	// DeleteSignal returns the channel a queued service delete closes for a running
	// job (*jobs.Scheduler.DeleteSignal): a deploy in its drain stops waiting.
	DeleteSignal func(job int64) <-chan struct{}
	// LogRoot is DATA/deploy-logs: a service delete removes <LogRoot>/<service id>.
	// Empty means no deploy logs are kept on this server.
	LogRoot string
	KeepLog func(ctx context.Context, deploy int64, container string) error
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
	// trace, if set (tests only), is told the phase boundaries of the switch, commit,
	// drain and stop_old, so a test can record the exact order against the ops.
	trace func(event string)

	progMu   sync.Mutex
	progress map[int64]agentops.PullProgress // latest pull progress of each running deploy; bounded (maxProgress), removed when the deploy ends
}

// maxProgress bounds the progress map: at most two jobs run at once, so it is
// never close; a deploy that would be the 17th is simply not tracked.
const maxProgress = 16

func (r *Runner) setProgress(deploy int64, p agentops.PullProgress) {
	r.progMu.Lock()
	defer r.progMu.Unlock()
	if _, ok := r.progress[deploy]; !ok && len(r.progress) >= maxProgress {
		return
	}
	r.progress[deploy] = p
}

func (r *Runner) progressOf(deploy int64) (agentops.PullProgress, bool) {
	r.progMu.Lock()
	defer r.progMu.Unlock()
	p, ok := r.progress[deploy]
	return p, ok
}

func (r *Runner) dropProgress(deploy int64) {
	r.progMu.Lock()
	delete(r.progress, deploy)
	r.progMu.Unlock()
}

// Progress is the latest pull progress of a running deploy (for deploys.get, step
// 11); ok is false when the deploy is not pulling.
func (r *Runner) Progress(deploy int64) (agentops.PullProgress, bool) { return r.progressOf(deploy) }

// NewRunner returns a Runner; it panics on a missing dependency (a wiring bug).
func NewRunner(cfg RunnerConfig) *Runner {
	if cfg.DB == nil || cfg.Jobs == nil || cfg.Inv == nil || cfg.Refresh == nil || cfg.Locks == nil {
		panic("deploy: NewRunner needs DB, Jobs, Inv, Refresh and Locks")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HealthInterval == 0 {
		cfg.HealthInterval = time.Second
	}
	if cfg.PreSwitchBound == 0 {
		cfg.PreSwitchBound = 10 * time.Minute
	}
	if cfg.CleanupBound == 0 {
		cfg.CleanupBound = 30 * time.Second
	}
	if cfg.ShutdownCleanupBound == 0 {
		cfg.ShutdownCleanupBound = 5 * time.Second
	}
	if cfg.SwitchBound == 0 {
		cfg.SwitchBound = SwitchBound
	}
	if cfg.CommitBound == 0 {
		cfg.CommitBound = CommitBound
	}
	if cfg.DrainUnit == 0 {
		cfg.DrainUnit = time.Second
	}
	r := &Runner{cfg: cfg, progress: map[int64]agentops.PullProgress{}}
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
	case jobs.KindRestart:
		action = "jobs.run_restart"
	case jobs.KindServiceDelete:
		action = "jobs.run_service_delete"
	default:
		return fmt.Errorf("deploy: jobs of kind %q are not supported", j.Kind)
	}
	in, _ := json.Marshal(RunDeployIn{Job: j.ID})
	if j.Kind == jobs.KindServiceDelete {
		// The target (the Confirm value) is in the job's payload.
		var p struct {
			Service int64  `json:"service"`
			Target  string `json:"target"`
		}
		if err := agent.DecodeArgs(j.Payload, &p); err != nil {
			return errors.New("the delete job has no target")
		}
		in, _ = json.Marshal(RunDeleteIn{Job: j.ID, Service: p.Target})
	}
	reqID := jobs.RequestID(j.ID, j.Attempt)
	rebuilt, err := j.RebuildActor(r.cfg.Inv)
	if err != nil {
		// A bad blob or a blob that disagrees with the row's kind, id and session is a
		// tamper signal: audited as a denial (the actor is empty, so the row says
		// "invalid"; nothing from the blob is echoed), logged, and the deploy ends
		// denied through the one sentinel actions.ErrDenied.
		r.cfg.Log.Warn("a job's actor could not be rebuilt; the job does not run", "job", j.ID, "attempt", j.Attempt, "actor_kind", j.Actor.Kind)
		if aerr := r.cfg.Inv.RecordDenied(ctx, actions.Actor{}, action, in, "the job's stored actor is not valid", reqID); aerr != nil {
			r.cfg.Log.Error("the refusal could not be audited", "job", j.ID, "err", aerr.Error())
		}
		return fmt.Errorf("%w: the job's stored actor is not valid", actions.ErrDenied)
	}
	actor, err := r.cfg.Refresh.RefreshJobActor(ctx, rebuilt, j.EnqueuedAt)
	if err != nil {
		// The actor that queued the job may no longer run it (token revoked or
		// expired, credentials changed, no longer an administrator). Nothing runs. The
		// refusal is audited like any denial (the job's actor, executor system) and
		// logged; the job fails "denied" and the queue ends the deploy row with it
		// (jobs.Store.Finish), on this attempt and on a retry attempt alike.
		if errors.Is(err, actions.ErrDenied) {
			r.cfg.Log.Warn("a queued job was refused at run time: its actor may no longer run it", "job", j.ID, "attempt", j.Attempt, "actor_kind", j.Actor.Kind)
			if aerr := r.cfg.Inv.RecordDenied(ctx, rebuilt, action, in, agent.CleanText(err.Error(), 240), reqID); aerr != nil {
				r.cfg.Log.Error("the refusal could not be audited", "job", j.ID, "err", aerr.Error())
			}
		}
		return err
	}
	_, err = r.cfg.Inv.InvokeAs(ctx, actor, action, in, actions.Options{Confirm: j.Confirm, RequestID: reqID})
	var de *actions.DeniedError
	if j.Kind == jobs.KindServiceDelete && errors.As(err, &de) && de.Code == "mfa_required" {
		// The 5-minute fresh-2FA window passed while the service was stopping: nothing
		// was deleted, and the user is told to confirm again.
		return confirmAgainError{}
	}
	return err
}

// RegisterJobActions registers the JobOnly actions the runner invokes.
func RegisterJobActions(reg *actions.Registry, r *Runner) {
	actions.Register(reg, actions.Def[RunDeleteIn, RunDeleteOut]{
		Name: "jobs.run_service_delete", Summary: "Run a queued service delete (only the job runner can invoke this, as the actor that queued it; fresh two-factor confirmation is checked again).",
		Risk: actions.RiskDestructive, Scope: actions.ScopeAdmin, JobOnly: true,
		Target: func(in RunDeleteIn) string { return in.Service },
		Execute: func(ctx context.Context, c actions.Call, in RunDeleteIn) (RunDeleteOut, error) {
			return r.runDelete(ctx, c, in.Job)
		},
	})
	actions.Register(reg, actions.Def[RunRestartIn, RunRestartOut]{
		Name: "jobs.run_restart", Summary: "Run a queued restart (only the job runner can invoke this, as the actor that queued it).",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, JobOnly: true,
		Target: func(in RunRestartIn) string { return fmt.Sprintf("job:%d", in.Job) },
		Execute: func(ctx context.Context, c actions.Call, in RunRestartIn) (RunRestartOut, error) {
			return r.runRestart(ctx, c, in.Job)
		},
	})
	actions.Register(reg, actions.Def[RunDeployIn, RunDeployOut]{
		Name: "jobs.run_deploy", Summary: "Run a queued deploy (only the job runner can invoke this, as the actor that queued it).",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, JobOnly: true,
		Target: func(in RunDeployIn) string { return fmt.Sprintf("job:%d", in.Job) },
		Execute: func(ctx context.Context, c actions.Call, in RunDeployIn) (RunDeployOut, error) {
			return r.exec(ctx, c, in.Job)
		},
	})
}
