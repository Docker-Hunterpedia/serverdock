package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// Store reads and writes the jobs table (and, for EnqueueDeploy, the deploys it
// supersedes). All writes go through the single writer in BEGIN IMMEDIATE
// transactions.
//
// The clock: now stamps every time the store writes and reads (run_after, due
// checks, NextDue). The Scheduler also has a clock (Config.Now) and computes the
// timer's delay as "a store time minus the scheduler's now" (Scheduler.arm). The
// two MUST be the same clock: in production both are time.Now (NewStore(st, nil)
// and Config.Now unset), and a test that injects a clock into one injects the same
// one into the other. If they differed, a retry would fire early or late by the
// difference.
type Store struct {
	st  *store.Store
	now func() time.Time
}

// NewStore returns a Store over st. now (optional) replaces the clock in tests.
func NewStore(st *store.Store, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{st: st, now: now}
}

// NewJob is a job to enqueue.
type NewJob struct {
	Kind        string
	LockKey     string
	Actor       Actor
	Confirm     string // the Confirm value of a destructive request
	Payload     json.RawMessage
	MaxAttempts int       // 0 means 1
	TimeoutS    int       // > 0
	RunAfter    time.Time // zero means now
}

func (n NewJob) validate() error {
	switch n.Kind {
	case KindDeploy, KindRestart, KindServiceDelete:
	default:
		return invalid("unknown job kind %q", n.Kind)
	}
	if !lockKeyRe().MatchString(n.LockKey) {
		return invalid("lock key must be service:<id>")
	}
	if err := n.Actor.validate(); err != nil {
		return err
	}
	if len(n.Payload) > MaxPayload {
		return invalid("payload is over %d bytes", MaxPayload)
	}
	if !json.Valid(n.Payload) {
		return invalid("payload is not JSON")
	}
	if len(n.Confirm) > 256 {
		return invalid("confirm is too long")
	}
	if n.MaxAttempts < 0 || n.MaxAttempts > 10 {
		return invalid("max_attempts must be 1..10")
	}
	if n.TimeoutS <= 0 || n.TimeoutS > 24*3600 {
		return invalid("timeout_s must be 1..86400")
	}
	return nil
}

const jobCols = `id, kind, lock_key, state, actor_kind, actor_id, COALESCE(actor_session, ''), actor_blob, COALESCE(confirm, ''),
	enqueued_at, payload_json, attempt, max_attempts, run_after, timeout_s, cancel_requested, COALESCE(cancel_reason, ''),
	COALESCE(boot_id, ''), COALESCE(request_id, '')`

type rowScanner interface{ Scan(dest ...any) error }

func scanJob(r rowScanner) (Job, error) {
	var j Job
	var enq, run, payload string
	var cancel int
	err := r.Scan(&j.ID, &j.Kind, &j.LockKey, &j.State, &j.Actor.Kind, &j.Actor.ID, &j.Actor.Session, &j.Actor.Blob, &j.Confirm,
		&enq, &payload, &j.Attempt, &j.MaxAttempts, &run, &j.TimeoutS, &cancel, &j.CancelReason, &j.BootID, &j.RequestID)
	if err != nil {
		return Job{}, err
	}
	j.CancelRequested = cancel != 0
	j.Payload = []byte(payload)
	if j.EnqueuedAt, err = ParseTime(enq); err != nil {
		return Job{}, fmt.Errorf("jobs: row %d enqueued_at: %w", j.ID, err)
	}
	if j.RunAfter, err = ParseTime(run); err != nil {
		return Job{}, fmt.Errorf("jobs: row %d run_after: %w", j.ID, err)
	}
	return j, nil
}

