package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/datadir"
	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// Lifetimes and caps (docs/plans/0003, "Sessions", "Auth state machine").
const (
	SetupTokenPrefix = "sd_setup_"
	// SetupTokenFile is the name, inside DATA, of the file the setup token is written to.
	SetupTokenFile = "setup-token"
	SetupTokenTTL  = time.Hour

	SessionIdle        = 12 * time.Hour
	SessionAbsolute    = 7 * 24 * time.Hour
	MFAFresh           = 5 * time.Minute
	MaxSessionsPerUser = 20

	PendingTTL        = 5 * time.Minute
	PendingTries      = 5
	MaxPendingPerUser = 5

	pruneBatch      = 100
	touchEvery      = time.Minute
	maxKnownIPs     = 10
	knownIPLifetime = 30 * 24 * time.Hour
	maxUserAgent    = 200
	maxTokenInput   = 256

	// bestEffortTimeout bounds the writes a read request makes on the side.
	bestEffortTimeout = 250 * time.Millisecond
)

// Errors of the setup and sign-in transitions. The HTTP layer maps them to
// statuses; none carries a secret.
var (
	ErrAdminExists        = errors.New("auth: an admin already exists")                        // 404
	ErrTokenInvalid       = errors.New("auth: the setup token is wrong")                       // 401
	ErrSetupExpired       = errors.New("auth: the setup token has expired")                    // 403 setup_token_expired
	ErrTokenConsumed      = errors.New("auth: the setup token was already used")               // 409
	ErrEnrollmentReplaced = errors.New("auth: this enrolment was replaced; scan the new code") // 409
	ErrBadCode            = errors.New("auth: the code is wrong")
	ErrCodeReused         = errors.New("auth: that code was already used")
	ErrKeyMismatch        = errors.New("the server's encryption key doesn't match its data; see the docs (admin reset)")
	ErrPendingInvalid     = errors.New("auth: the sign-in step is unknown, used up or expired")
	// ErrNoSession means no session credential was presented at all.
	ErrNoSession = errors.New("auth: no session credential")
)

// SessionError is returned by LookupSession for a session that cannot be used.
// Reason is idle_expired, absolute_expired or revoked (a session that is not in
// the database, deleted by a logout, a revocation or an earlier expiry check).
// The HTTP layer clears the cookie on any of them.
type SessionError struct{ Reason string }

func (e *SessionError) Error() string { return "auth: session " + e.Reason }

