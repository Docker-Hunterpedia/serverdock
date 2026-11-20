package auth

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
)

// ActionDeps are what the auth actions need beyond the store.
type ActionDeps struct {
	// Hasher hashes and verifies passwords (the process-wide *Hasher).
	Hasher interface {
		Hash(ctx context.Context, password string) (string, error)
		Verify(ctx context.Context, phc, password string) (bool, error)
	}
	// PasswordGate is the account-password back-off (the same instance the sign-in
	// uses), so a stolen session cannot guess the current password faster than a login.
	PasswordGate interface {
		Check(key string, bypassFull bool) time.Duration
		Fail(key string, bypassFull bool) (time.Duration, bool)
		Reset(key string)
	}
}

// Errors of the auth actions.
var (
	ErrWrongPassword = errors.New("the current password is wrong")
	ErrRateLimited   = errors.New("too many attempts; try again later")
)

type none = actions.None

func userID(c actions.Call) (int64, error) {
	if c.Actor.Kind() != actions.KindUser {
		return 0, errors.New("a signed-in user is required")
	}
	return strconv.ParseInt(c.Actor.ID(), 10, 64)
}

// ---- inputs and outputs ----------------------------------------------------------

// TokenCreateIn is the input of tokens.create.
type TokenCreateIn struct {
	Name          string `json:"name" doc:"what the token is for" sd:"audit"`
	Scope         string `json:"scope" doc:"read, write or admin" sd:"audit"`
	ExpiresInDays int    `json:"expires_in_days,omitempty" doc:"1 to 365; default 90"`
}

// Validate implements actions.Validator.
func (i TokenCreateIn) Validate() error {
	_, err := ValidateTokenRequest(i.Name, i.Scope, i.ExpiresInDays)
	return err
}

// TokenCreateOut is returned once: the plaintext token is not retrievable again.
type TokenCreateOut struct {
	ID        int64     `json:"id"`
	Token     string    `json:"token" doc:"shown once"`
	Prefix    string    `json:"prefix"`
	Scope     string    `json:"scope"`
	ExpiresAt time.Time `json:"expires_at"`
}

// TokenInfo is a listed token (never its hash).
type TokenInfo struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Revoked    bool       `json:"revoked"`
}

// TokenListOut is the output of tokens.list.
type TokenListOut struct {
	Tokens []TokenInfo `json:"tokens"`
}

// TokenRevokeIn is the input of tokens.revoke.
type TokenRevokeIn struct {
	ID int64 `json:"id"`
}

// Validate implements actions.Validator.
func (i TokenRevokeIn) Validate() error {
	if i.ID <= 0 {
		return errors.New("id is required")
	}
	return nil
}

// RevokedOut is the output of the revoke actions.
type RevokedOut struct {
	Revoked int64 `json:"revoked"`
}

// PasswordChangeIn is the input of auth.password_change.
type PasswordChangeIn struct {
	CurrentPassword string `json:"current_password" sd:"secret"`
	NewPassword     string `json:"new_password" sd:"secret"`
	KeepTokens      bool   `json:"keep_tokens,omitempty" doc:"keep API tokens; tick this only if you are rotating routinely, not after a suspected compromise"`
}

// Validate implements actions.Validator.
func (i PasswordChangeIn) Validate() error {
	if i.CurrentPassword == "" || i.NewPassword == "" {
		return errors.New("current_password and new_password are required")
	}
	return nil
}

// ChangedOut says what a password or authenticator change revoked.
type ChangedOut struct {
	SessionsRevoked int64 `json:"sessions_revoked"`
	TokensRevoked   int64 `json:"tokens_revoked"`
}

// ReenrollBeginOut is the pending enrolment (shown once).
type ReenrollBeginOut struct {
	EnrollmentID string `json:"enrollment_id"`
	Secret       string `json:"secret"`
	OtpauthURI   string `json:"otpauth_uri"`
}

// ReenrollConfirmIn is the input of auth.totp_reenroll_confirm.
type ReenrollConfirmIn struct {
	EnrollmentID string `json:"enrollment_id" sd:"secret"`
	Code         string `json:"code" sd:"secret"`
	// KeepTokens is deliberately untagged, so it is recorded in the audit row.
	KeepTokens bool `json:"keep_tokens,omitempty" doc:"keep API tokens; tick this only if you are rotating routinely, not after a suspected compromise"`
}

