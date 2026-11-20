package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Notice is shown to the admin at sign-in: second-factor failures since the last
// successful sign-in (docs/plans/0003, "Admin notice").
type Notice struct {
	TOTPFailures  int    `json:"totp_failures,omitempty"`
	LastFailureAt int64  `json:"last_failure_at,omitempty"`
	LastFailureIP string `json:"last_failure_ip,omitempty"`
}

// Session is an authenticated browser session.
type Session struct {
	PublicID       string
	UserID         int64
	CreatedAt      time.Time
	LastSeenAt     time.Time
	IdleExpiresAt  time.Time
	AbsExpiresAt   time.Time
	MFAFreshUntil  time.Time
	ReauthFailures int
	IPKey          string
	UserAgent      string
	Notice         Notice
}

// createSession inserts a new session inside tx. A session id is only ever made
// here, by the server. Before inserting it prunes up to 100 expired sessions and
// 100 expired pending rows (lazy cleanup: no timer); after inserting it keeps the
// user's 20 most recently seen sessions. step, if not nil, is called after each
// statement (a test hook).
func (s *Store) createSession(ctx context.Context, tx *sql.Tx, userID int64, ipKey, userAgent, noticeJSON string, step func() error) (secret string, sess Session, err error) {
	secret, idHash, err := newSecret()
	if err != nil {
		return "", sess, err
	}
	public, err := randomHex(8)
	if err != nil {
		return "", sess, err
	}
	now := s.now()
	t := now.Unix()
	exec := func(q string, args ...any) error {
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
		if step != nil {
			return step()
		}
		return nil
	}
	if err := exec(`DELETE FROM sessions WHERE rowid IN (SELECT rowid FROM sessions WHERE idle_expires_at <= ? OR abs_expires_at <= ? LIMIT ?)`, t, t, pruneBatch); err != nil {
		return "", sess, err
	}
	if err := exec(`DELETE FROM auth_pending WHERE rowid IN (SELECT rowid FROM auth_pending WHERE expires_at <= ? LIMIT ?)`, t, pruneBatch); err != nil {
		return "", sess, err
	}
	sess = Session{
		PublicID: public, UserID: userID, CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: now.Add(SessionIdle), AbsExpiresAt: now.Add(SessionAbsolute), MFAFreshUntil: now.Add(MFAFresh),
		IPKey: ipKey, UserAgent: truncateUTF8(userAgent, maxUserAgent),
	}
	_ = json.Unmarshal([]byte(noticeJSON), &sess.Notice)
	if err := exec(`INSERT INTO sessions (id_hash, public_id, user_id, created_at, last_seen_at, idle_expires_at, abs_expires_at, mfa_fresh_until, reauth_failures, ip_key, user_agent, notice_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		idHash, public, userID, t, t, sess.IdleExpiresAt.Unix(), sess.AbsExpiresAt.Unix(), sess.MFAFreshUntil.Unix(), sess.IPKey, sess.UserAgent, noticeJSON); err != nil {
		return "", sess, err
	}
	// Keep the 20 most recently seen (oldest last_seen_at beyond 20 go).
	if err := exec(`DELETE FROM sessions WHERE user_id = ? AND rowid NOT IN
		(SELECT rowid FROM sessions WHERE user_id = ? ORDER BY last_seen_at DESC, rowid DESC LIMIT ?)`, userID, userID, MaxSessionsPerUser); err != nil {
		return "", sess, err
	}
	return secret, sess, nil
}

// LookupSession finds the session for a cookie secret. An unknown, expired or
// revoked session is a *SessionError (and an expired one is deleted); the caller
// clears the cookie. Otherwise it records activity at most once a minute,
// best-effort: a failed write is logged (at most once a minute) and the request
// goes on.
func (s *Store) LookupSession(ctx context.Context, secret string) (*Session, error) {
	if secret == "" {
		return nil, ErrNoSession // no credential at all: nothing to clear, nothing revoked
	}
	if len(secret) > maxTokenInput {
		return nil, &SessionError{Reason: "revoked"}
	}
	idHash := hashSecret(secret)
	var (
		sess                            Session
		created, last, idle, abs, fresh int64
		notice                          string
	)
	err := s.st.R.QueryRowContext(ctx, `SELECT public_id, user_id, created_at, last_seen_at, idle_expires_at, abs_expires_at, mfa_fresh_until, reauth_failures, ip_key, user_agent, notice_json
		FROM sessions WHERE id_hash = ?
		AND user_id IN (SELECT id FROM users WHERE password_hash IS NOT NULL AND totp_confirmed_at IS NOT NULL)`, idHash).
		Scan(&sess.PublicID, &sess.UserID, &created, &last, &idle, &abs, &fresh, &sess.ReauthFailures, &sess.IPKey, &sess.UserAgent, &notice)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &SessionError{Reason: "revoked"}
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	t := now.Unix()
	reason := ""
	switch {
	case t >= abs:
		reason = "absolute_expired"
	case t >= idle:
		reason = "idle_expired"
	}
	if reason != "" {
		// Delete on lookup. A failed delete is not fatal: the session is refused
		// either way and the next prune takes it.
		dctx, cancel := context.WithTimeout(ctx, bestEffortTimeout)
		defer cancel()
		if err := s.write(dctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(dctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
			return err
		}); err != nil {
			s.warnOncePerMinute("could not delete an expired session", "err", err.Error())
		}
		return nil, &SessionError{Reason: reason}
	}
	sess.CreatedAt, sess.LastSeenAt = time.Unix(created, 0), time.Unix(last, 0)
	sess.IdleExpiresAt, sess.AbsExpiresAt, sess.MFAFreshUntil = time.Unix(idle, 0), time.Unix(abs, 0), time.Unix(fresh, 0)
	_ = json.Unmarshal([]byte(notice), &sess.Notice)
	if now.Sub(sess.LastSeenAt) >= touchEvery {
		newIdle := min(t+int64(SessionIdle/time.Second), abs)
		// Best effort: a busy writer must not stall a read, so give up after 250 ms.
		tctx, cancel := context.WithTimeout(ctx, bestEffortTimeout)
		defer cancel()
		err := s.touchTx(tctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(tctx, `UPDATE sessions SET last_seen_at = ?, idle_expires_at = ? WHERE id_hash = ?`, t, newIdle, idHash)
			return err
		})
		if err != nil {
			s.warnOncePerMinute("could not record session activity; continuing", "err", err.Error())
		} else {
			sess.LastSeenAt, sess.IdleExpiresAt = now, time.Unix(newIdle, 0)
		}
	}
	return &sess, nil
}

// RevokeSession deletes the session for a cookie secret (logout). It is not an
// error if there is none.
func (s *Store) RevokeSession(ctx context.Context, secret string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, hashSecret(secret))
		return err
	})
}

// RevokeUserSessions deletes every session of the user (password or TOTP change)
// and returns how many. Pending second-factor rows of the user go with them, in
// the same transaction: a password-only step started before the change must not
// finish afterwards.
func (s *Store) RevokeUserSessions(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		_, err = tx.ExecContext(ctx, `DELETE FROM auth_pending WHERE user_id = ?`, userID)
		return err
	})
	return n, err
}

// ---- pending second factor (between password and TOTP) ---------------------

// CreatePending is called after a correct password. It returns the secret of the
// pending-2FA cookie (a pending row is NOT a session: it authenticates nothing
// but POST auth/2fa). Only its SHA-256 is stored, with a 5 minute expiry and 5
// tries. Before inserting it deletes up to 100 expired pending rows; after, it
// keeps at most 5 pending rows per user (oldest deleted), so a stream of
// password-only attempts cannot grow the table.
func (s *Store) CreatePending(ctx context.Context, userID int64, ipKey string) (string, error) {
	secret, idHash, err := newSecret()
	if err != nil {
		return "", err
	}
	now := s.now().Unix()
	err = s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_pending WHERE rowid IN (SELECT rowid FROM auth_pending WHERE expires_at <= ? LIMIT ?)`, now, pruneBatch); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_pending (id_hash, user_id, expires_at, tries_left, ip_key, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			idHash, userID, now+int64(PendingTTL/time.Second), PendingTries, ipKey, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM auth_pending WHERE user_id = ? AND rowid NOT IN
			(SELECT rowid FROM auth_pending WHERE user_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ?)`, userID, userID, MaxPendingPerUser)
		return err
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// PendingInfo is a pending sign-in that still has tries.
type PendingInfo struct {
	UserID int64
	// TriesLeft is what remains AFTER this attempt was counted.
	TriesLeft int
}

// PendingAttempt counts one try against the pending sign-in BEFORE the code is
// checked: tries_left is decremented atomically (one UPDATE, one row affected),
// so concurrent guesses cannot exceed 5. An unknown, used-up or expired pending
// secret is ErrPendingInvalid.
func (s *Store) PendingAttempt(ctx context.Context, secret string) (PendingInfo, error) {
	var info PendingInfo
	if secret == "" || len(secret) > maxTokenInput {
		return info, ErrPendingInvalid
	}
	idHash := hashSecret(secret)
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE auth_pending SET tries_left = tries_left - 1 WHERE id_hash = ? AND tries_left > 0 AND expires_at > ?`, idHash, s.now().Unix())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrPendingInvalid
		}
		return tx.QueryRowContext(ctx, `SELECT user_id, tries_left FROM auth_pending WHERE id_hash = ?`, idHash).Scan(&info.UserID, &info.TriesLeft)
	})
	return info, err
}