// Options configures a Store.
type Options struct {
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log receives warnings (best-effort write failures, a failed token file
	// removal, a key that does not match). Never a secret. Nil discards.
	Log *slog.Logger
}

// Store is the authentication state in the database: the setup token, the admin,
// TOTP, recovery codes, pending sign-ins and sessions.
type Store struct {
	st     *store.Store
	box    *secretbox.Box
	hasher *Hasher
	log    *slog.Logger
	now    func() time.Time

	lastWarn atomic.Int64 // unix nanoseconds of the last rate-limited warning

	// Test hooks.
	write     func(ctx context.Context, fn func(*sql.Tx) error) error // default st.WriteTx
	touchTx   func(ctx context.Context, fn func(*sql.Tx) error) error // default write; the best-effort writes
	afterStmt func(n int) error                                       // called after each statement of SetupComplete's transaction
}

// NewStore returns a Store on st. box seals TOTP seeds; hasher hashes passwords.
func NewStore(st *store.Store, box *secretbox.Box, hasher *Hasher, o Options) *Store {
	s := &Store{st: st, box: box, hasher: hasher, log: o.Log, now: o.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.write = st.WriteTx
	s.touchTx = func(ctx context.Context, fn func(*sql.Tx) error) error { return s.write(ctx, fn) }
	return s
}

func (s *Store) tokenFilePath() string { return filepath.Join(s.st.Dir, SetupTokenFile) }

// warnOncePerMinute logs at WARN at most once a minute (best-effort failures
// repeat on every request, and must not flood the log).
func (s *Store) warnOncePerMinute(msg string, args ...any) {
	now := s.now().UnixNano()
	if last := s.lastWarn.Load(); now-last >= int64(time.Minute) && s.lastWarn.CompareAndSwap(last, now) {
		s.log.Warn(msg, args...)
	}
}

// AdminExists is the definition used everywhere: a user with a password and a
// confirmed TOTP. A reset user (password and TOTP cleared) is not an admin.
func (s *Store) AdminExists(ctx context.Context) (bool, error) { return adminExists(ctx, s.st.R) }

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func adminExists(ctx context.Context, q queryer) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE password_hash IS NOT NULL AND totp_confirmed_at IS NOT NULL)`).Scan(&ok)
	return ok, err
}

// openSeed opens a sealed seed. A key that does not match is reported once a
// minute with the table and column (never the bytes) and returned as ErrKeyMismatch.
func (s *Store) openSeed(table, column, aad string, ct []byte) ([]byte, error) {
	pt, err := s.box.Open(aad, ct)
	if errors.Is(err, secretbox.ErrWrongKey) {
		s.warnOncePerMinute("encrypted column was sealed with a different master key; see the docs (admin reset)", "table", table, "column", column)
		return nil, ErrKeyMismatch
	}
	return pt, err
}

// ---- setup token -----------------------------------------------------------

// SetupInfo describes the setup token written at start. The token itself is
// only in the file.
type SetupInfo struct {
	TokenFile string
	ExpiresAt time.Time
}

// PrepareSetup runs at every start. With no admin it generates a new setup token
// (256 bits), stores its SHA-256 and a 1 h expiry (clearing any pending
// enrolment) and writes the token to DATA/setup-token (0600, atomically). A
// failed file write fails the start. With an admin it deletes the setup row and
// any stale token file and returns nil.
func (s *Store) PrepareSetup(ctx context.Context) (*SetupInfo, error) {
	exists, err := s.AdminExists(ctx)
	if err != nil {
		return nil, err
	}
	path := s.tokenFilePath()
	if exists {
		if err := s.write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM setup_state`)
			return err
		}); err != nil {
			return nil, err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("could not remove a stale setup token file", "path", path, "err", err.Error())
		}
		return nil, nil
	}
	secret, _, err := newSecret()
	if err != nil {
		return nil, err
	}
	token := SetupTokenPrefix + secret
	hash := hashSecret(token)
	now := s.now()
	expires := now.Add(SetupTokenTTL)
	if err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO setup_state (id, token_hash, expires_at, enrollment_hash, pending_seed, created_at)
			VALUES (1, ?, ?, NULL, NULL, ?)
			ON CONFLICT(id) DO UPDATE SET token_hash = excluded.token_hash, expires_at = excluded.expires_at,
			  enrollment_hash = NULL, pending_seed = NULL, created_at = excluded.created_at`, hash, expires.Unix(), now.Unix())
		return err
	}); err != nil {
		return nil, err
	}
	if err := datadir.WriteAtomic(path, []byte(token+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("writing the setup token file %s: %w", path, err)
	}
	return &SetupInfo{TokenFile: path, ExpiresAt: expires}, nil
}

// StartupNotice is the one start-up log line while no admin exists: the token
// file's path (never the token), its expiry, the panel certificate fingerprint
// when there is one, and the SSH-tunnel hint (docs/plans/0003, G5).
func StartupNotice(info SetupInfo, panelFingerprint, httpsPort string) string {
	msg := fmt.Sprintf("no admin yet: setup token written to %s (expires %s UTC)", info.TokenFile, info.ExpiresAt.UTC().Format("15:04"))
	if panelFingerprint != "" {
		msg += "; panel certificate SHA-256 " + panelFingerprint
	}
	if httpsPort == "" {
		httpsPort = "443"
	}
	return msg + "; safest first run: ssh -L 8443:127.0.0.1:" + httpsPort + " you@this-server, then open https://localhost:8443/setup"
}

// SetupStatus is what the status endpoint reports.
type SetupStatus struct {
	Required   bool
	ExpiresAt  time.Time // zero when not required
	ServerTime time.Time
}

// SetupStatus reports whether setup is open and until when.
func (s *Store) SetupStatus(ctx context.Context) (SetupStatus, error) {
	st := SetupStatus{ServerTime: s.now().UTC()}
	exists, err := s.AdminExists(ctx)
	if err != nil || exists {
		return st, err
	}
	var exp int64
	err = s.st.R.QueryRowContext(ctx, `SELECT expires_at FROM setup_state WHERE id = 1`).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return SetupStatus{ServerTime: st.ServerTime, Required: true}, nil
	}
	if err != nil {
		return st, err
	}
	return SetupStatus{Required: true, ExpiresAt: time.Unix(exp, 0).UTC(), ServerTime: st.ServerTime}, nil
}

type setupRow struct {
	tokenHash, enrollHash, seed []byte
	expires                     int64
}

// readSetup reads the setup row and checks the token and the expiry, in that
// order (a wrong token learns nothing about the expiry).
func (s *Store) readSetup(ctx context.Context, token string) (setupRow, error) {
	var r setupRow
	if exists, err := s.AdminExists(ctx); err != nil {
		return r, err
	} else if exists {
		return r, ErrAdminExists
	}
	if len(token) > maxTokenInput {
		return r, ErrTokenInvalid
	}
	err := s.st.R.QueryRowContext(ctx, `SELECT token_hash, expires_at, enrollment_hash, pending_seed FROM setup_state WHERE id = 1`).
		Scan(&r.tokenHash, &r.expires, &r.enrollHash, &r.seed)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrTokenInvalid
	}
	if err != nil {
		return r, err
	}
	if subtle.ConstantTimeCompare(hashSecret(token), r.tokenHash) != 1 {
		return r, ErrTokenInvalid
	}
	if s.now().Unix() >= r.expires {
		return r, ErrSetupExpired
	}
	return r, nil
}

// EnrollResult is shown once, in the enrolment response.
type EnrollResult struct {
	EnrollmentID string // 16 random bytes, base64url; only its SHA-256 is stored
	Secret       string // base32 seed
	URI          string // otpauth://
}

// SetupEnroll checks the setup token and creates a pending TOTP seed and an
// enrollment_id, replacing any earlier enrolment (whose completion then fails
// with ErrEnrollmentReplaced). label names the account in the authenticator app.
func (s *Store) SetupEnroll(ctx context.Context, token, label string) (EnrollResult, error) {
	r, err := s.readSetup(ctx, token)
	if err != nil {
		return EnrollResult{}, err
	}
	seed, err := NewTOTPSeed()
	if err != nil {
		return EnrollResult{}, err
	}
	ct, err := s.box.Seal("setup_state/1/pending_seed", seed)
	if err != nil {
		return EnrollResult{}, err
	}
	var idRaw [16]byte
	if _, err := rand.Read(idRaw[:]); err != nil {
		return EnrollResult{}, err
	}
	id := base64.RawURLEncoding.EncodeToString(idRaw[:])
	err = s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE setup_state SET enrollment_hash = ?, pending_seed = ? WHERE id = 1 AND token_hash = ? AND expires_at > ?`,
			hashSecret(id), ct, r.tokenHash, s.now().Unix())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrTokenConsumed
		}
		return nil
	})
	if err != nil {
		return EnrollResult{}, err
	}
	if label == "" {
		label = "admin"
	}
	return EnrollResult{EnrollmentID: id, Secret: TOTPSecret(seed), URI: TOTPURI(seed, label)}, nil
}