// Validate implements actions.Validator.
func (i ReenrollConfirmIn) Validate() error {
	if i.EnrollmentID == "" || i.Code == "" {
		return errors.New("enrollment_id and code are required")
	}
	return nil
}

// RecoveryOut holds the new recovery codes (shown once).
type RecoveryOut struct {
	Codes []string `json:"codes"`
}

// SessionInfoOut is the output of auth.session_info.
type SessionInfoOut struct {
	Username          string    `json:"username"`
	PublicID          string    `json:"public_id"`
	MFAFreshUntil     time.Time `json:"mfa_fresh_until"`
	IdleExpiresAt     time.Time `json:"idle_expires_at"`
	AbsExpiresAt      time.Time `json:"abs_expires_at"`
	Notice            Notice    `json:"notice"`
	RecoveryCodesLeft int       `json:"recovery_codes_left"`
	// AdminCreatedAt and AdminCreatedFrom say when and from where first-run setup
	// was completed (docs/security.md G5). Zero and empty when unknown (a reset
	// admin that was created before this was recorded).
	AdminCreatedAt   time.Time `json:"admin_created_at"`
	AdminCreatedFrom string    `json:"admin_created_from"`
}

// SessionRow is a listed session.
type SessionRow struct {
	PublicID   string    `json:"public_id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	IPKey      string    `json:"ip_key"`
	UserAgent  string    `json:"user_agent"`
	Current    bool      `json:"current"`
}

// SessionsListOut is the output of auth.sessions_list.
type SessionsListOut struct {
	Sessions []SessionRow `json:"sessions"`
}

// SessionsRevokeIn is the input of auth.sessions_revoke: one session by public id,
// or every session but the current one.
type SessionsRevokeIn struct {
	PublicID  string `json:"public_id,omitempty"`
	AllOthers bool   `json:"all_others,omitempty"`
}

// Validate implements actions.Validator.
func (i SessionsRevokeIn) Validate() error {
	if (i.PublicID == "") == !i.AllOthers {
		return errors.New("give either public_id or all_others")
	}
	return nil
}

// RegisterAuthActions registers the token and account actions. Every one of them
// is SessionOnly: no API token, at any scope, can create, list or revoke tokens,
// change the password or the authenticator, or look at sessions. The ones that
// change a credential are Sensitive (fresh two-factor, audited intent first), and
// the ones that only remove access (tokens.revoke, auth.sessions_revoke) always
// run even when the audit log is down.
func RegisterAuthActions(r *actions.Registry, s *Store, d ActionDeps) {
	actions.Register(r, actions.Def[none, SessionInfoOut]{
		Name: "auth.session_info", Summary: "The signed-in user, the session's expiry, the step-up window, the sign-in notice and the recovery codes left.",
		Risk: actions.RiskRead, Scope: actions.ScopeRead, SessionOnly: true,
		Execute: func(ctx context.Context, c actions.Call, _ none) (SessionInfoOut, error) {
			uid, err := userID(c)
			if err != nil {
				return SessionInfoOut{}, err
			}
			sess, err := s.SessionByPublicID(ctx, uid, c.Actor.SessionID())
			if err != nil {
				return SessionInfoOut{}, err
			}
			name, _ := s.Username(ctx, uid)
			left, _ := s.RecoveryCodesLeft(ctx, uid)
			from, at, _ := s.AdminCreated(ctx, uid)
			return SessionInfoOut{Username: name, PublicID: sess.PublicID, MFAFreshUntil: sess.MFAFreshUntil, IdleExpiresAt: sess.IdleExpiresAt,
				AbsExpiresAt: sess.AbsExpiresAt, Notice: sess.Notice, RecoveryCodesLeft: left, AdminCreatedAt: at, AdminCreatedFrom: from}, nil
		},
	})

	actions.Register(r, actions.Def[PasswordChangeIn, ChangedOut]{
		Name: "auth.password_change", Summary: "Change the admin password. Ends every session and, unless keep_tokens, revokes every API token.",
		Risk: actions.RiskMutate, Scope: actions.ScopeAdmin, Sensitive: true, SessionOnly: true,
		Target: func(PasswordChangeIn) string { return "password" },
		Execute: func(ctx context.Context, c actions.Call, in PasswordChangeIn) (ChangedOut, error) {
			uid, err := userID(c)
			if err != nil {
				return ChangedOut{}, err
			}
			name, err := s.Username(ctx, uid)
			if err != nil {
				return ChangedOut{}, err
			}
			key := "user:" + strings.ToLower(name)
			if d.PasswordGate != nil {
				if wait := d.PasswordGate.Check(key, false); wait > 0 {
					return ChangedOut{}, ErrRateLimited
				}
			}
			phc, err := s.PasswordHash(ctx, uid)
			if err != nil || phc == "" {
				return ChangedOut{}, errors.New("no password is set")
			}
			ok, err := d.Hasher.Verify(ctx, phc, in.CurrentPassword)
			if err != nil {
				return ChangedOut{}, err
			}
			if !ok {
				if d.PasswordGate != nil {
					d.PasswordGate.Fail(key, false)
				}
				return ChangedOut{}, ErrWrongPassword
			}
			if err := CheckPassword(name, in.NewPassword); err != nil {
				return ChangedOut{}, err
			}
			newPHC, err := d.Hasher.Hash(ctx, in.NewPassword)
			if err != nil {
				return ChangedOut{}, err
			}
			sess, toks, err := s.ChangePassword(ctx, uid, newPHC, in.KeepTokens)
			if err != nil {
				return ChangedOut{}, err
			}
			if d.PasswordGate != nil {
				d.PasswordGate.Reset(key)
			}
			return ChangedOut{SessionsRevoked: sess, TokensRevoked: toks}, nil
		},
	})

	actions.Register(r, actions.Def[none, ReenrollBeginOut]{
		Name: "auth.totp_reenroll_begin", Summary: "Start replacing the authenticator: returns a new key and QR link (valid 10 minutes).",
		Risk: actions.RiskMutate, Scope: actions.ScopeAdmin, Sensitive: true, SessionOnly: true,
		Target: func(none) string { return "totp" },
		Execute: func(ctx context.Context, c actions.Call, _ none) (ReenrollBeginOut, error) {
			uid, err := userID(c)
			if err != nil {
				return ReenrollBeginOut{}, err
			}
			name, _ := s.Username(ctx, uid)
			res, err := s.ReenrollBegin(ctx, uid, name)
			return ReenrollBeginOut{EnrollmentID: res.EnrollmentID, Secret: res.Secret, OtpauthURI: res.URI}, err
		},
	})

	actions.Register(r, actions.Def[ReenrollConfirmIn, ChangedOut]{
		Name: "auth.totp_reenroll_confirm", Summary: "Finish replacing the authenticator with a code from the new one. Ends every session and, unless keep_tokens, revokes every API token.",
		Risk: actions.RiskMutate, Scope: actions.ScopeAdmin, Sensitive: true, SessionOnly: true,
		Target: func(ReenrollConfirmIn) string { return "totp" },
		Execute: func(ctx context.Context, c actions.Call, in ReenrollConfirmIn) (ChangedOut, error) {
			uid, err := userID(c)
			if err != nil {
				return ChangedOut{}, err
			}
			sess, toks, err := s.ReenrollConfirm(ctx, uid, in.EnrollmentID, in.Code, c.Actor.HostKind(), in.KeepTokens)
			return ChangedOut{SessionsRevoked: sess, TokensRevoked: toks}, err
		},
	})

	actions.Register(r, actions.Def[none, RecoveryOut]{
		Name: "auth.recovery_regenerate", Summary: "Replace all ten recovery codes; the new ones are returned once.",
		Risk: actions.RiskMutate, Scope: actions.ScopeAdmin, Sensitive: true, SessionOnly: true,
		Target: func(none) string { return "recovery_codes" },
		Execute: func(ctx context.Context, c actions.Call, _ none) (RecoveryOut, error) {
			uid, err := userID(c)
			if err != nil {
				return RecoveryOut{}, err
			}
			codes, err := s.RegenerateRecoveryCodes(ctx, uid)
			return RecoveryOut{Codes: codes}, err
		},
	})

	actions.Register(r, actions.Def[none, SessionsListOut]{
		Name: "auth.sessions_list", Summary: "The signed-in user's sessions.",
		Risk: actions.RiskRead, Scope: actions.ScopeAdmin, SessionOnly: true,
		Execute: func(ctx context.Context, c actions.Call, _ none) (SessionsListOut, error) {
			uid, err := userID(c)
			if err != nil {
				return SessionsListOut{}, err
			}
			list, err := s.ListSessions(ctx, uid)
			out := SessionsListOut{Sessions: []SessionRow{}}
			for _, x := range list {
				out.Sessions = append(out.Sessions, SessionRow{PublicID: x.PublicID, CreatedAt: x.CreatedAt, LastSeenAt: x.LastSeenAt,
					IPKey: x.IPKey, UserAgent: x.UserAgent, Current: x.PublicID == c.Actor.SessionID()})
			}
			return out, err
		},
	})

	actions.Register(r, actions.Def[SessionsRevokeIn, RevokedOut]{
		Name: "auth.sessions_revoke", Summary: "Sign out one session by its public id, or every session but this one.",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, SessionOnly: true, RemovesAccess: true,
		Target: func(i SessionsRevokeIn) string {
			if i.AllOthers {
				return "all_others"
			}
			return i.PublicID
		},
		Execute: func(ctx context.Context, c actions.Call, in SessionsRevokeIn) (RevokedOut, error) {
			uid, err := userID(c)
			if err != nil {
				return RevokedOut{}, err
			}
			if in.AllOthers {
				if c.Actor.SessionID() == "" {
					return RevokedOut{}, errors.New("all_others needs to know which session is the caller's")
				}
				n, err := s.RevokeOtherSessions(ctx, uid, c.Actor.SessionID())
				return RevokedOut{Revoked: n}, err
			}
			if err := s.RevokeSessionByPublicID(ctx, uid, in.PublicID); err != nil {
				return RevokedOut{}, err
			}
			return RevokedOut{Revoked: 1}, nil
		},
	})

	actions.Register(r, actions.Def[TokenCreateIn, TokenCreateOut]{
		Name: "tokens.create", Summary: "Create an API token. The token is shown once.",
		Risk: actions.RiskMutate, Scope: actions.ScopeAdmin, Sensitive: true, SessionOnly: true,
		Target: func(i TokenCreateIn) string { return i.Name },
		Execute: func(ctx context.Context, c actions.Call, in TokenCreateIn) (TokenCreateOut, error) {
			uid, err := userID(c)
			if err != nil {
				return TokenCreateOut{}, err
			}
			secret, t, err := s.CreateToken(ctx, uid, in.Name, in.Scope, in.ExpiresInDays)
			return TokenCreateOut{ID: t.ID, Token: secret, Prefix: t.Prefix, Scope: t.Scope, ExpiresAt: t.ExpiresAt}, err
		},
	})

	actions.Register(r, actions.Def[none, TokenListOut]{
		Name: "tokens.list", Summary: "The user's API tokens (never the token itself).",
		Risk: actions.RiskRead, Scope: actions.ScopeAdmin, SessionOnly: true,
		Execute: func(ctx context.Context, c actions.Call, _ none) (TokenListOut, error) {
			uid, err := userID(c)
			if err != nil {
				return TokenListOut{}, err
			}
			list, err := s.ListTokens(ctx, uid)
			out := TokenListOut{Tokens: []TokenInfo{}}
			for _, t := range list {
				ti := TokenInfo{ID: t.ID, Name: t.Name, Prefix: t.Prefix, Scope: t.Scope, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, Revoked: t.Revoked}
				if !t.LastUsedAt.IsZero() {
					lu := t.LastUsedAt
					ti.LastUsedAt = &lu
				}
				out.Tokens = append(out.Tokens, ti)
			}
			return out, err
		},
	})

	actions.Register(r, actions.Def[TokenRevokeIn, RevokedOut]{
		Name: "tokens.revoke", Summary: "Revoke an API token.",
		Risk: actions.RiskMutate, Scope: actions.ScopeAdmin, SessionOnly: true, RemovesAccess: true,
		Target: func(i TokenRevokeIn) string { return strconv.FormatInt(i.ID, 10) },
		Execute: func(ctx context.Context, c actions.Call, in TokenRevokeIn) (RevokedOut, error) {
			uid, err := userID(c)
			if err != nil {
				return RevokedOut{}, err
			}
			if err := s.RevokeToken(ctx, uid, in.ID); err != nil {
				return RevokedOut{}, err
			}
			return RevokedOut{Revoked: 1}, nil
		},
	})
}
