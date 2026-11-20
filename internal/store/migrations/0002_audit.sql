-- 0002: the hash-chained, append-only audit log (docs/plans/0002, M1 step 8).
CREATE TABLE audit_log (
	id         INTEGER PRIMARY KEY,
	ts         TEXT    NOT NULL,
	boot_id    TEXT    NOT NULL,
	actor_kind TEXT    NOT NULL,
	actor_id   TEXT    NOT NULL,
	executor   TEXT,                  -- NULL, or 'system' when a job runs it on the actor's behalf
	action     TEXT    NOT NULL,
	target     TEXT    NOT NULL,
	risk       TEXT    NOT NULL,
	dry_run    INTEGER NOT NULL,
	phase      TEXT    NOT NULL,      -- intent | outcome | denied
	intent_id  INTEGER,               -- on an outcome: the intent it resolves
	args_json  TEXT    NOT NULL,      -- redacted
	result     TEXT    NOT NULL,      -- pending | ok | error | denied
	error      TEXT    NOT NULL,      -- redacted
	approver   TEXT,
	request_id TEXT    NOT NULL,
	prev_hash  BLOB    NOT NULL,
	hash       BLOB    NOT NULL
) STRICT;
CREATE INDEX audit_log_intent ON audit_log(intent_id);
-- Append-only. (A guard against our own bugs; Verify is what detects tampering
-- by someone who can edit the file.)
CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