// SetupComplete is the input of SetupComplete.
type SetupComplete struct {
	Token        string
	EnrollmentID string
	Username     string
	Password     string
	Code         string // the current TOTP code from the pending seed
	IPKey        string // limiter key of the client; recorded as the setup IP
	UserAgent    string
	// HostKind is how the panel was reached: "ip", "localhost" or "name"
	// (recorded so M4 can recommend re-enrolment once a domain has a certificate).
	HostKind string
}

// SetupResult is returned once, after the admin was created.
type SetupResult struct {
	UserID        int64
	RecoveryCodes []string // shown once
	SessionSecret string   // the cookie value of the first session
	Session       Session
}

// SetupComplete finishes first-run setup atomically.
//
// Order (docs/plans/0003, G4b): read-only checks before any hashing (admin
// exists, token, expiry, enrolment, the TOTP code from the server-generated
// seed, the password rules); then argon2; then ONE transaction that
// compare-and-deletes the setup row (it must affect exactly one row), writes the
// user, the sealed seed and the accepted counter, ten recovery-code hashes and
// the first session. An admin therefore never exists without a confirmed TOTP,
// and twenty concurrent completions produce exactly one admin.
func (s *Store) SetupComplete(ctx context.Context, in SetupComplete) (SetupResult, error) {
	var zero SetupResult
	chk, err := s.checkSetupComplete(ctx, &in)
	if err != nil {
		return zero, err
	}
	r, seed, counter := chk.row, chk.seed, chk.counter
	phc, err := s.hasher.Hash(ctx, in.Password) // outside any transaction
	if err != nil {
		return zero, err
	}
	codes, codeHashes, err := NewRecoveryCodes()
	if err != nil {
		return zero, err
	}

	var res SetupResult
	n := 0
	err = s.write(ctx, func(tx *sql.Tx) error {
		n = 0
		stmt := func(q string, args ...any) (sql.Result, error) {
			out, err := tx.ExecContext(ctx, q, args...)
			if err != nil {
				return nil, err
			}
			n++
			if s.afterStmt != nil {
				if err := s.afterStmt(n); err != nil {
					return nil, err
				}
			}
			return out, nil
		}
		now := s.now().Unix()
		// Read first, then compare-and-delete by rows affected (no RETURNING).
		del, err := stmt(`DELETE FROM setup_state WHERE id = 1 AND token_hash = ? AND enrollment_hash = ? AND expires_at > ?`,
			r.tokenHash, r.enrollHash, now)
		if err != nil {
			return err
		}
		if k, _ := del.RowsAffected(); k != 1 {
			return ErrTokenConsumed
		}
		// After `admin reset` the user row is kept (and its id): reuse it.
		var id int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE password_hash IS NULL OR totp_confirmed_at IS NULL ORDER BY id LIMIT 1`).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			ins, err := stmt(`INSERT INTO users (username, scope, created_at) VALUES (?, 'admin', ?)`, in.Username, now)
			if err != nil {
				return mapUnique(err)
			}
			if id, err = ins.LastInsertId(); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		sealed, err := s.box.Seal(fmt.Sprintf("users/%d/totp_seed", id), seed)
		if err != nil {
			return err
		}
		if _, err := stmt(`UPDATE users SET username = ?, password_hash = ?, totp_seed = ?, totp_confirmed_at = ?, totp_last_counter = ?,
			totp_enrolled_host = ?, scope = 'admin', setup_ip = ?, setup_at = ?, password_changed_at = ?,
			totp_pending_seed = NULL, totp_pending_expires_at = NULL, totp_pending_enrollment_hash = NULL, totp_pending_tries = 0,
			totp_fail_since_login = 0, totp_fail_last_at = NULL, totp_fail_last_ip = NULL WHERE id = ?`,
			in.Username, phc, sealed, now, counter, in.HostKind, in.IPKey, now, now, id); err != nil {
			return mapUnique(err)
		}
		if _, err := stmt(`DELETE FROM recovery_codes WHERE user_id = ?`, id); err != nil {
			return err
		}
		for _, h := range codeHashes {
			if _, err := stmt(`INSERT INTO recovery_codes (user_id, hash) VALUES (?, ?)`, id, h); err != nil {
				return err
			}
		}
		secret, sess, err := s.createSession(ctx, tx, id, in.IPKey, in.UserAgent, "{}", func() error {
			n++
			if s.afterStmt != nil {
				return s.afterStmt(n)
			}
			return nil
		})
		if err != nil {
			return err
		}
		res = SetupResult{UserID: id, RecoveryCodes: codes, SessionSecret: secret, Session: sess}
		return nil
	})
	if err != nil {
		return zero, err
	}
	// The token is spent; a failed removal is harmless (the row is gone).
	if err := os.Remove(s.tokenFilePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("could not remove the setup token file", "path", s.tokenFilePath(), "err", err.Error())
	}
	return res, nil
}

func mapUnique(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrBadUsername
	}
	return err
}

// AcceptTOTP records counter as the last accepted time step for the user. A
// counter at or below the stored one is a reuse (ErrCodeReused): the UPDATE is
// the atomic check, so concurrent attempts with one code produce one success.
func (s *Store) AcceptTOTP(ctx context.Context, userID, counter int64) error {
	return s.write(ctx, func(tx *sql.Tx) error { return acceptTOTP(ctx, tx, userID, counter) })
}

func acceptTOTP(ctx context.Context, tx *sql.Tx, userID, counter int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE users SET totp_last_counter = ? WHERE id = ? AND totp_last_counter < ?`, counter, userID, counter)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrCodeReused
	}
	return nil
}

