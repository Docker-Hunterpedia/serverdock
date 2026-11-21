package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// ReenrollTTL is how long a pending re-enrolment lasts; ReenrollTries how many
// confirm attempts one enrolment allows.
const (
	ReenrollTTL   = 10 * time.Minute
	ReenrollTries = 5
)

// revokeAllTx deletes the user's sessions and pending sign-ins and, unless
// keepTokens, revokes their API tokens. Password and TOTP changes call it in the
// same transaction as the change.
func (s *Store) revokeAllTx(ctx context.Context, tx *sql.Tx, userID int64, keepTokens bool) (sessions, tokens int64, err error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, 0, err
	}
	sessions, _ = res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_pending WHERE user_id = ?`, userID); err != nil {
		return 0, 0, err
	}
	if !keepTokens {
		res, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, s.now().Unix(), userID)
		if err != nil {
			return 0, 0, err
		}
		tokens, _ = res.RowsAffected()
	}
	return sessions, tokens, nil
}

// PasswordHash returns the user's stored PHC string ("" when there is none).
func (s *Store) PasswordHash(ctx context.Context, userID int64) (string, error) {
	var h sql.NullString
	err := s.st.R.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&h)
	return h.String, err
}

// ChangePassword stores a new password hash and, in the same transaction, revokes
// every session (the current one too) and pending sign-in and, unless keepTokens,
// every API token.
func (s *Store) ChangePassword(ctx context.Context, userID int64, newPHC string, keepTokens bool) (sessions, tokens int64, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, password_changed_at = ? WHERE id = ? AND password_hash IS NOT NULL`, newPHC, s.now().Unix(), userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrPendingInvalid
		}
		sessions, tokens, err = s.revokeAllTx(ctx, tx, userID, keepTokens)
		return err
	})
	return
}

// ReenrollBegin creates a pending TOTP seed for the signed-in user (sealed, AAD
// users/<id>/totp_pending_seed, valid 10 minutes, 5 confirm tries), replacing an
// earlier pending enrolment. The current seed keeps working until confirmed.
func (s *Store) ReenrollBegin(ctx context.Context, userID int64, label string) (EnrollResult, error) {
	seed, err := NewTOTPSeed()
	if err != nil {
		return EnrollResult{}, err
	}
	ct, err := s.box.Seal(fmt.Sprintf("users/%d/totp_pending_seed", userID), seed)
	if err != nil {
		return EnrollResult{}, err
	}
	var idRaw [16]byte
	if _, err := rand.Read(idRaw[:]); err != nil {
		return EnrollResult{}, err
	}
	id := base64.RawURLEncoding.EncodeToString(idRaw[:])
	err = s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET totp_pending_seed = ?, totp_pending_expires_at = ?, totp_pending_enrollment_hash = ?, totp_pending_tries = ? WHERE id = ?`,
			ct, s.now().Add(ReenrollTTL).Unix(), hashSecret(id), ReenrollTries, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrPendingInvalid
		}
		return nil
	})
	if err != nil {
		return EnrollResult{}, err
	}
	return EnrollResult{EnrollmentID: id, Secret: TOTPSecret(seed), URI: TOTPURI(seed, label)}, nil
}

// ReenrollError says how many confirm tries remain after a wrong code.
type ReenrollError struct {
	Err       error
	TriesLeft int
}

func (e *ReenrollError) Error() string { return e.Err.Error() }
func (e *ReenrollError) Unwrap() error { return e.Err }

// ReenrollConfirm completes a re-enrolment: it counts one try (atomically, before
// the code is looked at; the 5th wrong code discards the enrolment), checks the
// code against the pending seed and then, in one transaction, swaps the seed in
// (sealed under users/<id>/totp_seed), records the code's counter, records how the
// panel was reached, and revokes every session and, unless keepTokens, every API
// token. The caller clears the session cookie.
func (s *Store) ReenrollConfirm(ctx context.Context, userID int64, enrollmentID, code, hostKind string, keepTokens bool) (sessions, tokens int64, err error) {
	switch hostKind {
	case "ip", "localhost", "name":
	default:
		hostKind = "" // unknown (no Host on the request): record nothing
	}
	if len(enrollmentID) > maxTokenInput {
		return 0, 0, ErrEnrollmentReplaced
	}
	now := s.now().Unix()
	var pending, enrollHash []byte
	var tries int
	err = s.write(ctx, func(tx *sql.Tx) error {
		var expires sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT totp_pending_seed, totp_pending_enrollment_hash, totp_pending_expires_at, totp_pending_tries FROM users WHERE id = ?`, userID).
			Scan(&pending, &enrollHash, &expires, &tries); err != nil {
			return err
		}
		if pending == nil || enrollHash == nil || subtle.ConstantTimeCompare(enrollHash, hashSecret(enrollmentID)) != 1 {
			return ErrEnrollmentReplaced
		}
		if !expires.Valid || now >= expires.Int64 || tries <= 0 {
			return ErrPendingInvalid
		}
		// One try, counted before the code is checked.
		res, err := tx.ExecContext(ctx, `UPDATE users SET totp_pending_tries = totp_pending_tries - 1 WHERE id = ? AND totp_pending_tries > 0 AND totp_pending_enrollment_hash = ?`, userID, enrollHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrPendingInvalid
		}
		tries--
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	discard := func() {
		if err := s.write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE users SET totp_pending_seed = NULL, totp_pending_expires_at = NULL, totp_pending_enrollment_hash = NULL, totp_pending_tries = 0 WHERE id = ? AND totp_pending_enrollment_hash = ?`, userID, enrollHash)
			return err
		}); err != nil {
			s.warnOncePerMinute("could not discard a used-up re-enrolment", "err", err.Error())
		}
	}
	seed, err := s.openSeed("users", "totp_pending_seed", fmt.Sprintf("users/%d/totp_pending_seed", userID), pending)
	if err != nil {
		return 0, 0, err
	}
	counter, ok := VerifyTOTP(seed, code, s.now())
	if !ok {
		if tries <= 0 {
			discard()
		}
		// A typo here is not a second-factor failure for the sign-in notice: the user
		// is signed in already, and the 5 tries bound the guessing.
		return 0, 0, &ReenrollError{Err: ErrBadCode, TriesLeft: tries}
	}
	sealed, err := s.box.Seal(fmt.Sprintf("users/%d/totp_seed", userID), seed)
	if err != nil {
		return 0, 0, err
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET totp_seed = ?, totp_last_counter = ?, totp_enrolled_host = COALESCE(NULLIF(?, ''), totp_enrolled_host), totp_confirmed_at = ?,
			totp_pending_seed = NULL, totp_pending_expires_at = NULL, totp_pending_enrollment_hash = NULL, totp_pending_tries = 0
			WHERE id = ? AND totp_pending_enrollment_hash = ? AND password_hash IS NOT NULL`, sealed, counter, hostKind, now, userID, enrollHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrEnrollmentReplaced // replaced or discarded while we checked
		}
		sessions, tokens, err = s.revokeAllTx(ctx, tx, userID, keepTokens)
		return err
	})
	return
}