// Get returns one job.
func (s *Store) Get(ctx context.Context, id int64) (Job, error) {
	return scanJob(s.st.R.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
}

// pruneFinished deletes finished jobs older than 30 days, at most PruneBatch per
// call (the G7 pattern: bounded work on insert, no timer). A deploy that
// references a pruned job keeps its row with job_id NULL (ON DELETE SET NULL).
func pruneFinished(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id IN (
		SELECT id FROM jobs WHERE state IN ('succeeded', 'failed', 'cancelled') AND finished_at IS NOT NULL AND finished_at < ?
		ORDER BY finished_at, id LIMIT ?)`, FormatTime(now.Add(-FinishedJobKeep)), PruneBatch)
	return err
}

func insertJob(ctx context.Context, tx *sql.Tx, n NewJob, now time.Time) (int64, error) {
	attempts := n.MaxAttempts
	if attempts == 0 {
		attempts = 1
	}
	runAfter := n.RunAfter
	if runAfter.IsZero() {
		runAfter = now
	}
	var session, confirm any
	if n.Actor.Session != "" {
		// Closes the revoke/enqueue race: a session revoked between the caller's
		// authorisation and this write has no row, and its job is refused here, in
		// the same transaction that would store it. (auth.sessions_revoke deletes
		// the row and cancels the session's jobs in one transaction.)
		var live bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE public_id = ?)`, n.Actor.Session).Scan(&live); err != nil {
			return 0, err
		}
		if !live {
			return 0, ErrActorSessionGone
		}
		session = n.Actor.Session
	}
	if n.Confirm != "" {
		confirm = n.Confirm
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO jobs(kind, lock_key, state, actor_kind, actor_id, actor_session, actor_blob, confirm,
		enqueued_at, payload_json, attempt, max_attempts, run_after, timeout_s, created_at)
		VALUES (?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		n.Kind, n.LockKey, n.Actor.Kind, n.Actor.ID, session, n.Actor.Blob, confirm,
		FormatTime(now), string(n.Payload), attempts, FormatTime(runAfter), n.TimeoutS, FormatTime(now))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ReasonDeleted is the cancel reason of the jobs a queued service delete cancels or
// flags (the deploy that reads it ends its drain at once).
const ReasonDeleted = "the service is being deleted"

// ErrServiceDeleting is returned when work is queued for a service that has a
// delete queued or running.
var ErrServiceDeleting = errors.New("jobs: the service is being deleted")

func deletePending(ctx context.Context, tx *sql.Tx, lock string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE lock_key = ? AND kind = 'service_delete' AND state IN ('queued', 'running')`, lock).Scan(&n)
	return n > 0, err
}

// Enqueue adds a job (restart, delete). It also prunes old finished jobs. Work for
// a service whose delete is pending is refused (ErrServiceDeleting).
func (s *Store) Enqueue(ctx context.Context, n NewJob) (int64, error) {
	if err := n.validate(); err != nil {
		return 0, err
	}
	var id int64
	now := s.now()
	err := s.st.WriteTx(ctx, func(tx *sql.Tx) error {
		if n.Kind != KindServiceDelete {
			if pending, err := deletePending(ctx, tx, n.LockKey); err != nil {
				return err
			} else if pending {
				return ErrServiceDeleting
			}
		}
		if err := pruneFinished(ctx, tx, now); err != nil {
			return err
		}
		var err error
		id, err = insertJob(ctx, tx, n, now)
		return err
	})
	return id, err
}

// NewDeploy is a deploy to queue: its row (no address yet) and its job.
type NewDeploy struct {
	ServiceID      int64
	Kind           string // deploy | redeploy | rollback
	SourceDeployID int64  // redeploy and rollback: the deploy whose snapshot was copied; 0 otherwise
	Snapshot       json.RawMessage
	Settings       Settings // timeout_s is derived from these
	Actor          Actor
	Confirm        string
	// InTx, if set, runs inside the enqueue's write transaction before anything
	// is inserted; an error from it rolls everything back. services.deploy uses it
	// to record the new image reference in the same transaction as the deploy.
	InTx func(ctx context.Context, tx *sql.Tx) error
}

// Enqueued is the result of EnqueueDeploy.
type Enqueued struct {
	DeployID int64
	Number   int
	JobID    int64
	// Superseded are the queued deploy jobs this one cancelled (their deploys are
	// now 'superseded').
	Superseded []int64
	// CancelRunning are running deploy jobs of the same service flagged
	// cancel_requested; the caller closes their in-memory cancel (Scheduler.Cancel).
	CancelRunning []int64
}

// EnqueueDeploy queues a deploy in one transaction: the deploy row, the job, and
// the supersede of the service's other deploys. Every queued deploy job of the
// service becomes cancelled and its deploy superseded (superseded_by set); a
// running one gets cancel_requested = 1 with reason "superseded".
func (s *Store) EnqueueDeploy(ctx context.Context, n NewDeploy) (Enqueued, error) {
	switch n.Kind {
	case "deploy", "redeploy", "rollback":
	default:
		return Enqueued{}, invalid("unknown deploy kind %q", n.Kind)
	}
	if n.ServiceID <= 0 {
		return Enqueued{}, invalid("service id")
	}
	if len(n.Snapshot) == 0 || len(n.Snapshot) > MaxPayload || !json.Valid(n.Snapshot) {
		return Enqueued{}, invalid("snapshot must be JSON of at most %d bytes", MaxPayload)
	}
	if n.SourceDeployID < 0 || (n.Kind == "deploy") != (n.SourceDeployID == 0) {
		return Enqueued{}, invalid("a source deploy is for redeploy and rollback only")
	}
	timeoutS, err := TimeoutSeconds(n.Settings)
	if err != nil {
		return Enqueued{}, err
	}
	if err := n.Actor.validate(); err != nil {
		return Enqueued{}, err
	}
	lock := LockKey(n.ServiceID)
	now := s.now()
	var out Enqueued
	err = s.st.WriteTx(ctx, func(tx *sql.Tx) error {
		out = Enqueued{}
		if pending, err := deletePending(ctx, tx, lock); err != nil {
			return err
		} else if pending {
			return ErrServiceDeleting
		}
		if n.InTx != nil {
			if err := n.InTx(ctx, tx); err != nil {
				return err
			}
		}
		if err := pruneFinished(ctx, tx, now); err != nil {
			return err
		}
		var source any
		if n.SourceDeployID != 0 {
			source = n.SourceDeployID
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO deploys(service_id, number, kind, source_deploy_id, snapshot_json, status, actor_kind, actor_id, created_at)
			VALUES (?, (SELECT COALESCE(MAX(number), 0) + 1 FROM deploys WHERE service_id = ?), ?, ?, ?, 'queued', ?, ?, ?)`,
			n.ServiceID, n.ServiceID, n.Kind, source, string(n.Snapshot), n.Actor.Kind, n.Actor.ID, FormatTime(now))
		if err != nil {
			return err
		}
		if out.DeployID, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT number FROM deploys WHERE id = ?`, out.DeployID).Scan(&out.Number); err != nil {
			return err
		}
		payload, _ := json.Marshal(struct {
			Deploy int64 `json:"deploy"`
		}{out.DeployID})
		out.JobID, err = insertJob(ctx, tx, NewJob{
			Kind: KindDeploy, LockKey: lock, Actor: n.Actor, Confirm: n.Confirm, Payload: payload,
			MaxAttempts: DeployAttempts, TimeoutS: timeoutS,
		}, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE deploys SET job_id = ? WHERE id = ?`, out.JobID, out.DeployID); err != nil {
			return err
		}
		return supersede(ctx, tx, lock, out.DeployID, out.JobID, now, &out)
	})
	if err != nil {
		return Enqueued{}, err
	}
	return out, nil
}

