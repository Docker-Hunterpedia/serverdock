-- 0005: service configuration (docs/plans/0005, M3b step 1). Never edited once
-- merged: later changes are 0006 and up (the runner checks this file's checksum).
--
-- Env versions are immutable and encrypted (secretbox). The AAD is
-- "service_env_versions/<service_id>/v<version>", which is known before the
-- insert, so a version is sealed and inserted in one step and the table needs no
-- UPDATE. Registry secrets use the AAD "registries/<host>/secret"; a host row is
-- never edited (a new host is a new row), so the AAD stays valid for the row's life.
--
-- services.env_version_id is ON DELETE RESTRICT: the current version cannot be
-- deleted while the service points at it. A service delete cascades to its
-- versions in the same statement (the service row is gone first), which is allowed.

CREATE TABLE service_env_versions (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	service_id  INTEGER NOT NULL REFERENCES services(id) ON DELETE CASCADE,
	version     INTEGER NOT NULL CHECK (version >= 1),
	ciphertext  BLOB NOT NULL,                    -- secretbox, AAD "service_env_versions/<service_id>/v<version>"
	names_json  TEXT NOT NULL,                    -- names and flags for display only; never authoritative
	actor_kind  TEXT NOT NULL,
	actor_id    TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	UNIQUE(service_id, version)
) STRICT;
CREATE TRIGGER service_env_versions_immutable BEFORE UPDATE ON service_env_versions
BEGIN
	SELECT RAISE(ABORT, 'env versions are immutable');
END;

CREATE TABLE service_volumes (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	service_id  INTEGER NOT NULL REFERENCES services(id) ON DELETE CASCADE,
	kind        TEXT NOT NULL CHECK (kind IN ('volume', 'bind')),
	name        TEXT,                             -- volume: short name ("data")
	host_path   TEXT,                             -- bind: validated absolute host path as entered
	target      TEXT NOT NULL,
	read_only   INTEGER NOT NULL DEFAULT 0 CHECK (read_only IN (0, 1)),
	detached_at TEXT,                             -- removed from the list; the Docker volume is kept
	created_at  TEXT NOT NULL,
	CHECK ((kind = 'volume' AND name IS NOT NULL AND host_path IS NULL)
	    OR (kind = 'bind' AND name IS NULL AND host_path IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX service_volumes_target ON service_volumes(service_id, target) WHERE detached_at IS NULL;
CREATE UNIQUE INDEX service_volumes_name ON service_volumes(service_id, name) WHERE name IS NOT NULL;

CREATE TABLE registries (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	host          TEXT NOT NULL UNIQUE,           -- normalised; never edited (a new host is a new row)
	username      TEXT,
	secret        BLOB,                           -- secretbox, AAD "registries/<host>/secret"
	allow_private INTEGER NOT NULL DEFAULT 0 CHECK (allow_private IN (0, 1)),
	created_at    TEXT NOT NULL,
	updated_at    TEXT NOT NULL,
	CHECK ((username IS NULL) = (secret IS NULL))
) STRICT;

ALTER TABLE services ADD COLUMN env_version_id INTEGER REFERENCES service_env_versions(id) ON DELETE RESTRICT;
ALTER TABLE services ADD COLUMN memory_mb INTEGER CHECK (memory_mb IS NULL OR memory_mb >= 32);
ALTER TABLE services ADD COLUMN cpus_milli INTEGER CHECK (cpus_milli IS NULL OR (cpus_milli >= 50 AND cpus_milli % 50 = 0));
ALTER TABLE services ADD COLUMN no_new_privileges INTEGER NOT NULL DEFAULT 0 CHECK (no_new_privileges IN (0, 1));
ALTER TABLE services ADD COLUMN last_oom_at TEXT;

-- A plain column (SQLite cannot ADD COLUMN ... UNIQUE); the partial index makes it
-- unique where it is set.
ALTER TABLE projects ADD COLUMN subnet TEXT;
CREATE UNIQUE INDEX projects_subnet ON projects(subnet) WHERE subnet IS NOT NULL;
