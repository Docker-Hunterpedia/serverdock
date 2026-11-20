package audit

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
)

// VerifyResult is the outcome of walking the chain.
type VerifyResult struct {
	OK bool
	// BrokenID is the first row whose link or hash does not check out (0 when OK).
	BrokenID int64
	Rows     int64
	Head     []byte
}

// Verify walks audit_log in id order and returns the first broken id: a row
// whose prev_hash is not the previous row's hash (a deleted or swapped row), or
// whose stored hash does not match its fields. ro should be a read-only handle
// (store.OpenReadOnly); Verify never writes.
func Verify(ctx context.Context, ro *sql.DB) (VerifyResult, error) {
	rows, err := ro.QueryContext(ctx, `SELECT id, ts, boot_id, actor_kind, actor_id, executor, action, target, risk, dry_run,
		phase, intent_id, args_json, result, error, approver, request_id, prev_hash, hash FROM audit_log ORDER BY id`)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("audit: verify: %w", err)
	}
	defer rows.Close()
	res := VerifyResult{OK: true}
	prev := Genesis
	for rows.Next() {
		var r row
		var executor, approver sql.NullString
		var intent sql.NullInt64
		var dry int
		if err := rows.Scan(&r.ID, &r.TS, &r.BootID, &r.ActorKind, &r.ActorID, &executor, &r.Action, &r.Target, &r.Risk, &dry,
			&r.Phase, &intent, &r.ArgsJSON, &r.Result, &r.Error, &approver, &r.RequestID, &r.Prev, &r.Hash); err != nil {
			return VerifyResult{}, fmt.Errorf("audit: verify: %w", err)
		}
		r.Executor, r.Approver, r.IntentID, r.DryRun = executor.String, approver.String, intent.Int64, dry != 0
		res.Rows++
		if res.OK && (!bytes.Equal(r.Prev, prev) || !bytes.Equal(r.compute(), r.Hash)) {
			res.OK, res.BrokenID = false, r.ID
		}
		prev = r.Hash
		res.Head = r.Hash
	}
	if err := rows.Err(); err != nil {
		return VerifyResult{}, fmt.Errorf("audit: verify: %w", err)
	}
	return res, nil
}
