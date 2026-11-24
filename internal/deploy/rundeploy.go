package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// This file is the pre-switch half of a deploy (plan 0004 step 8a): pull, check,
// start and the health gate, with the per-service lock, the address allocation,
// supersede and cancel, transient retries, the pre-switch deadline and the
// failure clean-up. It NEVER calls proxy_apply and never touches a route: the
// switch, commit, drain and stop of the old version are step 8b (switch.go),
// entered through afterHealth, which this file only calls. The source-scan test
// keeps the route calls out of this file and out of runner.go.

// Failure codes a deploy ends with (deploys.reason_code), besides the allocator's
// (docker, ip_pool_exhausted, app_subnet).
const (
	CodePullFailed    = "pull_failed"
	CodePullStalled   = "pull_stalled"
	CodeDiskFull      = "disk_full"
	CodePolicy        = "policy"
	CodeNetwork       = "network"
	CodeNameConflict  = "name_conflict"
	CodeStartFailed   = "start_failed"
	CodeHealthTimeout = "health_timeout"
	CodeExited        = "exited"
	CodeOOM           = "oom"
	CodeSuperseded    = "superseded"
	CodeCancelled     = "cancelled"
	CodeShutdown      = "shutdown"
	CodeTimeout       = "timeout"
	CodeDB            = "db"
	CodeDenied        = "denied"
	CodeSwitchFailed  = "switch_failed" // the proxy could not switch traffic to the candidate (the old routes were put back)
	CodeCommitFailed  = "commit_failed" // the commit transaction failed (the old routes were put back)
	CodeBadSnapshot   = "bad_snapshot"
	CodeInternal      = "internal"
)

// app_safe values (deploys.app_safe).
const (
	AppSafeYes     = "yes"
	AppSafeNo      = "no"
	AppSafeUnknown = "unknown"
)

// Deploy statuses the runner sets (the queue sets queued and superseded-while-queued).
const (
	StatusRunning    = "running"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
	StatusSuperseded = "superseded"
)

var (
	errWatchdog    = errors.New("deploy: the job's time budget ran out before the switch")
	errPreSwitch   = errors.New("deploy: pull, check and start took longer than their bound")
	errShutdownCtx = jobs.ErrShutdown
)

// failure ends a deploy. It is an error so a stage can return it directly.
type failure struct {
	code, msg string
	appSafe   string // "" means: yes when a version is live, else unknown
	status    string // "" means failed
	retry     bool   // transient: the scheduler may try again while attempts remain
	by        int64  // superseded_by
	noRecord  bool   // the deploy had already ended: write nothing (the end state is the store's)
	keep      bool   // do not remove the candidate: the routes may still point at it (a failed re-apply), or the commit's outcome is unknown
}

func (f *failure) Error() string { return f.msg }