// NewDelete is a service delete to queue.
type NewDelete struct {
	ServiceID  int64
	Target     string // "<project>/<service>": the Confirm value and the audit target
	Actor      Actor
	StopGraceS int // derives timeout_s
}

// Deleted is the result of EnqueueDelete.
type Deleted struct {
	JobID int64
	// Cancelled are the queued deploy and restart jobs of the service that were ended
	// (their deploys are cancelled); CancelRunning are the running ones, flagged with
	// ReasonDeleted (the caller closes their in-memory cancel).
	Cancelled     []int64
	CancelRunning []int64
}

// EnqueueDelete queues a service delete in one transaction: every queued deploy or
// restart job of the service is cancelled, a running one is flagged
// (cancel_requested, reason ReasonDeleted), and the delete job is inserted. The
// delete job's Confirm is the target: the policy checked it before this ran.
func (s *Store) EnqueueDelete(ctx context.Context, n NewDelete) (Deleted, error) {
	if n.ServiceID <= 0 || n.Target == "" || len(n.Target) > 256 {
		return Deleted{}, invalid("a delete needs a service and its target")
	}
	if n.StopGraceS < 0 || n.StopGraceS > 300 {
		return Deleted{}, invalid("stop_grace_s must be 0..300")
	}
	if err := n.Actor.validate(); err != nil {
		return Deleted{}, err
	}
	lock := LockKey(n.ServiceID)
	now := s.now()
	var out Deleted
	err := s.st.WriteTx(ctx, func(tx *sql.Tx) error {
		out = Deleted{}
		if pending, err := deletePending(ctx, tx, lock); err != nil {
			return err
		} else if pending {
			return ErrServiceDeleting
		}
		if err := pruneFinished(ctx, tx, now); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, state FROM jobs WHERE lock_key = ? AND state IN ('queued', 'running') ORDER BY id`, lock)
		if err != nil {
			return err
		}
		type jr struct {
			id    int64
			state string
		}
		var list []jr
		for rows.Next() {
			var j jr
			if err := rows.Scan(&j.id, &j.state); err != nil {
				rows.Close()
				return err
			}
			list = append(list, j)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, j := range list {
			if j.state == StateQueued {
				if _, err := s.cancelTx(ctx, tx, j.id, ReasonDeleted); err != nil {
					return err
				}
				out.Cancelled = append(out.Cancelled, j.id)
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET cancel_requested = 1, cancel_reason = ? WHERE id = ? AND state = 'running'`, ReasonDeleted, j.id); err != nil {
				return err
			}
			out.CancelRunning = append(out.CancelRunning, j.id)
		}
		payload, _ := json.Marshal(struct {
			Service int64  `json:"service"`
			Target  string `json:"target"`
		}{n.ServiceID, n.Target})
		out.JobID, err = insertJob(ctx, tx, NewJob{Kind: KindServiceDelete, LockKey: lock, Actor: n.Actor, Confirm: n.Target, Payload: payload,
			MaxAttempts: 1, TimeoutS: DeleteTimeoutSeconds(n.StopGraceS)}, now)
		return err
	})
	if err != nil {
		return Deleted{}, err
	}
	return out, nil
}

