package deploy

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// ResolveInterrupted is the DB-only part of the boot pass (plan 0004,
// "Reconciler"): every job still "running" from an earlier boot (a crash, a kill,
// or a shutdown that left it for this pass) is settled before the scheduler
// starts, so it cannot hold its service's lock key forever. It touches no container.
//
//   - A deploy whose commit happened (services.active_deploy_id names it) is
//     succeeded ("finished after a restart"); the commit point is authoritative.
//   - Any other deploy is failed "interrupted" with app_safe unknown; the first
//     resync (resync.go) finds out whether the previous version runs, raises
//     app_safe to yes if it does, and removes the candidate.
//   - A restart is failed "interrupted"; a service delete is failed "interrupted"
//     and its service is flagged delete_interrupted (every step of a delete is
//     idempotent: the user deletes it again).
//   - The jobs.run_<kind> intent of the run (found by its request id, job-<id>-a<n>)
//     gets its single outcome row, through the later-boot outcome path: the
//     intent's own actor, executor system, result ok (a finished deploy) or error.
//     An intent that already has an outcome is never given a second one, and a
//     pass that settles nothing writes nothing.
//
// bootID is the new scheduler's boot id: only jobs with another one are settled.
// aud may be nil (no audit rows are written). A failure to write an outcome is
// logged by the caller's next pass of the audit verifier, never fails the boot: the
// intent then stays "unknown", which is what it was.
func ResolveInterrupted(ctx context.Context, st *store.Store, bootID string, now time.Time, aud audit.Appender) (int, error) {
	n := 0
	var settled []settledJob
	err := st.WriteTx(ctx, func(tx *sql.Tx) error {
		n = 0
		settled = settled[:0]
		rows, err := tx.QueryContext(ctx, `SELECT id, kind, COALESCE(request_id, 'job-' || id || '-a' || attempt) FROM jobs WHERE state = 'running' AND (boot_id IS NULL OR boot_id <> ?) ORDER BY id`, bootID)
		if err != nil {
			return err
		}
		type jr struct {
			id          int64
			kind, reqID string
		}
		var list []jr
		for rows.Next() {
			var j jr
			if err := rows.Scan(&j.id, &j.kind, &j.reqID); err != nil {
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
		ts := jobs.FormatTime(now)
		for _, j := range list {
			jobState, jobErr := jobs.StateFailed, "interrupted by restart"
			if j.kind == jobs.KindDeploy {
				var dep int64
				err := tx.QueryRowContext(ctx, `SELECT id FROM deploys WHERE job_id = ?`, j.id).Scan(&dep)
				switch {
				case err == sql.ErrNoRows:
				case err != nil:
					return err
				default:
					var committed bool
					if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM services WHERE active_deploy_id = ?)`, dep).Scan(&committed); err != nil {
						return err
					}
					if !committed {
						// A stop-first deploy whose start stage had begun may have left its
						// candidate running beside a stopped previous version. Flag the service
						// versions_kept at once (the DB only): the run-time refusals then keep a
						// queued restart or stop-first deploy from running a second version while
						// the resync (which opens the scheduler's gate before it removes the
						// candidate) settles it. Another reason on the service is not overwritten.
						if _, err := tx.ExecContext(ctx, `UPDATE services SET needs_attention = ? WHERE id = (SELECT service_id FROM deploys WHERE id = ?)
							AND (needs_attention IS NULL OR needs_attention = ?)
							AND EXISTS (SELECT 1 FROM deploys d WHERE d.id = ? AND json_valid(d.snapshot_json) AND json_extract(d.snapshot_json, '$.strategy') = 'stop_first')
							AND EXISTS (SELECT 1 FROM deploy_stages WHERE deploy_id = ? AND stage = 'start' AND state IN ('running', 'ok'))`,
							AttentionVersionsKept, dep, AttentionPreviousDown, dep, dep); err != nil {
							return err
						}
					}
					if committed {
						jobState, jobErr = jobs.StateSucceeded, ""
						if _, err := tx.ExecContext(ctx, `UPDATE deploys SET status = 'succeeded', finished_at = ?, reason_code = NULL,
							reason = 'finished after a restart: the previous version is stopped at start-up' WHERE id = ? AND status IN ('queued', 'running')`, ts, dep); err != nil {
							return err
						}
					} else if _, err := tx.ExecContext(ctx, `UPDATE deploys SET status = 'failed', finished_at = ?, reason_code = 'interrupted',
						reason = 'interrupted by restart', app_safe = COALESCE(app_safe, 'unknown') WHERE id = ? AND status IN ('queued', 'running')`, ts, dep); err != nil {
						return err
					}
					// Its unfinished stages: for an uncommitted deploy a running one failed with it;
					// for a committed one (finished at start-up) the stages did not fail, they
					// were left behind by the restart.
					stageState, detail := "failed", "interrupted by restart"
					if committed {
						stageState, detail = "skipped", "finished at start-up"
					}
					if _, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = ?, finished_at = ?, detail = ?
						WHERE deploy_id = ? AND state = 'running'`, stageState, ts, detail, dep); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `UPDATE deploy_stages SET state = 'skipped' WHERE deploy_id = ? AND state = 'pending'`, dep); err != nil {
						return err
					}
				}
			}
			if j.kind == jobs.KindServiceDelete {
				// Every step of a delete is idempotent: the service is flagged and the
				// user deletes it again.
				if _, err := tx.ExecContext(ctx, `UPDATE services SET needs_attention = ? WHERE id = (SELECT json_extract(payload_json, '$.service') FROM jobs WHERE id = ?)`,
					AttentionDeleteInterrupted, j.id); err != nil {
					return err
				}
			}
			var e any
			if jobErr != "" {
				e = jobErr
			}
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, finished_at = ?, error = ? WHERE id = ? AND state = 'running'`, jobState, ts, e, j.id); err != nil {
				return err
			}
			settled = append(settled, settledJob{reqID: j.reqID, ok: jobState == jobs.StateSucceeded, errText: jobErr})
			n++
		}
		return nil
	})
	if err != nil || aud == nil {
		return n, err
	}
	for _, j := range settled {
		resolveIntent(ctx, st, aud, j)
	}
	return n, nil
}

// settledJob is a job the boot pass ended, for its audit outcome.
type settledJob struct {
	reqID   string
	ok      bool
	errText string
}

// resolveIntent writes the one outcome row of the job's intent, if the intent exists
// and has none yet. It never fails the boot: the intent stays unknown if it cannot.
func resolveIntent(ctx context.Context, st *store.Store, aud audit.Appender, j settledJob) {
	var intent audit.Entry
	var id int64
	var executor string
	err := st.R.QueryRowContext(ctx, `SELECT id, actor_kind, actor_id, COALESCE(executor, ''), action, target, risk, args_json FROM audit_log
		WHERE phase = ? AND request_id = ? AND action IN ('jobs.run_deploy', 'jobs.run_restart', 'jobs.run_service_delete')
		AND NOT EXISTS (SELECT 1 FROM audit_log o WHERE o.phase = ? AND o.intent_id = audit_log.id)
		ORDER BY id DESC LIMIT 1`, audit.PhaseIntent, j.reqID, audit.PhaseOutcome).
		Scan(&id, &intent.ActorKind, &intent.ActorID, &executor, &intent.Action, &intent.Target, &intent.Risk, &intent.ArgsJSON)
	if err != nil {
		return // no intent (the run never got that far) or already resolved
	}
	intent.Executor = "system" // the job ran as its actor with executor system; this outcome is the system's too
	intent.Phase, intent.IntentID, intent.RequestID = audit.PhaseOutcome, id, j.reqID
	intent.Result = audit.ResultOK
	if !j.ok {
		intent.Result, intent.Error = audit.ResultError, j.errText
	}
	if _, err := aud.Append(context.WithoutCancel(ctx), intent); err != nil {
		slog.Warn("the audit outcome of an interrupted job could not be written; its intent stays unknown", "request_id", j.reqID, "err", err.Error())
	}
}

// correctIntent gives the run of a deploy whose commit turned out to have happened a
// later "ok" outcome: its intent was resolved "error" when the deploy was ended failed
// (an unknown commit outcome), and the latest outcome decides (audit.IntentStatus).
func correctIntent(ctx context.Context, st *store.Store, aud audit.Appender, log *slog.Logger, deploy int64) {
	var id int64
	var e audit.Entry
	err := st.R.QueryRowContext(ctx, `SELECT a.id, a.actor_kind, a.actor_id, a.action, a.target, a.risk, a.args_json, a.request_id
		FROM audit_log a JOIN jobs j ON a.request_id = j.request_id JOIN deploys d ON d.job_id = j.id
		WHERE d.id = ? AND a.phase = ? AND a.action = 'jobs.run_deploy' AND
		COALESCE((SELECT o.result FROM audit_log o WHERE o.phase = ? AND o.intent_id = a.id ORDER BY o.id DESC LIMIT 1), '') = ?
		ORDER BY a.id DESC LIMIT 1`, deploy, audit.PhaseIntent, audit.PhaseOutcome, audit.ResultError).
		Scan(&id, &e.ActorKind, &e.ActorID, &e.Action, &e.Target, &e.Risk, &e.ArgsJSON, &e.RequestID)
	if err != nil {
		return // no intent, or its outcome is not an error
	}
	e.Executor, e.Phase, e.IntentID, e.Result = "system", audit.PhaseOutcome, id, audit.ResultOK
	e.Error = "corrected after a restart: the commit was found recorded" // the note that says why a later outcome contradicts the first
	if _, err := aud.Append(context.WithoutCancel(ctx), e); err != nil {
		log.Warn("the corrected audit outcome of a committed deploy could not be written", "deploy", deploy, "err", err.Error())
	}
}
