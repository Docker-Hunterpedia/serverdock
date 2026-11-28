package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// ResetArgs are the input of auth.admin_reset.
type ResetArgs struct {
	// ForgetAppSecrets allows the reset to delete app environment variables and
	// registry credentials whose master key is lost. It does nothing when the key
	// is present and matches: app secrets are then never touched.
	ForgetAppSecrets bool `json:"forget_app_secrets,omitempty"`
}

// Validate implements actions.Validator.
func (ResetArgs) Validate() error { return nil }

// ErrAppSecretsAtRisk is returned when app ciphertexts exist and the master key is
// missing or does not match, and --forget-app-secrets was not given.
var ErrAppSecretsAtRisk = errors.New("app environment variables or registry credentials exist, but the master key is missing or is not the one they were sealed with; a reset would orphan them. Restore the key from a backup, or run `serverdock admin reset --forget-app-secrets` to list and delete them")

// ErrKeyUnusable is returned when app ciphertexts exist and the key file is
// damaged, the wrong size or too open: nothing is changed.
var ErrKeyUnusable = errors.New("app environment variables or registry credentials exist and the master key file cannot be used (damaged, wrong size or readable by others); fix it or move it aside, then run the reset again")

// LostService is one service whose environment versions the master key protected.
type LostService struct {
	Project  string `json:"project"`
	Service  string `json:"service"`
	Versions int    `json:"versions"`
}

// AppSecretsReport says whether app ciphertexts exist, whether the key on disk can
// open them, and what would be lost. It holds names and counts only.
type AppSecretsReport struct {
	Exist      bool
	Key        secretbox.KeyState
	Services   []LostService
	Registries []string // hosts that hold a credential
}

// AtRisk reports whether app ciphertexts exist and the key cannot be trusted with them.
func (r AppSecretsReport) AtRisk() bool { return r.Exist && r.Key != secretbox.KeyOK }

// AppSecrets inspects the app ciphertexts and the key. It changes nothing.
func (s *Store) AppSecrets(ctx context.Context) (AppSecretsReport, error) {
	var rep AppSecretsReport
	var err error
	if rep.Exist, err = secretbox.CiphertextsExist(ctx, s.st.R, secretbox.AppColumns()); err != nil {
		return rep, err
	}
	if rep.Key, err = secretbox.CheckKey(ctx, s.st.Dir, secretbox.SQLMeta{R: s.st.R, W: s.st.W}); err != nil {
		return rep, err
	}
	if !rep.Exist {
		return rep, nil
	}
	rows, err := s.st.R.QueryContext(ctx, `SELECT p.name, s.name, count(*) FROM service_env_versions v
		JOIN services s ON s.id = v.service_id JOIN projects p ON p.id = s.project_id GROUP BY s.id ORDER BY p.name, s.name`)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var l LostService
		if err := rows.Scan(&l.Project, &l.Service, &l.Versions); err != nil {
			rows.Close()
			return rep, err
		}
		rep.Services = append(rep.Services, l)
	}
	if err := rows.Close(); err != nil {
		return rep, err
	}
	hr, err := s.st.R.QueryContext(ctx, `SELECT host FROM registries WHERE secret IS NOT NULL ORDER BY host`)
	if err != nil {
		return rep, err
	}
	defer hr.Close()
	for hr.Next() {
		var h string
		if err := hr.Scan(&h); err != nil {
			return rep, err
		}
		rep.Registries = append(rep.Registries, h)
	}
	return rep, hr.Err()
}

// ResetResult says what `serverdock admin reset` changed (counts only, never a secret).
type ResetResult struct {
	SessionsDeleted int64 `json:"sessions_deleted"`
	PendingDeleted  int64 `json:"pending_deleted"`
	TokensRevoked   int64 `json:"tokens_revoked"`
	KeyCreated      bool  `json:"key_created"`
	// AppSecretsKept: app ciphertexts exist and the key matched, so they were not touched.
	AppSecretsKept bool `json:"app_secrets_kept,omitempty"`
	// The next three are set only by --forget-app-secrets with a lost key.
	EnvVersionsDeleted int64 `json:"env_versions_deleted,omitempty"`
	ServicesFlagged    int64 `json:"services_flagged,omitempty"`
	RegistriesCleared  int64 `json:"registries_cleared,omitempty"`
}

// AdminResetSystemReason is the written reason the console recovery action may run
// as the system actor (and why it is listed under "Unattended destructive actions").
const AdminResetSystemReason = "console recovery: the operator ran `serverdock admin reset` as the data owner, with the server stopped"

// RegisterAdminReset registers auth.admin_reset. It must be called ONLY by the
// `admin reset` process: the server's registry never has it (a test checks both
// the registry and the source of the server command), so no HTTP request, token
// or MCP client can reach it.
func RegisterAdminReset(r *actions.Registry, s *Store) {
	actions.Register(r, actions.Def[ResetArgs, ResetResult]{
		Name: "auth.admin_reset", Summary: "Clear the admin password, two-factor and recovery codes, end every session and revoke every API token.",
		Risk: actions.RiskDestructive, Scope: actions.ScopeAdmin, ConsoleOnly: true, System: AdminResetSystemReason,
		Target: func(ResetArgs) string { return "admin" },
		Execute: func(ctx context.Context, _ actions.Call, in ResetArgs) (ResetResult, error) {
			return s.AdminReset(ctx, in.ForgetAppSecrets)
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
//
// App secrets (env versions, registry credentials): with the right key present
// they are never touched. With app ciphertexts and a missing or wrong key the reset
// is refused (ErrAppSecretsAtRisk), unless forgetApp is set; then, in the same
// transaction, every service loses its env version pointer and is flagged
// needs_attention = env_lost, the env versions are deleted and every registry
// loses its username and secret. A key file that is unusable is refused either way.
func (s *Store) AdminReset(ctx context.Context, forgetApp bool) (ResetResult, error) {
	var res ResetResult
	rep, err := s.AppSecrets(ctx)
	if err != nil {
		return res, err
	}
	forget := false
	switch {
	case rep.Exist && rep.Key == secretbox.KeyUnusable:
		return res, ErrKeyUnusable
	case rep.AtRisk() && !forgetApp:
		return res, ErrAppSecretsAtRisk
	case rep.AtRisk():
		forget = true
	case rep.Exist:
		res.AppSecretsKept = true
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
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
		if forget {
			// Pointers first: services.env_version_id is ON DELETE RESTRICT.
			r, err := tx.ExecContext(ctx, `UPDATE services SET env_version_id = NULL, needs_attention = CASE
				WHEN needs_attention IN ('delete_interrupted', 'delete_confirm_again') THEN needs_attention ELSE ? END
				WHERE env_version_id IS NOT NULL OR id IN (SELECT service_id FROM service_env_versions)`, store.AttentionEnvLost)
			if err != nil {
				return err
			}
			res.ServicesFlagged, _ = r.RowsAffected()
			if r, err = tx.ExecContext(ctx, `DELETE FROM service_env_versions`); err != nil {
				return err
			}
			res.EnvVersionsDeleted, _ = r.RowsAffected()
			if r, err = tx.ExecContext(ctx, `UPDATE registries SET username = NULL, secret = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE secret IS NOT NULL`); err != nil {
				return err
			}
			res.RegistriesCleared, _ = r.RowsAffected()
		}
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