// UserSeed returns the user's TOTP seed (opened from its sealed form).
func (s *Store) UserSeed(ctx context.Context, userID int64) ([]byte, error) {
	var ct []byte
	if err := s.st.R.QueryRowContext(ctx, `SELECT totp_seed FROM users WHERE id = ?`, userID).Scan(&ct); err != nil {
		return nil, err
	}
	if ct == nil {
		return nil, errors.New("auth: the user has no TOTP seed")
	}
	return s.openSeed("users", "totp_seed", fmt.Sprintf("users/%d/totp_seed", userID), ct)
}

// UseRecoveryCode marks a recovery code used. It reports false for an unknown or
// already used code. Codes are 80 random bits stored as SHA-256, so the lookup by
// hash reveals nothing a guess would not.
func (s *Store) UseRecoveryCode(ctx context.Context, userID int64, code string) (bool, error) {
	norm, ok := normaliseRecovery(code)
	if !ok {
		return false, nil
	}
	var used bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		used, err = useRecovery(ctx, tx, userID, norm, s.now().Unix())
		return err
	})
	return used, err
}

func useRecovery(ctx context.Context, tx *sql.Tx, userID int64, norm string, now int64) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND hash = ? AND used_at IS NULL`, now, userID, hashRecovery(norm))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RecoveryCodesLeft counts the unused codes.
func (s *Store) RecoveryCodesLeft(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.st.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL`, userID).Scan(&n)
	return n, err
}

