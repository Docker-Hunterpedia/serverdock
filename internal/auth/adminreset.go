package auth

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
)

// ResetResult says what `serverdock admin reset` changed (counts only, never a secret).
type ResetResult struct {
	SessionsDeleted int64 `json:"sessions_deleted"`
	PendingDeleted  int64 `json:"pending_deleted"`
	TokensRevoked   int64 `json:"tokens_revoked"`
	KeyCreated      bool  `json:"key_created"`
}

// AdminResetSystemReason is the written reason the console recovery action may run
// as the system actor (and why it is listed under "Unattended destructive actions").
const AdminResetSystemReason = "console recovery: the operator ran `serverdock admin reset` as the data owner, with the server stopped"

// RegisterAdminReset registers auth.admin_reset. It must be called ONLY by the
// `admin reset` process: the server's registry never has it (a test checks both
// the registry and the source of the server command), so no HTTP request, token
// or MCP client can reach it.
func RegisterAdminReset(r *actions.Registry, s *Store) {
	actions.Register(r, actions.Def[actions.None, ResetResult]{
		Name: "auth.admin_reset", Summary: "Clear the admin password, two-factor and recovery codes, end every session and revoke every API token.",
		Risk: actions.RiskDestructive, Scope: actions.ScopeAdmin, ConsoleOnly: true, System: AdminResetSystemReason,
		Target: func(actions.None) string { return "admin" },
		Execute: func(ctx context.Context, _ actions.Call, _ actions.None) (ResetResult, error) {
			return s.AdminReset(ctx)
		},
	})
}

// AdminReset clears, in ONE transaction: the user's password, TOTP seed, confirmed
// time, pending enrolment, last accepted counter and failure counters (the row and
// its id stay); every recovery code, session, pending sign-in and known IP; the
// setup state (so a pending seed is gone and the next start writes a new token);
// every API token (revoked, not deleted); and meta.secretbox_key_id, which belongs
// with the ciphertexts it describes. Afterwards it creates the master key if the
// file is absent (the checks that refuse a start without it now pass). It keeps the
// panel leaf, the hub CA, the agent token and an existing key.
func (s *Store) AdminReset(ctx context.Context) (ResetResult, error) {
	var res ResetResult
	err := s.write(ctx, func(tx *sql.Tx) error {
		now := s.now().Unix()
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = NULL, totp_seed = NULL, totp_confirmed_at = NULL,
			totp_pending_seed = NULL, totp_pending_expires_at = NULL, totp_pending_enrollment_hash = NULL, totp_pending_tries = 0,
			totp_last_counter = 0, totp_fail_since_login = 0, totp_fail_last_at = NULL, totp_fail_last_ip = NULL`); err != nil {
			return err
		}
		del := func(table string, out *int64) error {
			r, err := tx.ExecContext(ctx, `DELETE FROM `+table)
			if err != nil {
				return err
			}
			if out != nil {
				*out, _ = r.RowsAffected()
			}
			return nil
		}
		for _, t := range []struct {
			name string
			out  *int64
		}{{"recovery_codes", nil}, {"sessions", &res.SessionsDeleted}, {"auth_pending", &res.PendingDeleted}, {"auth_known_ips", nil}, {"setup_state", nil}} {
			if err := del(t.name, t.out); err != nil {
				return err
			}
		}
		r, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE revoked_at IS NULL`, now)
		if err != nil {
			return err
		}
		res.TokensRevoked, _ = r.RowsAffected()
		_, err = tx.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, secretbox.KeyIDMetaKey)
		return err
	})
	if err != nil {
		return res, err
	}
	_, created, err := secretbox.LoadOrCreate(ctx, s.st.Dir, secretbox.SQLMeta{R: s.st.R, W: s.st.W}, func() (bool, error) {
		return secretbox.CiphertextsExist(ctx, s.st.R, secretbox.Columns())
	})
	if err != nil {
		return res, fmt.Errorf("the encrypted admin data was cleared, but the master key could not be set up: %w", err)
	}
	res.KeyCreated = created
	return res, nil
}