func supersede(ctx context.Context, tx *sql.Tx, lock string, newDeploy, newJob int64, now time.Time, out *Enqueued) error {
	type pair struct{ job, deploy int64 }
	collect := func(state string) ([]pair, error) {
		rows, err := tx.QueryContext(ctx, `SELECT j.id, d.id FROM jobs j JOIN deploys d ON d.job_id = j.id
			WHERE j.lock_key = ? AND j.kind = 'deploy' AND j.state = ? AND j.id <> ? ORDER BY j.id`, lock, state, newJob)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ps []pair
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.job, &p.deploy); err != nil {
				return nil, err
			}
			ps = append(ps, p)
		}
		return ps, rows.Err()
	}
	ts := FormatTime(now)
	queued, err := collect(StateQueued)
	if err != nil {
		return err
	}
	// A queued job is either waiting to start or waiting to retry (its deploy is
	// then 'running'); both end here, so no deploy row is left non-final.
	for _, p := range queued {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'cancelled', cancel_requested = 1, cancel_reason = 'superseded', finished_at = ?, error = 'superseded by a newer deploy'
			WHERE id = ? AND state = 'queued'`, ts, p.job); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE deploys SET status = 'superseded', superseded_by = ?, finished_at = ?, reason_code = 'superseded',
			reason = 'superseded by a newer deploy' WHERE id = ? AND status IN ('queued', 'running')`, newDeploy, ts, p.deploy); err != nil {
			return err
		}
		out.Superseded = append(out.Superseded, p.job)
	}
	// A running job is only flagged. Its deploy may still finish (even succeed),
	// so superseded_by is not set here: the runner records the outcome.
	running, err := collect(StateRunning)
	if err != nil {
		return err
	}
	for _, p := range running {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET cancel_requested = 1, cancel_reason = 'superseded' WHERE id = ? AND state = 'running'`, p.job); err != nil {
			return err
		}
		out.CancelRunning = append(out.CancelRunning, p.job)
	}
	return nil
}

// Pick moves the next due job to running, or returns (zero, false). A job is due
// when its run_after has passed and no job with the same lock key is running; the
// partial unique index jobs_one_running_per_lock makes a second running job per
// key impossible even if two pickers race.
func (s *Store) Pick(ctx context.Context, bootID string) (Job, bool, error) {
	now := s.now()
	var j Job
	var found bool
	err := s.st.WriteTx(ctx, func(tx *sql.Tx) error {
		found = false
		row := tx.QueryRowContext(ctx, `UPDATE jobs SET state = 'running', boot_id = ?, started_at = ?, request_id = 'job-' || id || '-a' || attempt
			WHERE id = (SELECT id FROM jobs WHERE state = 'queued' AND run_after <= ?
				AND lock_key NOT IN (SELECT lock_key FROM jobs WHERE state = 'running')
				ORDER BY run_after, id LIMIT 1)
			RETURNING `+jobCols, bootID, FormatTime(now), FormatTime(now))
		var err error
		j, err = scanJob(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return j, found, err
}

// NextDue is the earliest run_after of a queued job later than since, which is
// when the scheduler's one timer must fire. since is the clock reading the
// scheduler took before its last Pick: a job that became due after that reading
// is returned (its time is already past, so the timer fires at once) instead of
// being lost between the pick and this query. ok is false when there is none:
// the timer is then stopped. A job that is due but blocked (its lock key is
// running) needs no timer: the running job's end wakes the scheduler.
func (s *Store) NextDue(ctx context.Context, since time.Time) (time.Time, bool, error) {
	var v sql.NullString
	if err := s.st.R.QueryRowContext(ctx, `SELECT MIN(run_after) FROM jobs WHERE state = 'queued' AND run_after > ?`, FormatTime(since)).Scan(&v); err != nil {
		return time.Time{}, false, err
	}
	if !v.Valid {
		return time.Time{}, false, nil
	}
	t, err := ParseTime(v.String)
	return t, err == nil, err
}

// Finish records the end of a running job. The error text is cut to
// MaxErrorText bytes at a rune boundary.
func (s *Store) Finish(ctx context.Context, id int64, state, errText string) error {
	return s.finish(ctx, id, state, errText, false)
}

// finish is Finish; denied says the run was refused by policy (the error wrapped
// actions.ErrDenied: the scheduler decides that with errors.Is, never by reading
// the text), so a deploy the runner left live ends with code "denied".
func (s *Store) finish(ctx context.Context, id int64, state, errText string, denied bool) error {
	switch state {
	case StateSucceeded, StateFailed, StateCancelled:
	default:
		return invalid("finish state %q", state)
	}
	var e any
	if errText != "" {
		e = clip(errText, MaxErrorText)
	}
	now := s.now()
	return s.st.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, finished_at = ?, error = ? WHERE id = ? AND state = 'running'`, state, FormatTime(now), e, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		switch {
		case n == 1 && (state == StateFailed || state == StateCancelled):
			return endDeployTx(ctx, tx, id, state, errText, denied, now)
		case n == 1 && state == StateSucceeded:
			return settleCommittedDeployTx(ctx, tx, id, now)
		}
		return nil
	})
}