// RunnerOps is what the runner asks of the agent: *agentops.Client. There is
// deliberately no proxy_apply here: the runner of step 8a cannot call it.
type RunnerOps interface {
	ImagePull(ctx context.Context, ref string, stall time.Duration, onProgress func(agentops.PullProgress)) (agentops.PullResult, error)
	ImageInspect(ctx context.Context, refOrID string, byID bool) (agentops.ImageInfo, error)
	ContainerCreate(ctx context.Context, name string, spec dockerapi.CreateSpec, ip, imageID string) (agentops.CreateResult, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerStop(ctx context.Context, id string, grace int) error
	ContainerRemove(ctx context.Context, id string) error
	ContainerInspect(ctx context.Context, id string) (agentops.ContainerState, error)
	ContainerList(ctx context.Context) (agentops.ListResult, error)
	HealthCheck(ctx context.Context, a agentops.HealthArgs) (agentops.HealthResult, error)
}

// StateTracker records the candidate in the control plane's state map without
// applying routes: *Routes (Track, Untrack, MarkUnknown, ClearUnknown).
type StateTracker interface {
	Track(c Container)
	Untrack(id string)
	State() *StateMap
}

// NetControl is the app network's status and its (re-)ensure: *Booter.
type NetControl interface {
	NetworkStatus() NetStatus
	Run(ctx context.Context) error
	NetworkGone()
}

// AddressAllocator is the allocator: *Allocator.
type AddressAllocator interface {
	Allocate(ctx context.Context, deployID int64) (netip.Addr, error)
}

// svcInfo is the service and project a deploy belongs to.
type svcInfo struct {
	id, projectID         int64
	project, name         string
	hasActive             bool
	activeDeploy          int64
	deployNumber          int
	status, snapshot, ip  string
	jobID                 sql.NullInt64
	kind                  string
	imageIDPin, digestPin string
}

// run is one execution (one attempt) of a deploy job.
type run struct {
	r       *Runner
	job     jobs.Job
	deploy  int64
	svc     svcInfo
	snap    Snapshot
	attempt int

	stage        string // the stage in progress ("" before the first)
	ip           string
	imageID      string
	digest       string
	subnet       string
	cid          string // the candidate's container id, once known
	createSent   bool   // a create was sent: the candidate may exist even if cid is ""
	started      bool
	failedDB     bool // a database write failed (logged once)
	switched     bool // the switch was started: the routes may point at the candidate
	reverted     bool // the routes were put back (revert ran)
	committed    bool // the commit transaction succeeded: from here the candidate is the live version and is never removed
	warnings     []string
	switchDetail string // the switch stage's detail, written in the commit transaction
	incomplete   bool   // a write after the commit failed ("record incomplete")
	jobCtx       context.Context
	snapErr      error
	create       agentops.CreateArgs
}

func (r *Runner) runDeploy(ctx context.Context, _ actions.Call, jobID int64) (out RunDeployOut, err error) {
	if r.cfg.Ops == nil || r.cfg.Alloc == nil || r.cfg.Net == nil || r.cfg.Tracker == nil || r.cfg.Routes == nil || r.cfg.AppNet == nil || r.cfg.Instance == "" {
		return RunDeployOut{}, errors.New("deploys are not wired on this server")
	}
	// An error that ends the run before the deploy is known (the job or the deploy
	// cannot be read) leaves the deploy row to the queue's backstop: when the job
	// ends failed, jobs.Store.Finish ends its deploy too.
	j, err := r.cfg.Jobs.Get(ctx, jobID)
	if err != nil {
		return RunDeployOut{}, errors.New("the job could not be read")
	}
	rn := &run{r: r, job: j, attempt: j.Attempt, jobCtx: ctx}
	if err := rn.load(ctx); err != nil {
		return RunDeployOut{}, err
	}
	unlock, err := r.cfg.Locks.Lock(ctx, rn.svc.id)
	if err != nil {
		return RunDeployOut{}, rn.finish(rn.ctxFailure(ctx))
	}
	defer unlock()
	defer r.dropProgress(rn.deploy)
	// A panic in a stage must not leave the candidate running or the row live: the
	// clean-up runs on a fresh context, the row ends, and the job fails. It is
	// registered after the lock, so it runs while the service lock is still held.
	defer func() {
		if p := recover(); p != nil {
			r.cfg.Log.Error("a deploy panicked", "job", jobID, "deploy", rn.deploy, "panic", p)
			out, err = RunDeployOut{Deploy: rn.svc.deployNumber, Status: rn.statusOf(&failure{})}, rn.finish(&failure{code: CodeInternal, appSafe: AppSafeUnknown,
				msg: "ServerDock hit an internal error while deploying; the new version was removed"})
		}
	}()

	f := rn.execute(ctx)
	return RunDeployOut{Deploy: rn.svc.deployNumber, Status: rn.statusOf(f)}, rn.finish(f)
}

// load reads the job's deploy, its service and snapshot.
func (rn *run) load(ctx context.Context) error {
	var payload struct {
		Deploy int64 `json:"deploy"`
	}
	if err := agent.DecodeArgs(rn.job.Payload, &payload); err != nil || payload.Deploy < 1 {
		return errors.New("the job has no deploy")
	}
	rn.deploy = payload.Deploy
	var s svcInfo
	var active sql.NullInt64
	err := rn.r.cfg.DB.R.QueryRowContext(ctx, `SELECT s.id, s.project_id, p.name, s.name, s.active_deploy_id, d.number, d.status, d.snapshot_json, d.kind,
		d.job_id FROM deploys d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id WHERE d.id = ?`, rn.deploy).
		Scan(&s.id, &s.projectID, &s.project, &s.name, &active, &s.deployNumber, &s.status, &s.snapshot, &s.kind, &s.jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("the deploy no longer exists")
	}
	if err != nil {
		return errors.New("the deploy could not be read")
	}
	if !s.jobID.Valid || s.jobID.Int64 != rn.job.ID {
		return errors.New("the deploy does not belong to this job")
	}
	s.hasActive, s.activeDeploy = active.Valid, active.Int64
	rn.svc = s
	snap, err := ParseSnapshot(s.snapshot)
	if err != nil {
		rn.snapErr = err
		return nil
	}
	rn.snap = snap
	return nil
}

func (rn *run) appSafe(f *failure) string {
	if f != nil && f.appSafe != "" {
		return f.appSafe
	}
	if rn.activeServing() {
		return AppSafeYes
	}
	return AppSafeUnknown
}

// activeServing: the state map shows the service's live deploy running (or
// restarting at its address). Without it app_safe is unknown, not yes: ServerDock
// does not claim the old version kept serving when it cannot see it.
func (rn *run) activeServing() bool {
	if !rn.svc.hasActive {
		return false
	}
	for _, c := range rn.r.cfg.Tracker.State().snapshot() {
		if c.Service == rn.svc.id && c.Deploy == rn.svc.activeDeploy && (c.State == StateRunning || c.State == StateRestarting) {
			return true
		}
	}
	return false
}

func (rn *run) statusOf(f *failure) string {
	if rn.committed {
		return StatusSucceeded // the commit is the point of no return: the deploy is succeeded whatever came after
	}
	if f == nil {
		return StatusFailed
	}
	if f.status != "" {
		return f.status
	}
	return StatusFailed
}

// execute runs the stages and returns why the deploy ended: a failure, or nil
// when it was committed (rn.committed) and ran through drain and stop_old.
func (rn *run) execute(ctx context.Context) *failure {
	if rn.snapErr != nil {
		return &failure{code: CodeBadSnapshot, msg: "the deploy's settings could not be read: " + agent.CleanText(rn.snapErr.Error(), 200)}
	}
	if f := rn.begin(ctx); f != nil {
		return f
	}
	// The watchdog (jobs.timeout_s) is enforced only before the switch: all of
	// 8a is before it. If it fires the deploy ends like a supersede: candidate
	// removed, old version kept.
	watchdog := time.Duration(rn.job.TimeoutS) * time.Second
	if rn.r.cfg.Watchdog > 0 {
		watchdog = rn.r.cfg.Watchdog // tests
	}
	pctx, cancelW := context.WithTimeoutCause(ctx, watchdog, errWatchdog)
	defer cancelW()
	// Pull, check and start share one 10-minute bound.
	sctx, cancelS := context.WithTimeoutCause(pctx, rn.r.cfg.PreSwitchBound, errPreSwitch)
	defer cancelS()

	pausePoint("pull:before")
	if f := rn.pull(sctx); f != nil {
		return f
	}
	pausePoint("pull:after")
	pausePoint("check:before")
	if f := rn.check(sctx); f != nil {
		return f
	}
	pausePoint("check:after")
	pausePoint("start:before")
	if f := rn.start(sctx); f != nil {
		return f
	}
	cancelS()
	pausePoint("start:after")
	pausePoint("health:before")
	if f := rn.health(pctx); f != nil {
		return f
	}
	pausePoint("health:after")
	rn.trace("health:done")
	if pctx.Err() != nil { // the watchdog fired between the gate and the switch: it still applies before the switch
		f := rn.ctxFailure0(pctx)
		f.appSafe = rn.appSafe(nil)
		return f
	}
	// Switch, commit, drain and stop_old run on a context cut off from the job's
	// cancellation and watchdog (plan 0004, "Deadlines and contexts"), each with
	// its own bound (switch.go); the job context stays in rn.jobCtx only so they
	// can read why it was cancelled (a shutdown ends the drain).
	return rn.afterHealth(context.WithoutCancel(rn.jobCtx))
}

// ---- stage bookkeeping ----

func (rn *run) now() string { return jobs.FormatTime(rn.r.cfg.Now()) }

// begin marks the deploy running and writes every stage as pending (a retry
// attempt starts the list over).
func (rn *run) begin(ctx context.Context) *failure {
	if rn.svc.status != "queued" && rn.svc.status != StatusRunning {
		return &failure{code: CodeCancelled, status: StatusCancelled, msg: "the deploy had already ended", noRecord: true}
	}
	err := rn.r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE deploys SET status = 'running', started_at = COALESCE(started_at, ?), stage = NULL, ip = NULL,
			reason_code = NULL, reason = NULL WHERE id = ?`, rn.now(), rn.deploy); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM deploy_stages WHERE deploy_id = ?`, rn.deploy); err != nil {
			return err
		}
		for _, s := range blueGreenStages {
			if _, err := tx.ExecContext(ctx, `INSERT INTO deploy_stages(deploy_id, stage, state) VALUES (?, ?, 'pending')`, rn.deploy, s); err != nil {
				return err
			}
		}
		return nil
	})
	return rn.dbFailure(ctx, err)
}

