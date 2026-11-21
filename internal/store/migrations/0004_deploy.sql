-- 0004: deploy from image (docs/plans/0004, M3a step 4a). Never edited once
-- merged: later changes are 0005 and up (the runner checks this file's checksum).
--
-- Times in these tables are TEXT, UTC, fixed width ("2006-01-02T15:04:05.000Z"),
-- so they sort and compare as strings. Every id is AUTOINCREMENT: an id is never
-- reused, even after the newest row was deleted, so a container name or label
-- (which carries project, service and deploy ids) can never be adopted by a
-- later row. foreign_keys(1) is on for every connection, so every foreign key
-- states what a delete does. The two cycles (services.active_deploy_id and
-- deploys.job_id) are resolved by SQLite when a row is written, not when the
-- table is created.
--
-- Not stored here: meta.app_subnet is written by the server at first start (it
-- depends on the host and Docker), and never changed afterwards.

CREATE TABLE projects (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
	created_at TEXT NOT NULL
) STRICT;

CREATE TABLE jobs (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	kind           TEXT NOT NULL,                  -- deploy | restart | service_delete (M6 adds cron kinds)
	lock_key       TEXT NOT NULL,                  -- "service:<id>"
	state          TEXT NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
	actor_kind     TEXT NOT NULL,
	actor_id       TEXT NOT NULL,
	actor_session  TEXT,                           -- the queuing session's public id (user actors from REST)
	actor_blob     TEXT NOT NULL,                  -- actions.Actor job encoding (never kind "system" for user work)
	confirm        TEXT,                           -- the Confirm value of a destructive request
	enqueued_at    TEXT NOT NULL,
	payload_json   TEXT NOT NULL,                  -- at most 64 KiB, no secrets
	attempt        INTEGER NOT NULL DEFAULT 1,
	max_attempts   INTEGER NOT NULL DEFAULT 1,
	run_after      TEXT NOT NULL,                  -- due time, UTC
	timeout_s      INTEGER NOT NULL,               -- derived from the snapshot settings
	cancel_requested INTEGER NOT NULL DEFAULT 0,
	cancel_reason  TEXT,
	boot_id        TEXT,                           -- the boot that set it running
	request_id     TEXT,                           -- "job-<id>-a<attempt>": the audit request id of its run
	created_at     TEXT NOT NULL,
	started_at     TEXT,
	finished_at    TEXT,
	error          TEXT
) STRICT;
CREATE INDEX jobs_due ON jobs(state, run_after);
CREATE INDEX jobs_session ON jobs(actor_session) WHERE state IN ('queued', 'running');
CREATE UNIQUE INDEX jobs_one_running_per_lock ON jobs(lock_key) WHERE state = 'running';

CREATE TABLE services (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id       INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
	name             TEXT NOT NULL,
	image_ref        TEXT NOT NULL,                -- desired image for the next deploy (validated reference)
	container_port   INTEGER,                      -- the port the app listens on; NULL = no HTTP entry (worker)
	public_port      INTEGER UNIQUE,               -- host port Caddy serves; NULL = not exposed
	health_kind      TEXT NOT NULL,                -- http | tcp | running
	health_path      TEXT,                         -- http only
	health_timeout_s INTEGER NOT NULL,             -- 5..600, default 60
	health_passes    INTEGER NOT NULL,             -- consecutive passes, 1..10, default 3 (1 s apart)
	strategy         TEXT NOT NULL,                -- blue_green | stop_first
	drain_s          INTEGER NOT NULL,             -- 10..300, default 10 (never below Caddy's 10 s reload grace)
	stop_grace_s     INTEGER NOT NULL,             -- 0..300, default 10
	active_deploy_id INTEGER REFERENCES deploys(id) ON DELETE SET NULL,   -- THE commit point
	needs_attention  TEXT,                         -- NULL, or a reason code (delete_interrupted, ...)
	created_at       TEXT NOT NULL,
	updated_at       TEXT NOT NULL,
	UNIQUE(project_id, name)
) STRICT;

CREATE TABLE deploys (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	service_id       INTEGER NOT NULL REFERENCES services(id) ON DELETE CASCADE,
	number           INTEGER NOT NULL,             -- per-service "#128"
	job_id           INTEGER REFERENCES jobs(id) ON DELETE SET NULL,   -- jobs are pruned after 30 days
	kind             TEXT NOT NULL,                -- deploy | redeploy | rollback
	source_deploy_id INTEGER,                      -- redeploy/rollback: the deploy whose snapshot was copied (no FK: history)
	snapshot_json    TEXT NOT NULL,                -- immutable (trigger below)
	ip               TEXT,                         -- static IPv4, allocated at the check stage (NULL before)
	image_id         TEXT,
	repo_digest      TEXT,                         -- resolved after the pull
	status           TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled', 'superseded')),
	stage            TEXT,
	reason_code      TEXT,
	reason           TEXT,                         -- at most 512 bytes, cleaned
	app_safe         TEXT,                         -- yes | no | unknown: did the previous version keep serving?
	superseded_by    INTEGER,                      -- deploy id (no FK: history)
	committed_at     TEXT,                         -- set in the commit transaction: "was once live" (rollback targets)
	actor_kind       TEXT NOT NULL,
	actor_id         TEXT NOT NULL,
	created_at       TEXT NOT NULL,
	started_at       TEXT,
	finished_at      TEXT,
	UNIQUE(service_id, number)
) STRICT;
CREATE UNIQUE INDEX deploys_ip_live ON deploys(ip) WHERE ip IS NOT NULL AND status IN ('queued', 'running');
CREATE INDEX deploys_job ON deploys(job_id) WHERE job_id IS NOT NULL;
CREATE TRIGGER deploys_snapshot_immutable BEFORE UPDATE OF snapshot_json ON deploys
BEGIN
	SELECT RAISE(ABORT, 'deploy snapshots are immutable');
END;

CREATE TABLE deploy_stages (
	deploy_id   INTEGER NOT NULL REFERENCES deploys(id) ON DELETE CASCADE,
	stage       TEXT NOT NULL,
	state       TEXT NOT NULL,                     -- pending | running | ok | failed | skipped
	started_at  TEXT,
	finished_at TEXT,
	detail      TEXT,                              -- at most 256 bytes, cleaned
	PRIMARY KEY (deploy_id, stage)
) STRICT;

INSERT OR IGNORE INTO meta (key, value) VALUES ('install_id', lower(hex(randomblob(16))));
