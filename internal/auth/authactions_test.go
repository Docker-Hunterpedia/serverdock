package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/limits"
)

type downSink struct {
	*audit.Logger
	down atomic.Bool
}

func (d *downSink) Append(c context.Context, e audit.Entry) (int64, error) {
	if d.down.Load() {
		return 0, errors.New("disk full")
	}
	return d.Logger.Append(c, e)
}

type aenv struct {
	*env
	reg    *actions.Registry
	sink   *downSink
	gate   *limits.Backoff
	uid    int64
	seed   []byte
	others map[string]string // public id -> secret of sessions the test made
	secret string            // the first session's cookie value
	public string            // ... and its public id
	codes  []string
}

func newAEnv(t *testing.T) *aenv {
	t.Helper()
	e := newEnv(t)
	token, er, seed := e.enrolled()
	res, err := e.s.SetupComplete(ctx, e.completion(token, er, seed))
	if err != nil {
		t.Fatal(err)
	}
	al, err := audit.New(e.st, slog.New(slog.NewTextHandler(e.logs, nil)), audit.WithClock(e.clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	a := &aenv{env: e, sink: &downSink{Logger: al}, gate: limits.NewBackoff(10, time.Second, 15*time.Minute, e.clk.Now),
		others: map[string]string{}, uid: res.UserID, seed: seed, secret: res.SessionSecret, public: res.Session.PublicID, codes: res.RecoveryCodes}
	a.reg = actions.New(a.sink, slog.New(slog.NewTextHandler(e.logs, nil)), actions.WithClock(e.clk.Now))
	RegisterAuthActions(a.reg, e.s, ActionDeps{Hasher: e.s.hasher, PasswordGate: a.gate})
	return a
}

func (a *aenv) user() actions.Actor { // the signed-in admin session, with the fresh-2FA window it has now
	sess, err := a.s.LookupSession(ctx, a.secret)
	if err != nil {
		a.t.Fatal(err)
	}
	return actions.UserSessionActor(strconv.FormatInt(a.uid, 10), a.public, actions.ScopeAdmin, sess.MFAFreshUntil)
}

func (a *aenv) inv(actor actions.Actor, name, in string) (actions.Result, error) {
	if in == "" {
		in = "{}"
	}
	return a.reg.Invoke(ctx, actor, name, json.RawMessage(in), actions.Options{})
}

func (a *aenv) mustInv(actor actions.Actor, name, in string) actions.Result {
	a.t.Helper()
	res, err := a.inv(actor, name, in)
	if err != nil {
		a.t.Fatalf("%s: %v", name, err)
	}
	return res
}

func (a *aenv) createToken(scope string, days int) TokenCreateOut {
	a.t.Helper()
	return a.mustInv(a.user(), "tokens.create", fmt.Sprintf(`{"name":"ci","scope":%q,"expires_in_days":%d}`, scope, days)).Out.(TokenCreateOut)
}

// ---- tokens ------------------------------------------------------------------------

func TestTokensCreateNeedsASessionWithFreshTwoFactor(t *testing.T) {
	a := newAEnv(t)
	in := `{"name":"ci","scope":"read"}`
	if _, err := a.inv(a.user(), "tokens.create", in); err != nil {
		t.Fatalf("fresh session: %v", err)
	}
	// After the 5 minute window: refused, nothing created.
	a.clk.Add(10 * time.Minute)
	sess, _ := a.s.LookupSession(ctx, a.secret)
	stale := actions.UserSessionActor(strconv.FormatInt(a.uid, 10), a.public, actions.ScopeAdmin, sess.MFAFreshUntil)
	before := a.count(`SELECT count(*) FROM api_tokens`)
	if _, err := a.inv(stale, "tokens.create", in); !errors.Is(err, actions.ErrDenied) || !strings.Contains(err.Error(), "two-factor") {
		t.Fatalf("stale session: %v", err)
	}
	if a.count(`SELECT count(*) FROM api_tokens`) != before {
		t.Fatal("a token was created without fresh 2FA")
	}
	// A lower scope is refused too, and so is every non-user actor.
	low := actions.UserSessionActor(strconv.FormatInt(a.uid, 10), a.public, actions.ScopeWrite, sess.MFAFreshUntil.Add(time.Hour))
	for name, actor := range map[string]actions.Actor{"user with write scope": low, "token admin": actions.TokenActor("1", actions.ScopeAdmin),
		"mcp": actions.MCPActor("m"), "webhook": actions.WebhookActor("h", "ci")} {
		if _, err := a.inv(actor, "tokens.create", in); !errors.Is(err, actions.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTokenExpiryDefaultMaximumAndRequired(t *testing.T) {
	a := newAEnv(t)
	expiry := func(days string) (time.Duration, error) {
		in := `{"name":"ci","scope":"read"` + days + `}`
		res, err := a.inv(a.user(), "tokens.create", in)
		if err != nil {
			return 0, err
		}
		return res.Out.(TokenCreateOut).ExpiresAt.Sub(a.clk.Now()), nil
	}
	day := 24 * time.Hour
	if d, err := expiry(""); err != nil || d != 90*day {
		t.Fatalf("default: %v %v", d, err)
	}
	if d, err := expiry(`,"expires_in_days":0`); err != nil || d != 90*day {
		t.Fatalf("0 means the default: %v %v", d, err)
	}
	for days, want := range map[string]time.Duration{`,"expires_in_days":1`: day, `,"expires_in_days":365`: 365 * day} {
		if d, err := expiry(days); err != nil || d != want {
			t.Fatalf("%s: %v %v", days, d, err)
		}
	}
	for _, bad := range []string{`,"expires_in_days":366`, `,"expires_in_days":-1`, `,"expires_in_days":100000`} {
		if _, err := expiry(bad); !errors.Is(err, actions.ErrInvalidInput) {
			t.Errorf("%s accepted: %v", bad, err)
		}
	}
	// Required in the schema: a token row without an expiry cannot exist.
	if _, err := a.st.W.Exec(`INSERT INTO api_tokens (user_id, name, prefix, hash, scope, created_at) VALUES (1, 'x', 'p', x'09', 'read', 1)`); err == nil {
		t.Error("the database accepts a token with no expiry")
	}
	// Bad names and scopes.
	for _, in := range []string{`{"name":"","scope":"read"}`, `{"name":"ci","scope":"root"}`, `{"name":"ci","scope":""}`, `{"name":"` + strings.Repeat("n", 65) + `","scope":"read"}`, `{"name":"a\u0000b","scope":"read"}`} {
		if _, err := a.inv(a.user(), "tokens.create", in); !errors.Is(err, actions.ErrInvalidInput) {
			t.Errorf("%s: %v", in, err)
		}
	}
}

func TestTokenFormatHashAndPlaintextNeverInAudit(t *testing.T) {
	a := newAEnv(t)
	tok := a.createToken("write", 30)
	if !strings.HasPrefix(tok.Token, "sd_") || len(tok.Token) != 3+43 || tok.Prefix != tok.Token[3:11] || tok.Scope != "write" {
		t.Fatalf("%+v", tok)
	}
	// Stored as a hash, with the prefix for display.
	var hash []byte
	var prefix string
	a.st.R.QueryRow(`SELECT hash, prefix FROM api_tokens WHERE id = ?`, tok.ID).Scan(&hash, &prefix)
	if len(hash) != 32 || prefix != tok.Prefix || bytes.Equal(hash, []byte(tok.Token)) || bytes.Contains(hash, []byte(tok.Token[3:])) {
		t.Fatalf("hash %x prefix %q", hash, prefix)
	}
	// Not in the database file, the WAL, the audit rows or the logs.
	secret := tok.Token
	body := tok.Token[3:]
	rows, _ := a.st.R.Query(`SELECT args_json, target, error FROM audit_log WHERE action = 'tokens.create'`)
	n := 0
	for rows.Next() {
		var args, target, e string
		rows.Scan(&args, &target, &e)
		n++
		if strings.Contains(args+target+e, body) || strings.Contains(args+target+e, fmt.Sprintf("%x", hash)) {
			t.Errorf("the token is in an audit row: %s %s %s", args, target, e)
		}
		if !strings.Contains(args, `"name":"ci"`) || !strings.Contains(args, `"scope":"write"`) {
			t.Errorf("the audit row lacks name and scope: %s", args)
		}
	}
	rows.Close()
	if n != 2 { // intent and outcome
		t.Errorf("%d audit rows for tokens.create", n)
	}
	if strings.Contains(a.logs.String(), body) {
		t.Error("the token is in the logs")
	}
	a.st.W.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	for _, f := range []string{"serverdock.db", "serverdock.db-wal"} {
		raw, _ := os.ReadFile(filepath.Join(a.dir, f))
		if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte(body)) {
			t.Errorf("the plaintext token is in %s", f)
		}
	}
	// tokens.list never returns it, or a hash.
	list := a.mustInv(a.user(), "tokens.list", "").Out.(TokenListOut)
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), body) || strings.Contains(string(b), "hash") || len(list.Tokens) != 1 || list.Tokens[0].Prefix != tok.Prefix {
		t.Errorf("list: %s", b)
	}
}

func TestVerifyTokenExpiredRevokedAndLastUsed(t *testing.T) {
	a := newAEnv(t)
	tok := a.createToken("read", 1)
	t2 := a.createToken("admin", 30) // revoked below
	t3 := a.createToken("read", 30)  // its user is reset below
	var writes atomic.Int32
	a.s.touchTx = func(c context.Context, fn func(*sql.Tx) error) error { writes.Add(1); return a.s.write(c, fn) }
	chk, err := a.s.VerifyToken(ctx, tok.Token)
	if err != nil || chk.UserID != a.uid || chk.Scope != "read" || chk.ID != tok.ID {
		t.Fatalf("%+v %v", chk, err)
	}
	for i := 0; i < 5; i++ { // once a minute: a burst writes last_used_at once
		a.clk.Add(5 * time.Second)
		if _, err := a.s.VerifyToken(ctx, tok.Token); err != nil {
			t.Fatal(err)
		}
	}
	if writes.Load() != 1 {
		t.Fatalf("%d writes for a burst of uses", writes.Load())
	}
	a.clk.Add(time.Minute)
	a.s.VerifyToken(ctx, tok.Token)
	if writes.Load() != 2 {
		t.Fatalf("%d writes after a minute", writes.Load())
	}
	// Malformed and unknown tokens are the same error; nothing leaks.
	for _, bad := range []string{"", "sd_", "sd_short", "xx_" + strings.Repeat("A", 43), "sd_" + strings.Repeat("A", 43), tok.Token + "x", tok.Token[:len(tok.Token)-1]} {
		if _, err := a.s.VerifyToken(ctx, bad); !errors.Is(err, ErrBadAPIToken) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// A touch failure is best-effort.
	a.clk.Add(2 * time.Minute)
	a.s.touchTx = func(context.Context, func(*sql.Tx) error) error { return errors.New("disk full") }
	if _, err := a.s.VerifyToken(ctx, tok.Token); err != nil {
		t.Fatalf("a failed last_used write failed the request: %v", err)
	}
	// Expired.
	a.clk.Add(25 * time.Hour)
	if _, err := a.s.VerifyToken(ctx, tok.Token); !errors.Is(err, ErrAPITokenExpired) {
		t.Fatalf("expired: %v", err)
	}
	// Revoked (a fresh token).
	a.clk.Add(time.Minute)
	if err := a.s.RevokeToken(ctx, a.uid, t2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.VerifyToken(ctx, t2.Token); !errors.Is(err, ErrBadAPIToken) {
		t.Fatalf("revoked: %v", err)
	}
	// A token of a user who is no longer an admin (after a reset) is refused.
	a.st.W.Exec(`UPDATE users SET password_hash = NULL, totp_confirmed_at = NULL`)
	if _, err := a.s.VerifyToken(ctx, t3.Token); !errors.Is(err, ErrBadAPIToken) {
		t.Fatalf("a reset user's token: %v", err)
	}
}

// Revoking a token (and a session) always runs, even when the audit log is down.
func TestRevokeRunsWhenTheAuditLogIsDown(t *testing.T) {
	a := newAEnv(t)
	tok := a.createToken("read", 30)
	a.sink.down.Store(true)
	res, err := a.inv(a.user(), "tokens.revoke", fmt.Sprintf(`{"id":%d}`, tok.ID))
	if !errors.Is(err, audit.ErrAuditOutcome) || res.Out == nil || res.AuditOutcomeRecorded || res.Audited {
		t.Fatalf("%+v %v", res, err)
	}
	if _, verr := a.s.VerifyToken(ctx, tok.Token); verr == nil {
		t.Fatal("the token still works after the revoke ran")
	}
	if !strings.Contains(a.logs.String(), "level=ERROR") {
		t.Error("the audit gap was not logged at ERROR")
	}
	// Creating a token with the log down is refused (it adds access).
	if _, err := a.inv(a.user(), "tokens.create", `{"name":"x","scope":"read"}`); !errors.Is(err, audit.ErrAuditUnavailable) {
		t.Errorf("create with the log down: %v", err)
	}
	// Sessions: the same rule.
	a.sink.down.Store(false)
	other, _ := a.s.CreatePending(ctx, a.uid, "x")
	_ = other
	a.sink.down.Store(true)
	if _, err := a.inv(a.user(), "auth.sessions_revoke", `{"all_others":true}`); !errors.Is(err, audit.ErrAuditOutcome) {
		t.Errorf("sessions_revoke with the log down: %v", err)
	}
}

// ---- the policy matrix: every auth and token action x every actor ---------------------

func TestNoTokenReachesASessionOnlyAction(t *testing.T) {
	a := newAEnv(t)
	inputs := policyInputs()
	list := a.reg.List()
	if len(list) != len(inputs) {
		t.Fatalf("%d registered, the test knows %d: extend the matrix", len(list), len(inputs))
	}
	for _, info := range list {
		in, ok := inputs[info.Name]
		if !ok {
			t.Fatalf("no input for %s", info.Name)
		}
		if !info.SessionOnly {
			t.Errorf("%s is not SessionOnly: a token could reach it", info.Name)
		}
		if strings.HasPrefix(info.Name, "tokens.") || strings.HasPrefix(info.Name, "auth.") {
			if info.ConsoleOnly {
				t.Errorf("%s is console-only", info.Name)
			}
		}
		snapshot := fmt.Sprint(a.count(`SELECT count(*) FROM api_tokens`), a.count(`SELECT count(*) FROM sessions`), a.count(`SELECT count(*) FROM recovery_codes WHERE used_at IS NULL`))
		for _, scope := range []actions.Scope{actions.ScopeRead, actions.ScopeWrite, actions.ScopeAdmin} {
			for _, actor := range []actions.Actor{actions.TokenActor("9", scope), actions.MCPActor("m"), actions.WebhookActor("h", "x")} {
				if _, err := a.inv(actor, info.Name, in); !errors.Is(err, actions.ErrDenied) {
					t.Errorf("%s as %s/%v: %v", info.Name, actor.Kind(), scope, err)
				}
			}
		}
		// Nothing ran: no token, session or code changed.
		if got := fmt.Sprint(a.count(`SELECT count(*) FROM api_tokens`), a.count(`SELECT count(*) FROM sessions`), a.count(`SELECT count(*) FROM recovery_codes WHERE used_at IS NULL`)); got != snapshot {
			t.Errorf("%s changed state when denied", info.Name)
		}
	}
}

func TestAuthActionsPolicyForTheSignedInUser(t *testing.T) {
	a := newAEnv(t)
	sess, _ := a.s.LookupSession(ctx, a.secret)
	mk := func(scope actions.Scope, fresh time.Time) actions.Actor {
		return actions.UserSessionActor(strconv.FormatInt(a.uid, 10), a.public, scope, fresh)
	}
	stale := sess.MFAFreshUntil.Add(-time.Hour)
	for _, name := range []string{"auth.password_change", "auth.totp_reenroll_begin", "auth.totp_reenroll_confirm", "auth.recovery_regenerate", "tokens.create"} {
		// Sensitive: fresh 2FA and admin scope.
		_, err := a.inv(mk(actions.ScopeAdmin, stale), name, policyInputs()[name])
		if !errors.Is(err, actions.ErrDenied) {
			t.Errorf("%s without fresh 2FA: %v", name, err)
		}
	}
	// Reads and revokes do not need the fresh window.
	for _, name := range []string{"auth.session_info", "auth.sessions_list", "tokens.list"} {
		if _, err := a.inv(mk(actions.ScopeAdmin, stale), name, ""); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := a.inv(mk(actions.ScopeRead, stale), "auth.session_info", ""); err != nil {
		t.Errorf("session_info needs only read scope: %v", err)
	}
	if _, err := a.inv(mk(actions.ScopeRead, stale), "auth.sessions_list", ""); !errors.Is(err, actions.ErrDenied) {
		t.Errorf("sessions_list needs admin scope: %v", err)
	}
}

func TestSessionInfoSessionsListAndRevoke(t *testing.T) {
	a := newAEnv(t)
	info := a.mustInv(a.user(), "auth.session_info", "").Out.(SessionInfoOut)
	if info.Username != "admin" || info.PublicID != a.public || info.RecoveryCodesLeft != 10 || info.AbsExpiresAt.IsZero() {
		t.Fatalf("%+v", info)
	}
	// Two more sessions, one belonging to another user.
	s2, _ := a.newSessionFor(a.uid)
	a.user2()
	list := a.mustInv(a.user(), "auth.sessions_list", "").Out.(SessionsListOut)
	if len(list.Sessions) != 2 {
		t.Fatalf("%d sessions listed", len(list.Sessions))
	}
	cur := 0
	for _, r := range list.Sessions {
		if r.Current {
			cur++
			if r.PublicID != a.public {
				t.Error("the wrong session is marked current")
			}
		}
	}
	if cur != 1 {
		t.Fatalf("%d current sessions", cur)
	}
	// Another user's session id cannot be revoked here.
	other := a.newSessionOf(2)
	if _, err := a.inv(a.user(), "auth.sessions_revoke", fmt.Sprintf(`{"public_id":%q}`, other)); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("%v", err)
	}
	if _, err := a.s.LookupSession(ctx, a.sessionSecretOf(other)); err != nil {
		t.Error("another user's session was revoked")
	}
	// Revoke one by id, then all the others.
	a.mustInv(a.user(), "auth.sessions_revoke", fmt.Sprintf(`{"public_id":%q}`, a.publicOf(s2)))
	if _, err := a.s.LookupSession(ctx, s2); err == nil {
		t.Error("the revoked session still works")
	}
	a.newSessionFor(a.uid)
	a.newSessionFor(a.uid)
	res := a.mustInv(a.user(), "auth.sessions_revoke", `{"all_others":true}`).Out.(RevokedOut)
	if res.Revoked != 2 {
		t.Errorf("revoked %d, want 2", res.Revoked)
	}
	if _, err := a.s.LookupSession(ctx, a.secret); err != nil {
		t.Errorf("the current session was revoked by all_others: %v", err)
	}
	for _, bad := range []string{`{}`, `{"public_id":"x","all_others":true}`} {
		if _, err := a.inv(a.user(), "auth.sessions_revoke", bad); !errors.Is(err, actions.ErrInvalidInput) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// helpers for sessions of several users
func (a *aenv) newSessionFor(uid int64) (secret string, public string) {
	a.t.Helper()
	var s string
	var sess Session
	err := a.s.write(ctx, func(tx *sql.Tx) error {
		var err error
		s, sess, err = a.s.createSession(ctx, tx, uid, "9.9.9.9", "ua", "{}", nil)
		return err
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return s, sess.PublicID
}

func (a *aenv) user2() {
	a.st.W.Exec(`INSERT OR IGNORE INTO users (id, username, password_hash, totp_confirmed_at, created_at) VALUES (2, 'second', 'x', 1, 1)`)
}

func (a *aenv) newSessionOf(uid int64) string {
	a.user2()
	s, p := a.newSessionFor(uid)
	a.others[p] = s
	return p
}

func (a *aenv) sessionSecretOf(public string) string { return a.others[public] }
func (a *aenv) publicOf(secret string) string {
	var got string
	a.st.R.QueryRow(`SELECT public_id FROM sessions WHERE id_hash = ?`, hashSecret(secret)).Scan(&got)
	return got
}

// ---- password change ----------------------------------------------------------------

func TestPasswordChangeRevokesEverythingByDefault(t *testing.T) {
	a := newAEnv(t)
	a.createToken("read", 30)
	a.createToken("admin", 30)
	a.newSessionFor(a.uid)
	pending, _ := a.s.CreatePending(ctx, a.uid, "x")
	res := a.mustInv(a.user(), "auth.password_change", fmt.Sprintf(`{"current_password":%q,"new_password":"a brand new passphrase"}`, goodPassword)).Out.(ChangedOut)
	if res.SessionsRevoked != 2 || res.TokensRevoked != 2 {
		t.Fatalf("%+v", res)
	}
	// Every session (the current one too) and the pending step are gone.
	if _, err := a.s.LookupSession(ctx, a.secret); err == nil {
		t.Error("the current session survived a password change")
	}
	if _, err := a.s.PendingAttempt(ctx, pending); !errors.Is(err, ErrPendingInvalid) {
		t.Errorf("a pending step survived: %v", err)
	}
	if a.count(`SELECT count(*) FROM api_tokens WHERE revoked_at IS NULL`) != 0 {
		t.Error("a token survived")
	}
	// The new password works and the old one does not.
	phc, _ := a.s.PasswordHash(ctx, a.uid)
	if ok, _ := a.s.hasher.Verify(ctx, phc, "a brand new passphrase"); !ok {
		t.Error("the new password does not verify")
	}
	if ok, _ := a.s.hasher.Verify(ctx, phc, goodPassword); ok {
		t.Error("the old password still verifies")
	}
	if a.count(`SELECT count(*) FROM users WHERE password_changed_at IS NOT NULL`) != 1 {
		t.Error("password_changed_at not set")
	}
}

func TestPasswordChangeKeepTokensKeepsTokens(t *testing.T) {
	a := newAEnv(t)
	tok := a.createToken("read", 30)
	res := a.mustInv(a.user(), "auth.password_change", fmt.Sprintf(`{"current_password":%q,"new_password":"a brand new passphrase","keep_tokens":true}`, goodPassword)).Out.(ChangedOut)
	if res.TokensRevoked != 0 || res.SessionsRevoked != 1 {
		t.Fatalf("%+v", res)
	}
	if _, err := a.s.VerifyToken(ctx, tok.Token); err != nil {
		t.Errorf("keep_tokens did not keep the token: %v", err)
	}
}

func TestPasswordChangeChecksTheCurrentPasswordAndTheRules(t *testing.T) {
	a := newAEnv(t)
	in := func(cur, nw string) string { return fmt.Sprintf(`{"current_password":%q,"new_password":%q}`, cur, nw) }
	if _, err := a.inv(a.user(), "auth.password_change", in("wrong password!!", "a brand new passphrase")); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("%v", err)
	}
	if _, err := a.inv(a.user(), "auth.password_change", in(goodPassword, "short")); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("a weak new password: %v", err)
	}
	if _, err := a.inv(a.user(), "auth.password_change", in(goodPassword, "admin")); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("%v", err)
	}
	// Nothing changed or was revoked by the refusals.
	if _, err := a.s.LookupSession(ctx, a.secret); err != nil {
		t.Fatalf("a refused change revoked the session: %v", err)
	}
	// Neither password appears in the audit rows or the logs, or in the returned error.
	for _, secret := range []string{"wrong password!!", "a brand new passphrase", goodPassword} {
		rows, _ := a.st.R.Query(`SELECT args_json || error FROM audit_log WHERE action = 'auth.password_change'`)
		for rows.Next() {
			var s string
			rows.Scan(&s)
			if strings.Contains(s, secret) {
				t.Errorf("%q is in an audit row: %s", secret, s)
			}
		}
		rows.Close()
		if strings.Contains(a.logs.String(), secret) {
			t.Errorf("%q is in the logs", secret)
		}
	}
	// Guessing the current password is bounded by the account back-off.
	for i := 0; i < 11; i++ {
		a.inv(a.user(), "auth.password_change", in(fmt.Sprintf("guess number %d!!", i), "a brand new passphrase"))
	}
	if _, err := a.inv(a.user(), "auth.password_change", in(goodPassword, "a brand new passphrase")); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("the right password during the back-off: %v", err)
	}
	a.clk.Add(time.Minute)
	if _, err := a.inv(a.user(), "auth.password_change", in(goodPassword, "a brand new passphrase")); err != nil {
		t.Fatalf("after the wait: %v", err)
	}
}

// ---- re-enrolment, recovery codes --------------------------------------------------------

func TestReenrolmentSwapsTheSeedAndRevokesEverything(t *testing.T) {
	a := newAEnv(t)
	tok := a.createToken("read", 30)
	begin := a.mustInv(a.user(), "auth.totp_reenroll_begin", "").Out.(ReenrollBeginOut)
	newSeed, err := b32().DecodeString(begin.Secret)
	if err != nil || len(newSeed) != 20 || !strings.HasPrefix(begin.OtpauthURI, "otpauth://totp/ServerDock:admin?") {
		t.Fatalf("%+v %v", begin, err)
	}
	// The pending seed is sealed; the old seed keeps working until confirmed.
	var pend []byte
	a.st.R.QueryRow(`SELECT totp_pending_seed FROM users WHERE id = ?`, a.uid).Scan(&pend)
	if len(pend) == 0 || bytes.Contains(pend, newSeed) {
		t.Fatalf("pending seed column %d bytes", len(pend))
	}
	a.clk.Add(31 * time.Second)
	sess, _ := a.s.LookupSession(ctx, a.secret)
	actor := actions.UserSessionActor(strconv.FormatInt(a.uid, 10), a.public, actions.ScopeAdmin, a.clk.Now().Add(time.Minute))
	_ = sess
	code := TOTPCode(newSeed, TOTPCounter(a.clk.Now()))
	// A code from the OLD seed does not confirm (and costs a try).
	oldCode := TOTPCode(a.seed, TOTPCounter(a.clk.Now()))
	if _, err := a.inv(actor, "auth.totp_reenroll_confirm", fmt.Sprintf(`{"enrollment_id":%q,"code":%q}`, begin.EnrollmentID, oldCode)); !errors.Is(err, ErrBadCode) {
		t.Fatalf("old seed's code: %v", err)
	}
	res := a.mustInv(actor.WithHostKind("localhost"), "auth.totp_reenroll_confirm", fmt.Sprintf(`{"enrollment_id":%q,"code":%q}`, begin.EnrollmentID, code)).Out.(ChangedOut)
	if res.SessionsRevoked != 1 || res.TokensRevoked != 1 {
		t.Fatalf("%+v", res)
	}
	if _, err := a.s.LookupSession(ctx, a.secret); err == nil {
		t.Error("the session survived the re-enrolment")
	}
	if _, err := a.s.VerifyToken(ctx, tok.Token); err == nil {
		t.Error("the token survived the re-enrolment")
	}
	got, err := a.s.UserSeed(ctx, a.uid)
	if err != nil || !bytes.Equal(got, newSeed) {
		t.Fatalf("the seed was not swapped: %v", err)
	}
	var host string
	var pendingLeft int
	var counter int64
	a.st.R.QueryRow(`SELECT totp_enrolled_host, totp_last_counter, (SELECT count(*) FROM users WHERE totp_pending_seed IS NOT NULL) FROM users`).Scan(&host, &counter, &pendingLeft)
	if host != "localhost" || counter != TOTPCounter(a.clk.Now()) || pendingLeft != 0 {
		t.Fatalf("host %q counter %d pending %d", host, counter, pendingLeft)
	}
	// The same code cannot be used again at sign-in, and the old seed is dead.
	if err := a.s.AcceptTOTP(ctx, a.uid, TOTPCounter(a.clk.Now())); !errors.Is(err, ErrCodeReused) {
		t.Errorf("the confirm code could be reused: %v", err)
	}
	if _, ok := VerifyTOTP(got, oldCode, a.clk.Now()); ok {
		t.Error("the old seed's code verifies against the new seed")
	}
}

func TestReenrolmentKeepTokensReplacedAndTries(t *testing.T) {
	a := newAEnv(t)
	tok := a.createToken("read", 30)
	first := a.mustInv(a.user(), "auth.totp_reenroll_begin", "").Out.(ReenrollBeginOut)
	second := a.mustInv(a.user(), "auth.totp_reenroll_begin", "").Out.(ReenrollBeginOut) // replaces the first
	a.clk.Add(31 * time.Second)
	actor := actions.UserSessionActor(strconv.FormatInt(a.uid, 10), a.public, actions.ScopeAdmin, a.clk.Now().Add(time.Minute))
	seed1, _ := b32().DecodeString(first.Secret)
	seed2, _ := b32().DecodeString(second.Secret)
	if _, err := a.inv(actor, "auth.totp_reenroll_confirm", fmt.Sprintf(`{"enrollment_id":%q,"code":%q}`, first.EnrollmentID, TOTPCode(seed1, TOTPCounter(a.clk.Now())))); !errors.Is(err, ErrEnrollmentReplaced) {
		t.Fatalf("a replaced enrolment: %v", err)
	}
	// 5 tries per enrolment: five wrong codes discard it; the right code is then too late.
	for i := 0; i < 5; i++ {
		_, err := a.inv(actor, "auth.totp_reenroll_confirm", fmt.Sprintf(`{"enrollment_id":%q,"code":"000000"}`, second.EnrollmentID))
		var re *ReenrollError
		if !errors.Is(err, ErrBadCode) || !errors.As(err, &re) || re.TriesLeft != 4-i {
			t.Fatalf("try %d: %v", i, err)
		}
	}
	if _, err := a.inv(actor, "auth.totp_reenroll_confirm", fmt.Sprintf(`{"enrollment_id":%q,"code":%q}`, second.EnrollmentID, TOTPCode(seed2, TOTPCounter(a.clk.Now())))); err == nil {
		t.Fatal("an enrolment worked after its tries ran out")
	}
	// A fresh enrolment confirmed with keep_tokens keeps the token.
	third := a.mustInv(a.user(), "auth.totp_reenroll_begin", "").Out.(ReenrollBeginOut)
	seed3, _ := b32().DecodeString(third.Secret)
	res := a.mustInv(actor, "auth.totp_reenroll_confirm", fmt.Sprintf(`{"enrollment_id":%q,"code":%q,"keep_tokens":true}`, third.EnrollmentID, TOTPCode(seed3, TOTPCounter(a.clk.Now())))).Out.(ChangedOut)
	if res.TokensRevoked != 0 {
		t.Fatalf("%+v", res)
	}
	if _, err := a.s.VerifyToken(ctx, tok.Token); err != nil {
		t.Errorf("keep_tokens did not keep the token: %v", err)
	}
	// Expiry: ten minutes.
	fourth := a.mustInv(actions.UserSessionActor(strconv.FormatInt(a.uid, 10), a.public, actions.ScopeAdmin, a.clk.Now().Add(time.Hour)), "auth.totp_reenroll_begin", "").Out.(ReenrollBeginOut)
	seed4, _ := b32().DecodeString(fourth.Secret)
	a.clk.Add(11 * time.Minute)
	if _, err := a.inv(actions.UserSessionActor(strconv.FormatInt(a.uid, 10), a.public, actions.ScopeAdmin, a.clk.Now().Add(time.Hour)), "auth.totp_reenroll_confirm",
		fmt.Sprintf(`{"enrollment_id":%q,"code":%q}`, fourth.EnrollmentID, TOTPCode(seed4, TOTPCounter(a.clk.Now())))); !errors.Is(err, ErrPendingInvalid) {
		t.Fatalf("an expired enrolment: %v", err)
	}
}

func TestRecoveryRegenerateReplacesAllTen(t *testing.T) {
	a := newAEnv(t)
	a.s.UseRecoveryCode(ctx, a.uid, a.codes[0])
	out := a.mustInv(a.user(), "auth.recovery_regenerate", "").Out.(RecoveryOut)
	if len(out.Codes) != 10 {
		t.Fatalf("%d codes", len(out.Codes))
	}
	for _, old := range a.codes {
		if ok, _ := a.s.UseRecoveryCode(ctx, a.uid, old); ok {
			t.Error("an old code still works")
		}
	}
	if left, _ := a.s.RecoveryCodesLeft(ctx, a.uid); left != 10 {
		t.Errorf("%d codes left", left)
	}
	if ok, _ := a.s.UseRecoveryCode(ctx, a.uid, out.Codes[4]); !ok {
		t.Error("a new code does not work")
	}
	// The new codes are never in the audit rows.
	rows, _ := a.st.R.Query(`SELECT args_json || error FROM audit_log WHERE action = 'auth.recovery_regenerate'`)
	defer rows.Close()
	for rows.Next() {
		var s string
		rows.Scan(&s)
		for _, c := range out.Codes {
			if strings.Contains(s, c) {
				t.Error("a recovery code is in an audit row")
			}
		}
	}
}

// Valid input for every auth and token action (strict decoding rejects extras).
func policyInputs() map[string]string {
	return map[string]string{
		"auth.session_info": `{}`, "auth.password_change": `{"current_password":"x","new_password":"yyyyyyyyyyyyy"}`, "auth.totp_reenroll_begin": `{}`,
		"auth.totp_reenroll_confirm": `{"enrollment_id":"e","code":"123456"}`, "auth.recovery_regenerate": `{}`, "auth.sessions_list": `{}`,
		"auth.sessions_revoke": `{"all_others":true}`, "tokens.create": `{"name":"n","scope":"read"}`, "tokens.list": `{}`, "tokens.revoke": `{"id":1}`,
	}
}

// host_kind is server state, never client input: it is not an input field, and an
// unknown host records nothing.
func TestReenrolmentHostKindIsNeverClientInput(t *testing.T) {
	a := newAEnv(t)
	begin := a.mustInv(a.user(), "auth.totp_reenroll_begin", "").Out.(ReenrollBeginOut)
	seed, _ := b32().DecodeString(begin.Secret)
	a.clk.Add(31 * time.Second)
	actor := actions.UserSessionActor(strconv.FormatInt(a.uid, 10), a.public, actions.ScopeAdmin, a.clk.Now().Add(time.Minute))
	code := TOTPCode(seed, TOTPCounter(a.clk.Now()))
	// A client cannot send it.
	if _, err := a.inv(actor, "auth.totp_reenroll_confirm", fmt.Sprintf(`{"enrollment_id":%q,"code":%q,"host_kind":"localhost"}`, begin.EnrollmentID, code)); !errors.Is(err, actions.ErrInvalidInput) {
		t.Fatalf("host_kind was accepted as input: %v", err)
	}
	var before string
	a.st.R.QueryRow(`SELECT totp_enrolled_host FROM users`).Scan(&before)
	// No Host on the call: the column is left alone.
	a.mustInv(actor, "auth.totp_reenroll_confirm", fmt.Sprintf(`{"enrollment_id":%q,"code":%q}`, begin.EnrollmentID, code))
	var after string
	a.st.R.QueryRow(`SELECT totp_enrolled_host FROM users`).Scan(&after)
	if after != before || after != "ip" {
		t.Errorf("host recorded without a Host: %q -> %q", before, after)
	}
}

// Typos while re-enrolling are not second-factor failures for the sign-in notice.
func TestReenrolmentTyposAreNotSignInFailures(t *testing.T) {
	a := newAEnv(t)
	begin := a.mustInv(a.user(), "auth.totp_reenroll_begin", "").Out.(ReenrollBeginOut)
	a.clk.Add(31 * time.Second)
	actor := actions.UserSessionActor(strconv.FormatInt(a.uid, 10), a.public, actions.ScopeAdmin, a.clk.Now().Add(time.Minute))
	for i := 0; i < 3; i++ {
		a.inv(actor, "auth.totp_reenroll_confirm", fmt.Sprintf(`{"enrollment_id":%q,"code":"000000"}`, begin.EnrollmentID))
	}
	var fails int
	a.st.R.QueryRow(`SELECT totp_fail_since_login FROM users`).Scan(&fails)
	if fails != 0 {
		t.Errorf("%d failures counted for re-enrolment typos", fails)
	}
}

func TestSessionsRevokeAllOthersNeedsToKnowTheCallersSession(t *testing.T) {
	a := newAEnv(t)
	a.newSessionFor(a.uid)
	noSession := actions.UserActor(strconv.FormatInt(a.uid, 10), actions.ScopeAdmin, a.clk.Now().Add(time.Hour))
	before := a.count(`SELECT count(*) FROM sessions`)
	if _, err := a.inv(noSession, "auth.sessions_revoke", `{"all_others":true}`); err == nil {
		t.Fatal("all_others ran without a session id")
	}
	if a.count(`SELECT count(*) FROM sessions`) != before {
		t.Fatal("sessions were deleted")
	}
}

// Revoked or long-expired token rows are pruned when a token is created (100 at a time).
func TestTokenCreatePrunesOldRows(t *testing.T) {
	a := newAEnv(t)
	now := a.clk.Now().Unix()
	day := int64(86400)
	for i := 0; i < 130; i++ { // revoked 40 days ago
		a.st.W.Exec(`INSERT INTO api_tokens (user_id, name, prefix, hash, scope, created_at, expires_at, revoked_at) VALUES (?, 'old', 'p', ?, 'read', 1, ?, ?)`, a.uid, []byte(fmt.Sprint("r", i)), now+100*day, now-40*day)
	}
	a.st.W.Exec(`INSERT INTO api_tokens (user_id, name, prefix, hash, scope, created_at, expires_at) VALUES (?, 'expired-long-ago', 'p', x'aa', 'read', 1, ?)`, a.uid, now-40*day)
	a.st.W.Exec(`INSERT INTO api_tokens (user_id, name, prefix, hash, scope, created_at, expires_at, revoked_at) VALUES (?, 'revoked-recently', 'p', x'bb', 'read', 1, ?, ?)`, a.uid, now+100*day, now-5*day)
	a.st.W.Exec(`INSERT INTO api_tokens (user_id, name, prefix, hash, scope, created_at, expires_at) VALUES (?, 'expired-recently', 'p', x'cc', 'read', 1, ?)`, a.uid, now-5*day)
	live := a.createToken("read", 30)
	if n := a.count(`SELECT count(*) FROM api_tokens WHERE name = 'old'`); n != 131-100-1 && n != 30 {
		// 131 old candidates (130 revoked + 1 expired), 100 removed by the first create
		t.Logf("old rows left: %d", n)
	}
	if n := a.count(`SELECT count(*) FROM api_tokens WHERE name IN ('old', 'expired-long-ago')`); n != 31 {
		t.Errorf("%d long-dead rows left after one create, want 31 (100 of 131 pruned)", n)
	}
	a.createToken("read", 30)
	if n := a.count(`SELECT count(*) FROM api_tokens WHERE name IN ('old', 'expired-long-ago')`); n != 0 {
		t.Errorf("%d long-dead rows left after two creates", n)
	}
	for _, kept := range []string{"revoked-recently", "expired-recently", "ci"} {
		if a.count(`SELECT count(*) FROM api_tokens WHERE name = ?`, kept) == 0 {
			t.Errorf("%s was pruned", kept)
		}
	}
	if _, err := a.s.VerifyToken(ctx, live.Token); err != nil {
		t.Errorf("a live token was pruned: %v", err)
	}
}