// PendingFailed is called after a wrong second factor: when no tries are left the
// pending row is deleted and the password is needed again.
func (s *Store) PendingFailed(ctx context.Context, secret string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM auth_pending WHERE id_hash = ? AND tries_left <= 0`, hashSecret(secret))
		return err
	})
}

// PendingUser returns the user a pending sign-in belongs to, without counting a
// try. The HTTP layer uses it to key the per-account back-off before the code is
// checked; it authenticates nothing.
func (s *Store) PendingUser(ctx context.Context, secret string) (int64, error) {
	if secret == "" || len(secret) > maxTokenInput {
		return 0, ErrPendingInvalid
	}
	var id int64
	err := s.st.R.QueryRowContext(ctx, `SELECT user_id FROM auth_pending WHERE id_hash = ? AND expires_at > ?`, hashSecret(secret), s.now().Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrPendingInvalid
	}
	return id, err
}

// LoginInput finishes a sign-in. The caller passes the raw second factor and
// never an "already verified" result: CompleteLogin counts the try, checks the
// code and mints the session itself, so no caller can mint a session without a
// valid pending row and a valid code.
type LoginInput struct {
	PendingSecret string
	// Exactly one of Code (a 6-digit TOTP code) and RecoveryCode is set.
	Code         string
	RecoveryCode string
	// OldSessionSecret is a session cookie presented with the request, if any: it
	// is revoked when the new session is minted.
	OldSessionSecret string
	IPKey, UserAgent string
}

// LoginResult is the new session.
type LoginResult struct {
	SessionSecret string
	Session       Session
}

// LoginError wraps ErrBadCode (or ErrPendingInvalid) with what the caller needs
// to answer and to count: the user and the tries left after this attempt.
type LoginError struct {
	Err       error
	UserID    int64
	TriesLeft int
}

func (e *LoginError) Error() string { return e.Err.Error() }
func (e *LoginError) Unwrap() error { return e.Err }

// CompleteLogin is the 2FA step of sign-in.
//
//  1. It counts one try against the pending row (an atomic decrement, before the
//     code is looked at); no pending row, a used-up or expired one is
//     ErrPendingInvalid. The user must still be an admin (password and confirmed
//     TOTP): after `admin reset` a pending row from before cannot complete.
//  2. It opens the user's sealed seed and verifies the code (TOTP within +-1
//     step, or an unused recovery code).
//  3. In one transaction it deletes the pending row (single use: exactly one row),
//     records the TOTP counter (a reused code is ErrCodeReused) or consumes the
//     recovery code, revokes the old session if one was presented, creates a NEW
//     session id (the client never chooses it), remembers the IP as known and
//     moves the failed-second-factor notice into the session.
//
// A wrong code is a *LoginError wrapping ErrBadCode: the failure is counted for
// the notice, and the pending row is deleted when no tries are left.
func (s *Store) CompleteLogin(ctx context.Context, in LoginInput) (LoginResult, error) {
	var out LoginResult
	if (in.Code == "") == (in.RecoveryCode == "") {
		return out, &LoginError{Err: ErrBadCode}
	}
	info, err := s.PendingAttempt(ctx, in.PendingSecret)
	if err != nil {
		return out, err
	}
	fail := func(err error) (LoginResult, error) {
		if errors.Is(err, ErrBadCode) {
			if ferr := s.RecordSecondFactorFailure(ctx, info.UserID, in.IPKey); ferr != nil {
				s.warnOncePerMinute("could not record a failed second factor", "err", ferr.Error())
			}
		}
		if ferr := s.PendingFailed(ctx, in.PendingSecret); ferr != nil {
			s.warnOncePerMinute("could not clear a used-up pending sign-in", "err", ferr.Error())
		}
		return LoginResult{}, &LoginError{Err: err, UserID: info.UserID, TriesLeft: info.TriesLeft}
	}
	var counter int64
	var norm string
	if in.Code != "" {
		seed, err := s.UserSeed(ctx, info.UserID)
		if err != nil {
			if errors.Is(err, ErrKeyMismatch) {
				return LoginResult{}, &LoginError{Err: err, UserID: info.UserID, TriesLeft: info.TriesLeft}
			}
			return fail(ErrPendingInvalid) // no seed: not an admin (after a reset)
		}
		c, ok := VerifyTOTP(seed, in.Code, s.now())
		if !ok {
			return fail(ErrBadCode)
		}
		counter = c
	} else {
		n, ok := normaliseRecovery(in.RecoveryCode)
		if !ok {
			return fail(ErrBadCode)
		}
		norm = n
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		now := s.now().Unix()
		var admin bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = ? AND password_hash IS NOT NULL AND totp_confirmed_at IS NOT NULL)`, info.UserID).Scan(&admin); err != nil {
			return err
		}
		if !admin {
			return ErrPendingInvalid
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM auth_pending WHERE id_hash = ? AND expires_at > ?`, hashSecret(in.PendingSecret), now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrPendingInvalid
		}
		if norm == "" {
			if err := acceptTOTP(ctx, tx, info.UserID, counter); err != nil {
				return err
			}
		} else if used, err := useRecovery(ctx, tx, info.UserID, norm, now); err != nil {
			return err
		} else if !used {
			return ErrBadCode
		}
		if in.OldSessionSecret != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, hashSecret(in.OldSessionSecret)); err != nil {
				return err
			}
		}
		var fails int
		var failAt sql.NullInt64
		var failIP sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT totp_fail_since_login, totp_fail_last_at, totp_fail_last_ip FROM users WHERE id = ?`, info.UserID).Scan(&fails, &failAt, &failIP); err != nil {
			return err
		}
		notice := Notice{}
		if fails > 0 {
			notice = Notice{TOTPFailures: fails, LastFailureAt: failAt.Int64, LastFailureIP: failIP.String}
			if _, err := tx.ExecContext(ctx, `UPDATE users SET totp_fail_since_login = 0, totp_fail_last_at = NULL, totp_fail_last_ip = NULL WHERE id = ?`, info.UserID); err != nil {
				return err
			}
		}
		nj, _ := json.Marshal(notice)
		secret, sess, err := s.createSession(ctx, tx, info.UserID, in.IPKey, in.UserAgent, string(nj), nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_known_ips (user_id, ip_key, last_ok_at) VALUES (?, ?, ?)
			ON CONFLICT(user_id, ip_key) DO UPDATE SET last_ok_at = excluded.last_ok_at`, info.UserID, in.IPKey, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_known_ips WHERE user_id = ? AND ip_key NOT IN
			(SELECT ip_key FROM auth_known_ips WHERE user_id = ? ORDER BY last_ok_at DESC, rowid DESC LIMIT ?)`, info.UserID, info.UserID, maxKnownIPs); err != nil {
			return err
		}
		out = LoginResult{SessionSecret: secret, Session: sess}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrBadCode) {
			return fail(err)
		}
		return LoginResult{}, &LoginError{Err: err, UserID: info.UserID, TriesLeft: info.TriesLeft}
	}
	return out, nil
}