// AdminCreated returns where (the client address key) and when first-run setup
// was completed for the user; empty and zero when not recorded.
func (s *Store) AdminCreated(ctx context.Context, userID int64) (from string, at time.Time, err error) {
	var ip sql.NullString
	var unix sql.NullInt64
	err = s.st.R.QueryRowContext(ctx, `SELECT setup_ip, setup_at FROM users WHERE id = ?`, userID).Scan(&ip, &unix)
	if err != nil {
		return "", time.Time{}, err
	}
	if unix.Valid {
		at = time.Unix(unix.Int64, 0)
	}
	return ip.String, at, nil
}

// TokenFile is where the setup token is written (the path is not secret).
func (s *Store) TokenFile() string { return s.tokenFilePath() }

// UserRecord is what the sign-in handler needs about a user name.
type UserRecord struct {
	ID           int64
	Username     string // as stored
	PasswordHash string // PHC; empty when the user has none (reset)
	// Admin: a password and a confirmed TOTP. A reset user is not one, and signs
	// in like a user that does not exist.
	Admin bool
}

// UserByName looks a user up case-insensitively. ok is false for no such user.
func (s *Store) UserByName(ctx context.Context, name string) (u UserRecord, ok bool, err error) {
	var ph sql.NullString
	var confirmed sql.NullInt64
	err = s.st.R.QueryRowContext(ctx, `SELECT id, username, password_hash, totp_confirmed_at FROM users WHERE username = ?`, name).Scan(&u.ID, &u.Username, &ph, &confirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return UserRecord{}, false, nil
	}
	if err != nil {
		return UserRecord{}, false, err
	}
	u.PasswordHash = ph.String
	u.Admin = ph.Valid && confirmed.Valid
	return u, true, nil
}

// Username returns a user's name.
func (s *Store) Username(ctx context.Context, id int64) (string, error) {
	var n string
	err := s.st.R.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, id).Scan(&n)
	return n, err
}

type setupChecked struct {
	row     setupRow
	seed    []byte
	counter int64
}

// checkSetupComplete is the read-only half of SetupComplete: admin exists, token,
// expiry, enrolment, the TOTP code from the server-generated seed, the user name
// and the password rules. It hashes nothing and writes nothing.
func (s *Store) checkSetupComplete(ctx context.Context, in *SetupComplete) (setupChecked, error) {
	var zero setupChecked
	r, err := s.readSetup(ctx, in.Token)
	if err != nil {
		return zero, err
	}
	if len(in.EnrollmentID) > maxTokenInput || r.enrollHash == nil || r.seed == nil ||
		subtle.ConstantTimeCompare(hashSecret(in.EnrollmentID), r.enrollHash) != 1 {
		return zero, ErrEnrollmentReplaced
	}
	seed, err := s.openSeed("setup_state", "pending_seed", "setup_state/1/pending_seed", r.seed)
	if err != nil {
		return zero, err
	}
	counter, ok := VerifyTOTP(seed, in.Code, s.now())
	if !ok {
		return zero, ErrBadCode
	}
	if err := ValidateUsername(in.Username); err != nil {
		return zero, err
	}
	if err := CheckPassword(in.Username, in.Password); err != nil {
		return zero, err
	}
	switch in.HostKind {
	case "ip", "localhost", "name":
	default:
		in.HostKind = "name"
	}
	return setupChecked{row: r, seed: seed, counter: counter}, nil
}

// PrecheckSetupComplete runs the read-only checks of SetupComplete, so a caller
// can refuse (and rate-limit, and audit as a denial) a bad request before it
// writes an audit intent or hashes anything.
func (s *Store) PrecheckSetupComplete(ctx context.Context, in SetupComplete) error {
	_, err := s.checkSetupComplete(ctx, &in)
	return err
}