// DeniedReason is the fixed reason of a deploy whose queuing actor could no longer
// run it (the job failed "denied").
const DeniedReason = "the user or token that queued this deploy can no longer run it"

// endDeployTx is the backstop of the queue: when a deploy job reaches a failed or
// cancelled end, the same transaction ends its deploy row if the runner has not.
// A deploy left queued or running after its job is over would hold its static
// address (index deploys_ip_live) and show "running" forever; this makes that
// impossible whatever went wrong in the runner (a refusal before it started, a
// panic, a failed write of its own, a retry that lost the race against a cancel).
// The runner normally ends the row itself first, with a precise reason; this only
// touches rows still queued or running (the cancelTx pattern).
func endDeployTx(ctx context.Context, tx *sql.Tx, job int64, state, errText string, denied bool, now time.Time) error {
	// A deploy that has committed is the service's live version: whatever ended its
	// job (a cancel or supersede that reached the drain, a shutdown), it is never
	// recorded failed, cancelled or superseded.
	if err := settleCommittedDeployTx(ctx, tx, job, now); err != nil {
		return err
	}
	ts := FormatTime(now)
	status, code, reason := "failed", "job_failed", clip(errText, 512)
	switch {
	case state == StateCancelled && errText == "superseded":
		status, code, reason = "superseded", "superseded", "superseded by a newer deploy"
	case state == StateCancelled:
		status, code = "cancelled", "cancelled"
		if reason == "" {
			reason = "cancelled"
		}
	case denied:
		code, reason = "denied", DeniedReason
	case reason == "":
		reason = "the deploy's job failed"
	}
	res, err := tx.ExecContext(ctx, `UPDATE deploys SET status = ?, reason_code = ?, reason = ?, finished_at = ?,
		app_safe = COALESCE(app_safe, 'unknown'), -- the queue cannot see the old version; step 10's resync raises it
		superseded_by = CASE WHEN ? = 'superseded' THEN (SELECT MAX(d2.id) FROM deploys d2 WHERE d2.service_id = deploys.service_id AND d2.id > deploys.id) ELSE superseded_by END
		WHERE job_id = ? AND status IN ('queued', 'running')`, status, code, reason, ts, status, job)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'failed', finished_at = ?, detail = 'ended with its job'
		WHERE state = 'running' AND deploy_id IN (SELECT id FROM deploys WHERE job_id = ?)`, ts, job); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'skipped' WHERE state = 'pending' AND deploy_id IN (SELECT id FROM deploys WHERE job_id = ?)`, job)
	return err
}