// RecordSecondFactorFailure counts a wrong second factor after a correct
// password, for the notice shown at the next sign-in.
func (s *Store) RecordSecondFactorFailure(ctx context.Context, userID int64, ipKey string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE users SET totp_fail_since_login = totp_fail_since_login + 1, totp_fail_last_at = ?, totp_fail_last_ip = ? WHERE id = ?`,
			s.now().Unix(), ipKey, userID)
		return err
	})
}

// KnownIP reports whether ipKey completed a full sign-in for the user in the last
// 30 days (such an IP skips the per-account back-off and is admitted when the
// limiter maps are full).
func (s *Store) KnownIP(ctx context.Context, userID int64, ipKey string) (bool, error) {
	var ok bool
	err := s.st.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM auth_known_ips WHERE user_id = ? AND ip_key = ? AND last_ok_at > ?)`,
		userID, ipKey, s.now().Add(-knownIPLifetime).Unix()).Scan(&ok)
	return ok, err
}

// MaxReauthFailures is how many wrong step-up codes revoke a session.
const MaxReauthFailures = 5

// ErrSessionRevoked: the session was just revoked (too many wrong step-up codes).
var ErrSessionRevoked = errors.New("auth: session revoked after too many wrong codes")

// ReauthError wraps ErrBadCode with the failures left before the session is revoked.
type ReauthError struct {
	Err          error
	UserID       int64
	FailuresLeft int
}