// dbFailure turns a failed database write into the deploy's failure (before the
// switch a deploy that cannot be recorded must not go on: the candidate is
// removed by finish).
func (rn *run) dbFailure(ctx context.Context, err error) *failure {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return rn.ctxFailure(ctx)
	}
	rn.failedDB = true
	rn.r.cfg.Log.Error("a deploy could not be recorded", "deploy", rn.deploy, "err", err.Error())
	return &failure{code: CodeDB, msg: "ServerDock's database could not be written, so the deploy stopped before changing anything"}
}

func (rn *run) enter(ctx context.Context, stage string) *failure {
	rn.stage = stage
	err := rn.r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE deploys SET stage = ? WHERE id = ?`, stage, rn.deploy); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'running', started_at = ? WHERE deploy_id = ? AND stage = ?`, rn.now(), rn.deploy, stage)
		return err
	})
	return rn.dbFailure(ctx, err)
}

func (rn *run) leave(ctx context.Context, stage, detail string) *failure {
	err := rn.r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'ok', finished_at = ?, detail = ? WHERE deploy_id = ? AND stage = ?`,
			rn.now(), agent.CleanText(detail, agent.MaxDetailText), rn.deploy, stage)
		return err
	})
	return rn.dbFailure(ctx, err)
}

// ctxFailure says why a cancelled context ended the deploy.
func (rn *run) ctxFailure(ctx context.Context) *failure {
	f := rn.ctxFailure0(ctx)
	if rn.stage == StageHealth {
		return rn.unhealthyBy(f) // a supersede or cancel during the health gate is an unhealthy result
	}
	return f
}

func (rn *run) ctxFailure0(ctx context.Context) *failure {
	cause := context.Cause(ctx)
	var cc jobs.CancelCause
	switch {
	case errors.As(cause, &cc) && cc.Reason == "superseded":
		return &failure{code: CodeSuperseded, status: StatusSuperseded, msg: "superseded by a newer deploy", by: rn.newerDeploy()}
	case errors.As(cause, &cc):
		return &failure{code: CodeCancelled, status: StatusCancelled, msg: agent.CleanText(cc.Reason, 200)}
	case errors.Is(cause, errShutdownCtx):
		return &failure{code: CodeShutdown, msg: "ServerDock was stopped while this deploy was running"}
	case errors.Is(cause, errWatchdog):
		return &failure{code: CodeTimeout, msg: "the deploy ran out of time before switching traffic"}
	case errors.Is(cause, errPreSwitch):
		return &failure{code: CodeTimeout, msg: "pulling, checking and starting the new version took too long"}
	}
	return &failure{code: CodeCancelled, status: StatusCancelled, msg: "the deploy was cancelled"}
}

// newerDeploy is the deploy that superseded this one: the service's newest.
func (rn *run) newerDeploy() int64 {
	var id sql.NullInt64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = rn.r.cfg.DB.R.QueryRowContext(ctx, `SELECT MAX(id) FROM deploys WHERE service_id = ? AND id > ?`, rn.svc.id, rn.deploy).Scan(&id)
	return id.Int64
}

// ---- operations ----

// op records the outcome of one agent operation for the service's state: a docker
// or timeout failure makes it unknown (until an operation succeeds); success clears it.
func (rn *run) op(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return // the stage's own context ended (a cancel, a deadline, a shutdown): the op says nothing about Docker
	}
	st := rn.r.cfg.Tracker.State()
	switch {
	case err == nil:
		st.ClearUnknown(rn.svc.id)
	case isDockerTrouble(err):
		st.MarkUnknown(rn.svc.id)
	}
}

// ErrAgentDown is what a caller returns when the local agent is not connected.
var ErrAgentDown = errors.New("the local agent is not connected")

// isDockerTrouble: the op ended in a docker or timeout error from the agent, or
// could not reach the agent at all (the op's own timeout, the local agent down).
// A refusal, a cancelled or expired job context and database errors are NOT
// trouble: they say nothing about Docker, and a state marked unknown stays so until
// an op succeeds.
func isDockerTrouble(err error) bool {
	var oe *agent.OpError
	switch {
	case errors.As(err, &oe):
		return oe.Code == agent.CodeDocker || oe.Code == agent.CodeTimeout
	case errors.Is(err, hub.ErrOpTimeout), errors.Is(err, ErrAgentDown):
		return true
	}
	return false
}

var transientRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b(429|5\d\d)\b|too ?many ?requests|time(d)? ?out|connection refused|unavailable|temporar|reset by peer|\beof\b|the docker request failed`)
})