// RecordIncompleteReason is the reason of a deploy that committed but whose end
// the runner could not write: the queue settles it as succeeded when its job ends
// succeeded (plan 0004, failure rules: after the commit the deploy stays
// succeeded, honestly marked "record incomplete").
const RecordIncompleteReason = "record incomplete: the new version is live, but the end of this deploy could not be written"

// settleCommittedDeployTx is the succeeded-side backstop: a deploy job that ends
// succeeded while its deploy row is still running and has a commit (committed_at)
// ends the row as succeeded, in the same transaction, so the row never stays
// "running" (holding its address in deploys_ip_live) after a post-commit write
// failure in the runner. A row with no commit is left alone: it is not ours to
// call succeeded.
func settleCommittedDeployTx(ctx context.Context, tx *sql.Tx, job int64, now time.Time) error {
	ts := FormatTime(now)
	res, err := tx.ExecContext(ctx, `UPDATE deploys SET status = 'succeeded', stage = 'done', reason_code = 'warning', reason = ?, finished_at = ?
		WHERE job_id = ? AND status = 'running' AND committed_at IS NOT NULL`, RecordIncompleteReason, ts, job)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'ok', finished_at = ?, detail = 'record incomplete'
		WHERE state = 'running' AND deploy_id IN (SELECT id FROM deploys WHERE job_id = ?)`, ts, job); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'skipped' WHERE state = 'pending' AND deploy_id IN (SELECT id FROM deploys WHERE job_id = ?)`, job)
	return err
}