func (e *ReauthError) Error() string { return e.Err.Error() }
func (e *ReauthError) Unwrap() error { return e.Err }

// Reauth is the step-up: it checks a TOTP code (never a recovery code) for the
// session's user, records the counter (a code is never reused) and moves the
// session's mfa_fresh_until to now + 5 minutes. A wrong code is counted on the
// session and for the sign-in notice; the 5th wrong code deletes the session
// (ErrSessionRevoked) and the caller clears the cookie.
func (s *Store) Reauth(ctx context.Context, sessionSecret, code string) (time.Time, error) {
	idHash := hashSecret(sessionSecret)
	var userID int64
	var failures int
	if err := s.st.R.QueryRowContext(ctx, `SELECT user_id, reauth_failures FROM sessions WHERE id_hash = ?`, idHash).Scan(&userID, &failures); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, &SessionError{Reason: "revoked"}
		}
		return time.Time{}, err
	}
	seed, err := s.UserSeed(ctx, userID)
	if err != nil {
		return time.Time{}, err
	}
	counter, ok := VerifyTOTP(seed, code, s.now())
	if !ok {
		var revoked bool
		var left int
		err := s.write(ctx, func(tx *sql.Tx) error {
			now := s.now().Unix()
			res, err := tx.ExecContext(ctx, `UPDATE sessions SET reauth_failures = reauth_failures + 1 WHERE id_hash = ?`, idHash)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return &SessionError{Reason: "revoked"}
			}
			var f int
			if err := tx.QueryRowContext(ctx, `SELECT reauth_failures FROM sessions WHERE id_hash = ?`, idHash).Scan(&f); err != nil {
				return err
			}
			left = MaxReauthFailures - f
			if _, err := tx.ExecContext(ctx, `UPDATE users SET totp_fail_since_login = totp_fail_since_login + 1, totp_fail_last_at = ?, totp_fail_last_ip = (SELECT ip_key FROM sessions WHERE id_hash = ?) WHERE id = ?`, now, idHash, userID); err != nil {
				return err
			}
			if f >= MaxReauthFailures {
				revoked = true
				_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
			}
			return err
		})
		if err != nil {
			return time.Time{}, err
		}
		if revoked {
			return time.Time{}, &ReauthError{Err: ErrSessionRevoked, UserID: userID}
		}
		return time.Time{}, &ReauthError{Err: ErrBadCode, UserID: userID, FailuresLeft: left}
	}
	var fresh time.Time
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := acceptTOTP(ctx, tx, userID, counter); err != nil {
			return err
		}
		fresh = s.now().Add(MFAFresh)
		res, err := tx.ExecContext(ctx, `UPDATE sessions SET mfa_fresh_until = ?, reauth_failures = 0 WHERE id_hash = ?`, fresh.Unix(), idHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return &SessionError{Reason: "revoked"}
		}
		return nil
	})
	if errors.Is(err, ErrCodeReused) {
		return time.Time{}, &ReauthError{Err: err, UserID: userID, FailuresLeft: MaxReauthFailures - failures}
	}
	return fresh, err
}