// transientPull: a pull failure worth another attempt (plan 0004: registry 429 or
// 5xx, timeout, connection refused, daemon unavailable). A stall, a full disk, a
// refusal and "manifest unknown" are permanent.
func transientPull(err error) bool {
	var oe *agent.OpError
	if !errors.As(err, &oe) {
		return true // the agent or Docker could not be reached at all
	}
	switch oe.Code {
	case agent.CodeTimeout:
		return true
	case agent.CodeDocker:
		return transientRe().MatchString(oe.Msg)
	}
	return false
}

func opText(err error, fallback string) string {
	var oe *agent.OpError
	if errors.As(err, &oe) && oe.Msg != "" {
		return agent.CleanText(oe.Msg, 240)
	}
	return fallback
}

func (rn *run) canRetry() bool { return rn.attempt < rn.job.MaxAttempts }

// ---- stage 1: pull ----

func (rn *run) pull(ctx context.Context) *failure {
	if f := rn.enter(ctx, StagePull); f != nil {
		return f
	}
	// The install's registry rules, now: the flag or the host's name may have
	// changed since the service was saved (plan 0004 Revision 8). The agent
	// applies them again to the pull itself.
	pullRef, err := policy.ValidateImageRef(rn.snap.ImageRef, rn.r.cfg.Policy)
	if err != nil {
		return &failure{code: CodePolicy, msg: "the image reference is not allowed: " + agent.CleanText(err.Error(), 200)}
	}
	if rn.snap.ImageID != "" { // redeploy or rollback: the pinned image, if it is still here
		info, err := rn.r.cfg.Ops.ImageInspect(ctx, rn.snap.ImageID, true)
		rn.op(ctx, err)
		if err == nil {
			rn.imageID, rn.digest = info.ID, rn.snap.RepoDigest
			return rn.recordImage(ctx, "image present, not pulled")
		}
		if ctx.Err() != nil {
			return rn.ctxFailure(ctx)
		}
		var oe *agent.OpError
		if !errors.As(err, &oe) || oe.Code != agent.CodeNotFound {
			return &failure{code: CodeDocker, msg: "Docker could not look for the image: " + opText(err, "no answer"), retry: rn.canRetry()}
		}
		if rn.snap.RepoDigest == "" {
			return &failure{code: "image_gone", msg: "the image of that deploy is no longer on this server"}
		}
		// By digest: the same registry rules as for the tag, now.
		if pullRef, err = policy.ValidateImageRef(rn.snap.RepoDigest, rn.r.cfg.Policy); err != nil {
			return &failure{code: CodePolicy, msg: "the image reference is not allowed: " + agent.CleanText(err.Error(), 200)}
		}
	}
	res, err := rn.r.cfg.Ops.ImagePull(ctx, pullRef, rn.r.cfg.StallTimeout, func(p agentops.PullProgress) { rn.r.setProgress(rn.deploy, p) })
	rn.op(ctx, err)
	if err != nil {
		if ctx.Err() != nil {
			return rn.ctxFailure(ctx)
		}
		var oe *agent.OpError
		if errors.As(err, &oe) {
			switch oe.Code {
			case agent.CodeDiskFull:
				return &failure{code: CodeDiskFull, msg: "the server's disk is full: " + opText(err, "no space left")}
			case agent.CodeStalled:
				return &failure{code: CodePullStalled, msg: "the pull stopped making progress: " + opText(err, "stalled")}
			case agent.CodePolicy, agent.CodeInvalid:
				return &failure{code: CodePolicy, msg: "the image reference is not allowed: " + opText(err, "refused")}
			}
		}
		f := &failure{code: CodePullFailed, msg: "the image could not be pulled: " + opText(err, "Docker or the registry did not answer")}
		if transientPull(err) {
			f.retry = rn.canRetry()
		}
		return f
	}
	rn.imageID, rn.digest = res.ImageID, res.RepoDigest
	p, _ := rn.r.progressOf(rn.deploy)
	detail := "pulled"
	if p.Layers > 0 {
		detail = fmt.Sprintf("pulled %d layers, %d bytes", p.Layers, p.Bytes)
	}
	return rn.recordImage(ctx, detail)
}

