package deploy

import (
	"context"
	"database/sql"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// ResolveInterrupted is the DB-only part of the boot pass (plan 0004,
// "Reconciler"): every job still "running" from an earlier boot (a crash, a kill,
// or a shutdown that left it for this pass) is settled before the scheduler
// starts, so it cannot hold its service's lock key forever. For a deploy, the
// commit point is authoritative: if services.active_deploy_id names it, the deploy
// is succeeded; if not, it is failed "interrupted" with app_safe unknown (the
// reconciler's resync, step 10, finds out whether the previous version runs and
// removes the candidate; nothing here touches Docker). Other kinds are failed
// "interrupted". Passes that change nothing write nothing.
//
// This is the part of step 10's boot pass that step 8a needs on its own, because
// 8a is the first step that can leave a job running across a restart. Step 10
// extends it (app_safe from the resync, the audit outcome of the intent, the
// service_delete needs_attention flag) and runs it before the first resync.
//
// bootID is the new scheduler's boot id: only jobs with another one are settled.
func ResolveInterrupted(ctx context.Context, st *store.Store, bootID string, now time.Time) (int, error) {
	n := 0
	err := st.WriteTx(ctx, func(tx *sql.Tx) error {
		n = 0
		rows, err := tx.QueryContext(ctx, `SELECT id, kind FROM jobs WHERE state = 'running' AND (boot_id IS NULL OR boot_id <> ?) ORDER BY id`, bootID)
		if err != nil {
			return err
		}
		type jr struct {
			id   int64
			kind string
		}
		var list []jr
		for rows.Next() {
			var j jr
			if err := rows.Scan(&j.id, &j.kind); err != nil {
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
			var e any
			if jobErr != "" {
				e = jobErr
			}
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ?, finished_at = ?, error = ? WHERE id = ? AND state = 'running'`, jobState, ts, e, j.id); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