// RegenerateRecoveryCodes replaces all ten recovery codes and returns the new ones
// (shown once).
func (s *Store) RegenerateRecoveryCodes(ctx context.Context, userID int64) ([]string, error) {
	codes, hashes, err := NewRecoveryCodes()
	if err != nil {
		return nil, err
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
			return err
		}
		for _, h := range hashes {
			if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes (user_id, hash) VALUES (?, ?)`, userID, h); err != nil {
				return err
			}
		}
		return nil
	})
	return codes, err
}

// ---- sessions of one user -----------------------------------------------------

// SessionByPublicID returns the user's session with that public id.
func (s *Store) SessionByPublicID(ctx context.Context, userID int64, publicID string) (*Session, error) {
	list, err := s.ListSessions(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].PublicID == publicID {
			return &list[i], nil
		}
	}
	return nil, &SessionError{Reason: "revoked"}
}

// ListSessions returns the user's live sessions, most recently seen first.
func (s *Store) ListSessions(ctx context.Context, userID int64) ([]Session, error) {
	rows, err := s.st.R.QueryContext(ctx, `SELECT public_id, user_id, created_at, last_seen_at, idle_expires_at, abs_expires_at, mfa_fresh_until, reauth_failures, ip_key, user_agent
		FROM sessions WHERE user_id = ? AND idle_expires_at > ? AND abs_expires_at > ? ORDER BY last_seen_at DESC, rowid DESC`, userID, s.now().Unix(), s.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var x Session
		var created, last, idle, abs, fresh int64
		if err := rows.Scan(&x.PublicID, &x.UserID, &created, &last, &idle, &abs, &fresh, &x.ReauthFailures, &x.IPKey, &x.UserAgent); err != nil {
			return nil, err
		}
		x.CreatedAt, x.LastSeenAt, x.IdleExpiresAt, x.AbsExpiresAt, x.MFAFreshUntil = time.Unix(created, 0), time.Unix(last, 0), time.Unix(idle, 0), time.Unix(abs, 0), time.Unix(fresh, 0)
		out = append(out, x)
	}
	return out, rows.Err()
}

// ErrNoSuchSession: revoke of a public id that is not the user's.
var ErrNoSuchSession = errors.New("auth: no such session")

// RevokeSessionByPublicID deletes one of the user's sessions.
func (s *Store) RevokeSessionByPublicID(ctx context.Context, userID int64, publicID string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND public_id = ?`, userID, publicID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNoSuchSession
		}
		return nil
	})
}

// RevokeOtherSessions deletes every session of the user except the one with
// keepPublicID, and returns how many.
func (s *Store) RevokeOtherSessions(ctx context.Context, userID int64, keepPublicID string) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND public_id != ?`, userID, keepPublicID)
		if err == nil {
			n, _ = res.RowsAffected()
		}
		return err
	})
	return n, err
}
