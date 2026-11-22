package auth

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
)

func scopeOf(s string) actions.Scope {
	switch s {
	case "read":
		return actions.ScopeRead
	case "write":
		return actions.ScopeWrite
	case "admin":
		return actions.ScopeAdmin
	}
	return 0
}

func minScope(a, b actions.Scope) actions.Scope {
	if a < b {
		return a
	}
	return b
}

func jobDenied(reason string) error { return &actions.DeniedError{Reason: reason} }

// RefreshJobActor re-checks, at run time, the actor that queued a job (rebuilt
// with SystemInvoker.ActorFromJob) against the database, and returns the actor to
// run it as. Every refusal is an *actions.DeniedError, so the job fails "denied"
// and nothing runs.
//
//   - Token: it must still exist, not be revoked or expired, and its user must
//     still be the admin. The scope is read from the database (never wider than
//     the one stored when the job was queued), so a narrowed scope takes effect.
//   - User: the user must still be the admin (a password and a confirmed
//     authenticator), and neither the password nor the authenticator may have
//     changed since the job was queued (a change, a re-enrolment or `admin reset`
//     stops work queued before it). Signing out or letting the session expire does
//     not: an explicit revoke cancels the session's jobs at the moment it happens
//     (auth.sessions_revoke). The fresh-2FA window is the one the job was queued
//     with, so a destructive job that waited past it is refused by the registry.
//   - Any other kind (system, MCP, webhook) cannot own a job.
func (s *Store) RefreshJobActor(ctx context.Context, a actions.Actor, enqueuedAt time.Time) (actions.Actor, error) {
	id, err := strconv.ParseInt(a.ID(), 10, 64)
	if err != nil || id <= 0 {
		return actions.Actor{}, jobDenied("the job's actor is not valid")
	}
	switch a.Kind() {
	case actions.KindToken:
		var (
			scope   string
			expires int64
			revoked sql.NullInt64
			admin   bool
		)
		err := s.st.R.QueryRowContext(ctx, `SELECT t.scope, t.expires_at, t.revoked_at,
			EXISTS (SELECT 1 FROM users u WHERE u.id = t.user_id AND u.password_hash IS NOT NULL AND u.totp_confirmed_at IS NOT NULL)
			FROM api_tokens t WHERE t.id = ?`, id).Scan(&scope, &expires, &revoked, &admin)
		if errors.Is(err, sql.ErrNoRows) {
			return actions.Actor{}, jobDenied("the API token that queued this job no longer exists")
		}
		if err != nil {
			return actions.Actor{}, err
		}
		if revoked.Valid || s.now().Unix() >= expires || !admin {
			return actions.Actor{}, jobDenied("the API token that queued this job was revoked or has expired")
		}
		sc := minScope(a.Scope(), scopeOf(scope))
		if sc == 0 {
			return actions.Actor{}, jobDenied("the API token's scope is not valid")
		}
		return actions.TokenActor(a.ID(), sc), nil
	case actions.KindUser:
		var (
			scope              string
			admin              bool
			passwordAt, totpAt int64
		)
		err := s.st.R.QueryRowContext(ctx, `SELECT scope, password_hash IS NOT NULL AND totp_confirmed_at IS NOT NULL,
			COALESCE(password_changed_at, 0), COALESCE(totp_confirmed_at, 0) FROM users WHERE id = ?`, id).Scan(&scope, &admin, &passwordAt, &totpAt)
		if errors.Is(err, sql.ErrNoRows) {
			return actions.Actor{}, jobDenied("the user that queued this job no longer exists")
		}
		if err != nil {
			return actions.Actor{}, err
		}
		if !admin {
			return actions.Actor{}, jobDenied("the user that queued this job is no longer an administrator")
		}
		// Fail closed on a tie: a change in the same second as the enqueue stops the job.
		if max(passwordAt, totpAt) >= enqueuedAt.Unix() {
			return actions.Actor{}, jobDenied("the password or authenticator changed after this job was queued")
		}
		sc := minScope(a.Scope(), scopeOf(scope))
		if sc == 0 {
			return actions.Actor{}, jobDenied("the user's scope is not valid")
		}
		return actions.UserSessionActor(a.ID(), a.SessionID(), sc, a.MFAFreshUntil()).WithHostKind(a.HostKind()), nil
	}
	return actions.Actor{}, jobDenied("this kind of caller cannot own a job")
}

// JobCanceller cancels the jobs that revoked sessions queued (the jobs scheduler).
type JobCanceller interface {
	CancelSessionJobs(ctx context.Context, sessions []string, reason string) (int, error)
}

// SessionRevokedJobReason is the cancel reason of a job whose session was revoked.
const SessionRevokedJobReason = "the session that queued this job was revoked"