// Requeue puts a running job back to queued for another attempt after delay. It
// refuses (false) a job whose cancel was requested: the caller then finishes it
// as cancelled, so a cancel or supersede that arrived during the attempt is never
// undone by a retry.
func (s *Store) Requeue(ctx context.Context, id int64, delay time.Duration, errText string) (bool, error) {
	var ok bool
	err := s.st.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'queued', attempt = attempt + 1, run_after = ?, boot_id = NULL, started_at = NULL,
			request_id = NULL, error = ? WHERE id = ? AND state = 'running' AND attempt < max_attempts AND cancel_requested = 0`,
			FormatTime(s.now().Add(delay)), clip(errText, MaxErrorText), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n == 1
		return err
	})
	return ok, err
}

// cancel flags a job. A queued job becomes cancelled at once (returns
// StateCancelled); a running one gets cancel_requested = 1 (returns
// StateRunning) and the caller closes its in-memory cancel.
func (s *Store) cancel(ctx context.Context, id int64, reason string) (string, error) {
	reason = clip(reason, 128)
	var state string
	err := s.st.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		state, err = s.cancelTx(ctx, tx, id, reason)
		return err
	})
	return state, err
}

func (s *Store) cancelTx(ctx context.Context, tx *sql.Tx, id int64, reason string) (string, error) {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, id).Scan(&state); err != nil {
		return "", err
	}
	switch state {
	case StateQueued:
		ts := FormatTime(s.now())
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'cancelled', cancel_requested = 1, cancel_reason = ?, finished_at = ?, error = ? WHERE id = ?`,
			reason, ts, reason, id); err != nil {
			return "", err
		}
		// The deploy of a queued job (never started, or waiting to retry) ends
		// with it, in the same transaction.
		_, err := tx.ExecContext(ctx, `UPDATE deploys SET status = 'cancelled', reason_code = 'cancelled', reason = ?, finished_at = ?
			WHERE job_id = ? AND status IN ('queued', 'running')`, reason, ts, id)
		return StateCancelled, err
	case StateRunning:
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET cancel_requested = 1, cancel_reason = COALESCE(cancel_reason, ?) WHERE id = ?`, reason, id)
		return StateRunning, err
	}
	return "", ErrNotCancellable
}

// ErrStageNotCancellable is returned by CancelDeploy for a deploy whose runner is
// in a stage that a cancel cannot reach (switch, commit, drain, stop_old).
var ErrStageNotCancellable = errors.New("jobs: the deploy is in a stage that cannot be cancelled")

// cancelDeploy is cancel for a deploy job with a guard that is atomic with the
// flag: in the same write transaction it reads the deploy's stage, refuses when
// the stage is one of forbidden, and otherwise sets the cancel flag. The runner
// moves its deploy into a stage with a write of its own, and writes are
// serialised, so a cancel either lands before that write (the runner then sees the
// flag) or after it (the cancel sees the stage and is refused): it can never pass
// the check and then be applied inside the switch.
func (s *Store) cancelDeploy(ctx context.Context, id int64, reason string, forbidden []string) (string, error) {
	reason = clip(reason, 128)
	var state string
	err := s.st.WriteTx(ctx, func(tx *sql.Tx) error {
		var stage sql.NullString
		var status string
		err := tx.QueryRowContext(ctx, `SELECT stage, status FROM deploys WHERE job_id = ?`, id).Scan(&stage, &status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && status != "queued" && status != "running" {
			return ErrNotCancellable // the deploy has ended (its job is about to): never "cancelling"
		}
		if stage.Valid {
			for _, f := range forbidden {
				if stage.String == f {
					return ErrStageNotCancellable
				}
			}
		}
		state, err = s.cancelTx(ctx, tx, id, reason)
		return err
	})
	return state, err
}

// CancelSessionJobsTx cancels, inside the caller's write transaction, every
// queued or running job that the given sessions queued. auth.sessions_revoke
// calls it in the same transaction that deletes the sessions, so a session can
// never be gone with its jobs still live. It returns the ids of the running jobs,
// whose in-memory cancel must be closed after the transaction commits
// (Scheduler.CancelSessionJobsTx does both).
func (s *Store) CancelSessionJobsTx(ctx context.Context, tx *sql.Tx, sessions []string, reason string) (running []int64, n int, err error) {
	reason = clip(reason, 128)
	for _, sess := range sessions {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM jobs WHERE actor_session = ? AND state IN ('queued', 'running') ORDER BY id`, sess)
		if err != nil {
			return nil, 0, err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, 0, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, 0, err
		}
		rows.Close()
		for _, id := range ids {
			state, err := s.cancelTx(ctx, tx, id, reason)
			if err != nil {
				return nil, 0, err
			}
			n++
			if state == StateRunning {
				running = append(running, id)
			}
		}
	}
	return running, n, nil
}

// clip cuts s to at most n bytes without splitting a rune.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
