-- 0003: authentication (docs/plans/0003, M2 step 5). Never edited once merged:
-- later changes are 0004 and up (the runner checks this file's checksum).
--
-- Times are INTEGER unix seconds (UTC). Secrets are never stored in the clear:
-- passwords as argon2id PHC strings, session / pending / setup / API-token /
-- recovery-code secrets as SHA-256, TOTP seeds as secretbox ciphertext. The
-- ciphertext columns are listed in secretbox.Columns() (a test enforces it).

CREATE TABLE users (
	id                           INTEGER PRIMARY KEY,
	username                     TEXT    NOT NULL UNIQUE COLLATE NOCASE,
	password_hash                TEXT,                    -- argon2id PHC; NULL until setup completes (and after admin reset)
	totp_seed                    BLOB,                    -- secretbox, AAD users/<id>/totp_seed
	totp_pending_seed            BLOB,                    -- secretbox, AAD users/<id>/totp_pending_seed (re-enrolment)
	totp_pending_expires_at      INTEGER,
	totp_pending_enrollment_hash BLOB,                    -- SHA-256 of the enrollment_id
	totp_pending_tries           INTEGER NOT NULL DEFAULT 0,
	totp_confirmed_at            INTEGER,                 -- NULL until a code from the seed was accepted
	totp_last_counter            INTEGER NOT NULL DEFAULT 0, -- the last accepted time step: a code is never used twice
	totp_enrolled_host           TEXT CHECK (totp_enrolled_host IS NULL OR totp_enrolled_host IN ('ip', 'localhost', 'name')),
	scope                        TEXT    NOT NULL DEFAULT 'admin' CHECK (scope IN ('read', 'write', 'admin')),
	setup_ip                     TEXT,
	setup_at                     INTEGER,
	password_changed_at          INTEGER,
	totp_fail_since_login        INTEGER NOT NULL DEFAULT 0,
	totp_fail_last_at            INTEGER,
	totp_fail_last_ip            TEXT,
	created_at                   INTEGER NOT NULL
) STRICT;

CREATE TABLE recovery_codes (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	hash    BLOB    NOT NULL,                             -- SHA-256 of the normalised code
	used_at INTEGER,
	PRIMARY KEY (user_id, hash)
) STRICT;

CREATE TABLE sessions (
	id_hash         BLOB    PRIMARY KEY,                  -- SHA-256 of the cookie secret
	public_id       TEXT    NOT NULL UNIQUE,              -- shown in the sessions list
	user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at      INTEGER NOT NULL,
	last_seen_at    INTEGER NOT NULL,
	idle_expires_at INTEGER NOT NULL,
	abs_expires_at  INTEGER NOT NULL,
	mfa_fresh_until INTEGER NOT NULL,
	reauth_failures INTEGER NOT NULL DEFAULT 0,
	ip_key          TEXT    NOT NULL,
	user_agent      TEXT    NOT NULL,
	notice_json     TEXT    NOT NULL DEFAULT '{}'
) STRICT;
CREATE INDEX sessions_user ON sessions(user_id, last_seen_at);

CREATE TABLE auth_pending (
	id_hash    BLOB    PRIMARY KEY,                       -- SHA-256 of the pending-2FA cookie secret
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL,
	tries_left INTEGER NOT NULL,
	ip_key     TEXT    NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX auth_pending_user ON auth_pending(user_id, created_at);

CREATE TABLE auth_known_ips (
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	ip_key     TEXT    NOT NULL,
	last_ok_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, ip_key)
) STRICT;

CREATE TABLE api_tokens (
	id           INTEGER PRIMARY KEY,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT    NOT NULL,
	prefix       TEXT    NOT NULL,                        -- the displayable first characters
	hash         BLOB    NOT NULL UNIQUE,                 -- SHA-256 of the token
	scope        TEXT    NOT NULL CHECK (scope IN ('read', 'write', 'admin')),
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,                        -- required
	last_used_at INTEGER,
	revoked_at   INTEGER
) STRICT;

CREATE TABLE setup_state (
	id              INTEGER PRIMARY KEY CHECK (id = 1),
	token_hash      BLOB    NOT NULL,                     -- SHA-256 of the setup token
	expires_at      INTEGER NOT NULL,
	enrollment_hash BLOB,                                 -- SHA-256 of the enrollment_id
	pending_seed    BLOB,                                 -- secretbox, AAD setup_state/1/pending_seed
	created_at      INTEGER NOT NULL
) STRICT;
