-- 0001: the Phase 0 baseline. A Phase 0 database already has this and is
-- adopted as v1 without running it. meta.schema_version is written here only
-- (for fresh databases) and never again: the version lives in schema_migrations.
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;
INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', '1');
