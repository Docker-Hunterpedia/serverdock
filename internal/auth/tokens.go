package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// API tokens (docs/plans/0003, "API tokens"): `sd_` + 43 base64url characters (256
// bits), stored as SHA-256 with a displayable prefix, scoped read < write < admin,
// with a REQUIRED expiry (default 90 days, at most 365). They are accepted only
// from the Authorization header (the api package never reads a cookie or a query
// string for them), and the action registry denies them every sensitive,
// destructive, session-only and console-only action.
const (
	TokenPrefix       = "sd_"
	tokenSecretLen    = 43
	prefixLen         = 8
	DefaultTokenDays  = 90
	MaxTokenDays      = 365
	maxTokenNameBytes = 64
)

// Errors of the token paths.
var (
	// ErrBadAPIToken: unknown, malformed or revoked (the caller cannot tell which).
	ErrBadAPIToken = errors.New("auth: invalid API token")
	// ErrAPITokenExpired: a known token past its expiry.
	ErrAPITokenExpired = errors.New("auth: API token expired")
	// ErrNoSuchToken: revoke of a token id that is not the user's.
	ErrNoSuchToken = errors.New("auth: no such API token")
	// ErrInvalidToken: bad name, scope or lifetime for a new token.
	ErrInvalidToken = errors.New("auth: invalid token request")
)

// APIToken is a token's public data (never its hash or its secret).
type APIToken struct {
	ID         int64
	Name       string
	Prefix     string
	Scope      string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt time.Time // zero when never used
	Revoked    bool
}

// TokenCheck is what a verified token grants.
type TokenCheck struct {
	ID     int64
	UserID int64
	Scope  string
}

func validTokenScope(s string) bool { return s == "read" || s == "write" || s == "admin" }

// ValidateTokenRequest checks the inputs of a new token and returns the lifetime
// in days (0 means the default).
func ValidateTokenRequest(name, scope string, days int) (int, error) {
	bad := func(msg string) (int, error) { return 0, errors.Join(ErrInvalidToken, errors.New(msg)) }
	if name == "" || len(name) > maxTokenNameBytes || !utf8.ValidString(name) {
		return bad("a token name has 1 to 64 characters")
	}
	for _, r := range name {
		if r < ' ' || r == 0x7f {
			return bad("a token name has no control characters")
		}
	}
	if !validTokenScope(scope) {
		return bad("the scope is read, write or admin")
	}
	if days < 0 || days > MaxTokenDays {
		return bad(fmt.Sprintf("a token lasts 1 to %d days (default %d)", MaxTokenDays, DefaultTokenDays))
	}
	if days == 0 {
		days = DefaultTokenDays
	}
	return days, nil
}

// CreateToken makes a token for the user and returns its secret ONCE. Only the
// hash is stored.
func (s *Store) CreateToken(ctx context.Context, userID int64, name, scope string, days int) (string, APIToken, error) {
	days, err := ValidateTokenRequest(name, scope, days)
	if err != nil {
		return "", APIToken{}, err
	}
	raw, _, err := newSecret() // 43 base64url characters
	if err != nil {
		return "", APIToken{}, err
	}
	secret := TokenPrefix + raw
	now := s.now()
	t := APIToken{Name: name, Prefix: raw[:prefixLen], Scope: scope, CreatedAt: now, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}
	err = s.write(ctx, func(tx *sql.Tx) error {
		// Lazy cleanup, no timer: drop up to 100 tokens that were revoked or have
		// been expired for more than 30 days.
		cutoff := now.Add(-30 * 24 * time.Hour).Unix()
		if _, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE id IN (SELECT id FROM api_tokens WHERE (revoked_at IS NOT NULL AND revoked_at < ?) OR expires_at < ? LIMIT 100)`, cutoff, cutoff); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO api_tokens (user_id, name, prefix, hash, scope, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			userID, name, t.Prefix, hashSecret(secret), scope, now.Unix(), t.ExpiresAt.Unix())
		if err != nil {
			return err
		}
		t.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return "", APIToken{}, err
	}
	return secret, t, nil
}

// VerifyToken checks a bearer token: well-formed, known, not revoked, not expired,
// and its user is still an admin. last_used_at is written at most once a minute,
// best-effort and bounded (a busy writer never stalls a request).
func (s *Store) VerifyToken(ctx context.Context, token string) (TokenCheck, error) {
	var c TokenCheck
	if len(token) != len(TokenPrefix)+tokenSecretLen || token[:len(TokenPrefix)] != TokenPrefix {
		return c, ErrBadAPIToken
	}
	h := hashSecret(token)
	var (
		expires           int64
		lastUsed, revoked sql.NullInt64
	)
	err := s.st.R.QueryRowContext(ctx, `SELECT t.id, t.user_id, t.scope, t.expires_at, t.last_used_at, t.revoked_at FROM api_tokens t
		WHERE t.hash = ? AND t.user_id IN (SELECT id FROM users WHERE password_hash IS NOT NULL AND totp_confirmed_at IS NOT NULL)`, h).
		Scan(&c.ID, &c.UserID, &c.Scope, &expires, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenCheck{}, ErrBadAPIToken
	}
	if err != nil {
		return TokenCheck{}, err
	}
	if revoked.Valid {
		return TokenCheck{}, ErrBadAPIToken
	}
	now := s.now()
	if now.Unix() >= expires {
		return TokenCheck{}, ErrAPITokenExpired
	}
	if !lastUsed.Valid || now.Unix()-lastUsed.Int64 >= int64(touchEvery/time.Second) {
		tctx, cancel := context.WithTimeout(ctx, bestEffortTimeout)
		defer cancel()
		if err := s.touchTx(tctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(tctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now.Unix(), c.ID)
			return err
		}); err != nil {
			s.warnOncePerMinute("could not record API token use; continuing", "err", err.Error())
		}
	}
	return c, nil
}

// ListTokens returns the user's tokens, newest first. Never a hash.
func (s *Store) ListTokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.st.R.QueryContext(ctx, `SELECT id, name, prefix, scope, created_at, expires_at, last_used_at, revoked_at FROM api_tokens WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var created, expires int64
		var last, rev sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.Scope, &created, &expires, &last, &rev); err != nil {
			return nil, err
		}
		t.CreatedAt, t.ExpiresAt, t.Revoked = time.Unix(created, 0), time.Unix(expires, 0), rev.Valid
		if last.Valid {
			t.LastUsedAt = time.Unix(last.Int64, 0)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken revokes one of the user's tokens (revoking twice is not an error).
func (s *Store) RevokeToken(ctx context.Context, userID, id int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNoSuchToken
		}
		_, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, s.now().Unix(), id, userID)
		return err
	})
}