func (rn *run) recordImage(ctx context.Context, detail string) *failure {
	err := rn.r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE deploys SET image_id = ?, repo_digest = ? WHERE id = ?`, rn.imageID, nullStr(rn.digest), rn.deploy)
		return err
	})
	if f := rn.dbFailure(ctx, err); f != nil {
		return f
	}
	return rn.leave(ctx, StagePull, detail)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- stage 2: check ----

func (rn *run) check(ctx context.Context) *failure {
	if f := rn.enter(ctx, StageCheck); f != nil {
		return f
	}
	if _, err := policy.ValidateImageRef(rn.snap.ImageRef, rn.r.cfg.Policy); err != nil { // again, at the check stage
		return &failure{code: CodePolicy, msg: "the image reference is not allowed: " + agent.CleanText(err.Error(), 200)}
	}
	if f := rn.networkReady(ctx); f != nil {
		return f
	}
	info, err := rn.r.cfg.Ops.ImageInspect(ctx, rn.imageID, true)
	rn.op(ctx, err)
	if err != nil {
		if ctx.Err() != nil {
			return rn.ctxFailure(ctx)
		}
		return &failure{code: CodeDocker, msg: "Docker could not inspect the image: " + opText(err, "no answer"), retry: rn.canRetry()}
	}
	rep, err := policy.CheckImage(policy.ImageFacts{LabelKeys: info.ReservedLabelKeys, Volumes: info.Volumes, ExposedPorts: info.ExposedPorts})
	if err != nil {
		return &failure{code: CodePolicy, msg: agent.CleanText(err.Error(), 240)}
	}
	subnet, err := rn.r.cfg.AppNet.Subnet(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return rn.ctxFailure(ctx)
		}
		return &failure{code: CodeDocker, msg: "the app subnet is not known yet: Docker did not answer", retry: rn.canRetry()}
	}
	rn.subnet = subnet.String()

	// The address, under the allocator's lock, from a fresh container_list plus the DB.
	addr, f := rn.allocate(ctx)
	if f != nil {
		return f
	}
	rn.ip = addr.String()
	args, err := BuildCreate(CreateInput{
		ProjectName: rn.svc.project, ServiceName: rn.svc.name, ProjectID: rn.svc.projectID, ServiceID: rn.svc.id,
		DeployID: rn.deploy, Attempt: rn.attempt, ImageID: rn.imageID, IP: rn.ip, ContainerPort: rn.snap.ContainerPort,
		Network: agentops.NetworkName(rn.r.cfg.Instance), Subnet: rn.subnet,
	})
	if err != nil {
		return &failure{code: CodePolicy, msg: agent.CleanText(err.Error(), 240)}
	}
	rn.create = args
	detail := "address " + rn.ip
	if len(rep.Warnings) > 0 {
		detail += "; " + rep.Warnings[0]
	}
	return rn.leave(ctx, StageCheck, detail)
}

// networkReady refuses the deploy when the app network has a problem. Whenever the
// state is not ok (unknown, or a problem the user may have fixed since: a foreign
// network removed, a VPN overlap gone) it ensures the network again, once per
// deploy: one network_ensure, no timer, so a fixed problem never needs a restart.
func (rn *run) networkReady(ctx context.Context) *failure {
	st := rn.r.cfg.Net.NetworkStatus()
	if st.State != NetOK {
		_ = rn.r.cfg.Net.Run(ctx)
		st = rn.r.cfg.Net.NetworkStatus()
	}
	switch st.State {
	case NetProblem:
		return &failure{code: CodeNetwork, msg: agent.CleanText(st.Reason, 400)}
	case NetUnknown:
		if ctx.Err() != nil {
			return rn.ctxFailure(ctx)
		}
		return &failure{code: CodeDocker, msg: "the app network could not be checked: Docker did not answer", retry: rn.canRetry()}
	}
	return nil
}

// allocate takes the deploy's address. A network that was pruned after ServerDock
// created it is ensured again once.
func (rn *run) allocate(ctx context.Context) (netip.Addr, *failure) {
	for try := 0; ; try++ {
		addr, err := rn.r.cfg.Alloc.Allocate(ctx, rn.deploy)
		if err == nil {
			rn.op(ctx, nil)
			return addr, nil
		}
		if ctx.Err() != nil {
			return netip.Addr{}, rn.ctxFailure(ctx)
		}
		if agentops.IsNetworkMissing(err) && try == 0 {
			rn.r.cfg.Net.NetworkGone()
			_ = rn.r.cfg.Net.Run(ctx)
			continue
		}
		var de *Error
		if errors.As(err, &de) {
			switch de.Code {
			case CodeIPPoolExhausted:
				return netip.Addr{}, &failure{code: CodeIPPoolExhausted, msg: de.Msg}
			case CodeAppSubnet:
				return netip.Addr{}, &failure{code: CodeNetwork, msg: agent.CleanText(de.Msg, 400)}
			case CodeDocker:
				rn.op(ctx, de.Err)
				return netip.Addr{}, &failure{code: CodeDocker, msg: de.Msg, retry: rn.canRetry()}
			}
		}
		rn.op(ctx, err)
		if isDockerTrouble(err) {
			return netip.Addr{}, &failure{code: CodeDocker, msg: "Docker could not allocate an address: " + opText(err, "no answer"), retry: rn.canRetry()}
		}
		return netip.Addr{}, &failure{code: CodeDB, msg: "an address could not be recorded"}
	}
}

// ---- stage 3: start ----

func (rn *run) start(ctx context.Context) *failure {
	if f := rn.enter(ctx, StageStart); f != nil {
		return f
	}
	ops := rn.r.cfg.Ops
	var res agentops.CreateResult
	var err error
	for try := 0; ; try++ {
		rn.createSent = true
		res, err = ops.ContainerCreate(ctx, rn.create.Name, rn.create.Spec, rn.create.IP, rn.create.ImageID)
		rn.op(ctx, err)
		if err == nil {
			break
		}
		if agentops.IsNetworkMissing(err) && try == 0 && ctx.Err() == nil {
			rn.r.cfg.Net.NetworkGone()
			if rn.r.cfg.Net.Run(ctx); rn.r.cfg.Net.NetworkStatus().State == NetOK {
				continue
			}
		}
		return rn.createFailure(ctx, err)
	}
	rn.cid = res.ID
	if err := ops.ContainerStart(ctx, rn.cid); err != nil {
		rn.op(ctx, err)
		if ctx.Err() != nil {
			return rn.ctxFailure(ctx)
		}
		return &failure{code: CodeStartFailed, msg: "the new version could not be started: " + opText(err, "Docker did not answer")}
	}
	rn.op(ctx, nil)
	if f := rn.confirm(ctx); f != nil {
		return f
	}
	detail := "started at " + rn.ip
	if res.Existed {
		detail = "reused this attempt's container, started at " + rn.ip
	}
	return rn.leave(ctx, StageStart, detail)
}

func (rn *run) createFailure(ctx context.Context, err error) *failure {
	if ctx.Err() != nil {
		return rn.ctxFailure(ctx)
	}
	var oe *agent.OpError
	if errors.As(err, &oe) {
		switch oe.Code {
		case agent.CodeConflict:
			return &failure{code: CodeNameConflict, msg: opText(err, "the container name is taken")}
		case agent.CodePolicy, agent.CodeInvalid:
			return &failure{code: CodePolicy, msg: "the create request was refused: " + opText(err, "refused")}
		case agent.CodeDiskFull:
			return &failure{code: CodeDiskFull, msg: "the server's disk is full"}
		case agent.CodeNotFound:
			if agentops.IsNetworkMissing(err) {
				return &failure{code: CodeNetwork, msg: "the app network does not exist and could not be created again"}
			}
		}
	}
	// Outcome unknown (a timeout, Docker erroring): finish looks the container up
	// by its labels, not by an id that never arrived.
	return &failure{code: CodeStartFailed, msg: "the new version could not be created: " + opText(err, "Docker did not answer")}
}

// confirm is the runner's own inspect of its candidate: it trusts the answer only
// when the agent says the container is ours AND its labels are this service, deploy
// and attempt; then it checks the address and records the container in the state
// map itself (it does not wait for the watcher).
func (rn *run) confirm(ctx context.Context) *failure {
	st, err := rn.r.cfg.Ops.ContainerInspect(ctx, rn.cid)
	rn.op(ctx, err)
	if err != nil {
		if ctx.Err() != nil {
			return rn.ctxFailure(ctx)
		}
		return &failure{code: CodeStartFailed, msg: "the new version could not be inspected: " + opText(err, "Docker did not answer")}
	}
	if !rn.ours(st) {
		rn.cid = "" // not trusted: the clean-up goes by labels, never by this id
		return &failure{code: CodeStartFailed, msg: "the container that was started is not the one this deploy created"}
	}
	if st.ConfiguredIP != rn.ip || (st.IP != "" && st.IP != rn.ip) {
		return &failure{code: CodeStartFailed, msg: fmt.Sprintf("the new version is at %s, not at its allocated address %s", shownAddr(st.ConfiguredIP, st.IP), rn.ip)}
	}
	c, ok := ContainerFromInspect(st)
	if !ok || c.Service != rn.svc.id || c.Deploy != rn.deploy {
		rn.cid = ""
		return &failure{code: CodeStartFailed, msg: "the container that was started is not the one this deploy created"}
	}
	rn.r.cfg.Tracker.Track(c)
	rn.started = true
	return nil
}

func shownAddr(cfg, live string) string {
	if cfg != "" {
		return cfg
	}
	if live != "" {
		return live
	}
	return "no address"
}

// ours is the trust rule for an inspect result: owned AND the labels name this
// service, deploy and attempt.
func (rn *run) ours(st agentops.ContainerState) bool {
	return st.Owned && st.ID == rn.cid &&
		st.Labels[agentops.LabelService] == strconv.FormatInt(rn.svc.id, 10) &&
		st.Labels[agentops.LabelDeploy] == strconv.FormatInt(rn.deploy, 10) &&
		st.Labels[agentops.LabelAttempt] == strconv.Itoa(rn.attempt)
}

// ---- stage 4: health ----

func (rn *run) health(ctx context.Context) *failure {
	if f := rn.enter(ctx, StageHealth); f != nil {
		return f
	}
	h := rn.snap.Health
	hctx, cancel := context.WithTimeout(ctx, time.Duration(h.TimeoutS)*time.Second)
	defer cancel()
	target := net.JoinHostPort(rn.ip, strconv.Itoa(rn.snap.ContainerPort))
	passes, lastErr := 0, ""
	for {
		f, inspected := rn.unhealthy(hctx, ctx, &lastErr)
		if f != nil {
			return f
		}
		ok := false
		if inspected {
			var f *failure
			if ok, f = rn.probe(hctx, ctx, target, &lastErr); f != nil {
				return f
			}
		}
		if ok {
			passes++
			if passes >= h.Passes {
				return rn.leave(ctx, StageHealth, fmt.Sprintf("healthy: %d consecutive checks passed", passes))
			}
		} else {
			passes = 0
		}
		t := time.NewTimer(rn.r.cfg.HealthInterval)
		select {
		case <-hctx.Done():
			t.Stop()
			if ctx.Err() != nil {
				return rn.unhealthyBy(rn.ctxFailure(ctx))
			}
			msg := fmt.Sprintf("the new version was not healthy within %d seconds", h.TimeoutS)
			if lastErr != "" {
				msg += " (" + lastErr + ")"
			}
			return &failure{code: CodeHealthTimeout, msg: agent.CleanText(msg, 400)}
		case <-t.C:
		}
	}
}

// unhealthyBy: a supersede or cancel during the health gate counts as an
// unhealthy result of this candidate (plan 0004, Blue-green table).
func (rn *run) unhealthyBy(f *failure) *failure {
	if f.status != "" && !strings.HasPrefix(f.msg, "unhealthy: ") {
		f.msg = "unhealthy: " + f.msg
	}
	return f
}

// unhealthy is the fail-fast check of the candidate: an inspect showing it exited,
// is restarting or was killed for memory fails the gate at once.
func (rn *run) unhealthy(hctx, ctx context.Context, lastErr *string) (f *failure, inspected bool) {
	if ctx.Err() != nil {
		return rn.unhealthyBy(rn.ctxFailure(ctx)), false
	}
	st, err := rn.r.cfg.Ops.ContainerInspect(hctx, rn.cid)
	rn.op(hctx, err)
	if err != nil {
		if ctx.Err() != nil {
			return rn.unhealthyBy(rn.ctxFailure(ctx)), false
		}
		if hctx.Err() != nil {
			return nil, false // the gate's own deadline: it says nothing about the app or Docker; the loop reports the timeout
		}
		if agentops.IsCode(err, agent.CodeNotFound) {
			// The candidate is gone (removed by hand): waiting out the timeout helps nobody.
			rn.cid = ""
			return &failure{code: CodeStartFailed, msg: "the new version's container disappeared while it was being checked (it was removed)"}, true
		}
		// One failed inspect is one failed check, not the end of the gate: Docker
		// may answer the next one. Only the deadline ends it.
		if isDockerTrouble(err) {
			*lastErr = "Docker did not answer the inspect"
		} else {
			*lastErr = opText(err, "the container could not be inspected")
		}
		return nil, false
	}
	if !rn.ours(st) {
		rn.cid = ""
		return &failure{code: CodeStartFailed, msg: "the container is not the one this deploy created"}, true
	}
	switch {
	case st.OOMKilled:
		return &failure{code: CodeOOM, msg: "the new version was killed for using too much memory"}, true
	case st.Restarting:
		return &failure{code: CodeExited, msg: fmt.Sprintf("the new version keeps crashing (it restarted after exit code %d)", st.ExitCode)}, true
	case !st.Running:
		return &failure{code: CodeExited, msg: fmt.Sprintf("the new version exited with code %d", st.ExitCode)}, true
	}
	return nil, true
}

func (rn *run) probe(hctx, ctx context.Context, target string, lastErr *string) (bool, *failure) {
	h := rn.snap.Health
	if h.Kind == "running" {
		return true, nil // unhealthy() just saw it running
	}
	kind := agentops.KindTCP
	if h.Kind == "http" {
		kind = agentops.KindHTTP
	}
	res, err := rn.r.cfg.Ops.HealthCheck(hctx, agentops.HealthArgs{Target: target, Kind: kind, Path: h.Path, TimeoutMS: agentops.MaxHealthMS})
	rn.op(ctx, err)
	if err != nil {
		if ctx.Err() != nil {
			return false, rn.unhealthyBy(rn.ctxFailure(ctx))
		}
		if hctx.Err() == nil {
			var oe *agent.OpError
			if errors.As(err, &oe) && (oe.Code == agent.CodePolicy || oe.Code == agent.CodeRefusedUnowned) {
				return false, &failure{code: CodeNetwork, msg: "ServerDock cannot reach the app network to check the new version: " + opText(err, "refused")}
			}
			*lastErr = opText(err, "the check could not run")
		}
		return false, nil
	}
	if !res.OK {
		if res.Status != 0 {
			*lastErr = fmt.Sprintf("the app answered HTTP %d", res.Status)
		} else {
			*lastErr = res.Err
		}
	}
	return res.OK, nil
}

// ---- the end of a deploy ----

// finish ends the attempt: it removes the candidate (on a FRESH context, so a
// cancelled job context cannot stop the clean-up), records the outcome and returns
// the job's error. A transient failure with attempts left returns a Retryable error
// and leaves the deploy running ("waiting to retry"); anything else is final. A
// job whose context was cancelled (a cancel, a supersede, a session revoke, a
// shutdown) never retries: the scheduler would refuse the requeue and end the job
// cancelled, and the row must not be left "waiting".
func (rn *run) finish(f *failure) error {
	if rn.committed {
		// Past the commit the candidate is the live version: it is never removed, and
		// nothing that happened after it can fail the deploy (switch.go).
		return rn.finishCommitted(f)
	}
	if f == nil {
		f = &failure{code: CodeStartFailed, msg: "the deploy ended without a result"}
	}
	bound := rn.cleanupBound() // 30 s, or 5 s when the process is going away
	if !f.noRecord && rn.switched {
		// The routes may point at the candidate (a panic, or any early return, after
		// the switch): put them back BEFORE it is removed, so no route names a
		// container that is gone and whose address may be handed out again.
		f = rn.unwindSwitch(f)
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(rn.jobCtx), bound)
	defer cancel()
	if !f.keep {
		rn.removeCandidate(fctx)
	}
	if f.noRecord {
		return errors.New(f.msg)
	}
	// The end of the row gets its own fresh, short budget: a removal that used up
	// its own must not stop the deploy from being recorded as ended.
	rbound := min(bound, 10*time.Second)
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(rn.jobCtx), rbound)
	defer rcancel()

	if f.retry && rn.jobCtx.Err() != nil {
		// The transient failure lost the race against a cancel, a supersede, a revoke
		// or a shutdown: the deploy reads that, not the pull error.
		f = rn.ctxFailure(rn.jobCtx)
	}
	if f.retry && rn.canRetry() {
		rn.recordRetry(rctx, f)
		return jobs.Retryable(errors.New(f.msg))
	}
	rn.recordFinal(rctx, f)
	return errors.New(f.msg)
}

// removeCandidate stops and removes this attempt's container: the one it knows by
// id, and, when a create was sent whose answer never arrived (a timeout) or the
// inspect of the container was not trusted, every container of this install
// carrying this service, deploy and attempt in its labels. A candidate the
// switch never reached never served traffic and is stopped with no grace; one the
// routes pointed at (rn.switched) may have, and gets stop_grace_s. It is forgotten (Untrack) only
// after its removal succeeded.
func (rn *run) removeCandidate(ctx context.Context) {
	ops := rn.r.cfg.Ops
	ids := map[string]bool{}
	if rn.cid != "" {
		ids[rn.cid] = true
	}
	if rn.createSent && rn.cid == "" {
		if list, err := ops.ContainerList(ctx); err == nil {
			rn.op(ctx, nil)
			for _, c := range list.Containers {
				if !c.Unowned && c.Labels.Service == strconv.FormatInt(rn.svc.id, 10) && c.Labels.Deploy == strconv.FormatInt(rn.deploy, 10) &&
					c.Labels.Attempt == strconv.Itoa(rn.attempt) {
					ids[c.ID] = true
				}
			}
		} else {
			rn.op(ctx, err)
			rn.r.cfg.Log.Warn("could not look for a candidate by its labels; the next resync removes it", "deploy", rn.deploy)
		}
	}
	for id := range ids {
		stopErr := rn.stopCandidate(ctx, id)
		gone := agentops.IsCode(stopErr, agent.CodeNotFound)
		if stopErr != nil && !gone {
			rn.op(ctx, stopErr)
			rn.r.cfg.Log.Warn("the candidate could not be stopped; the next resync removes it", "deploy", rn.deploy)
		}
		if gone {
			rn.r.cfg.Tracker.Untrack(id)
			continue
		}
		// The removal has its own bound: a switched candidate stopped with a long
		// grace must not be left behind because the clean-up context ran out.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), RemoveBound)
		if rn.shuttingDown() {
			rcancel()
			rctx, rcancel = context.WithTimeout(context.WithoutCancel(ctx), rn.r.cfg.ShutdownCleanupBound)
		}
		rmErr := ops.ContainerRemove(rctx, id)
		rcancel()
		switch {
		case rmErr == nil || agentops.IsCode(rmErr, agent.CodeNotFound):
			if stopErr == nil {
				rn.op(ctx, nil) // both succeeded: Docker answers
			}
			rn.r.cfg.Tracker.Untrack(id)
		default:
			rn.op(ctx, rmErr)
			rn.r.cfg.Log.Warn("the candidate could not be removed; the next resync removes it", "deploy", rn.deploy)
		}
	}
}

func (rn *run) recordRetry(ctx context.Context, f *failure) {
	detail := fmt.Sprintf("waiting to retry (attempt %d of %d): %s", rn.attempt+1, rn.job.MaxAttempts, f.msg)
	err := rn.r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'pending', detail = ? WHERE deploy_id = ? AND stage = ?`,
			agent.CleanText(detail, agent.MaxDetailText), rn.deploy, orDefault(rn.stage, StagePull))
		return err
	})
	if err != nil {
		rn.r.cfg.Log.Error("a retry could not be recorded", "deploy", rn.deploy, "err", err.Error())
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (rn *run) recordFinal(ctx context.Context, f *failure) {
	status := rn.statusOf(f)
	safe := rn.appSafe(f)
	err := rn.r.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var by any
		if f.by > 0 {
			by = f.by
		}
		// Only a row that is still live: an end state the queue wrote (a cancel, a
		// supersede of a queued deploy) is not rewritten.
		res, err := tx.ExecContext(ctx, `UPDATE deploys SET status = ?, reason_code = ?, reason = ?, app_safe = ?, superseded_by = COALESCE(?, superseded_by),
			finished_at = ? WHERE id = ? AND status IN ('queued', 'running')`, status, f.code, agent.CleanText(f.msg, agent.MaxReasonText), safe, by, rn.now(), rn.deploy)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if rn.stage != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'failed', finished_at = ?, detail = ? WHERE deploy_id = ? AND stage = ? AND state IN ('running', 'pending')`,
				rn.now(), agent.CleanText(f.msg, agent.MaxDetailText), rn.deploy, rn.stage); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'skipped' WHERE deploy_id = ? AND state = 'pending'`, rn.deploy)
		return err
	})
	if err != nil {
		rn.r.cfg.Log.Error("the end of a deploy could not be recorded (the boot pass settles it)", "deploy", rn.deploy, "err", err.Error())
		return
	}
	if _, err := PruneDeploys(ctx, rn.r.cfg.DB, rn.svc.id); err != nil {
		rn.r.cfg.Log.Warn("old deploys could not be pruned", "err", err.Error())
	}
}
