package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
	"github.com/Docker-Hunterpedia/serverdock/internal/testbin"
)

// seededAdmin is a data directory that has completed first run: an admin with a
// session, a pending sign-in, recovery codes, known IPs, an API token and the keys.
type seededAdmin struct {
	data    string
	userID  int64
	seed    []byte
	pending string
	session string
	token   string // the setup token that was used (now gone)
}

func seedAdmin(t *testing.T) seededAdmin {
	t.Helper()
	data := newData(t)
	// A server start creates the keys, the hub files and the setup token.
	_, stop := startServer(t, serverConfig{Data: data, ControlSocket: shortSock(t), AgentListen: "127.0.0.1:0", AgentSANs: []string{"127.0.0.1"}})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key, _ := os.ReadFile(secretbox.KeyPath(data))
	box, _ := secretbox.New(key)
	as := auth.NewStore(st, box, auth.NewHasher(), auth.Options{})
	tokB, _ := os.ReadFile(filepath.Join(data, "setup-token"))
	tok := strings.TrimSpace(string(tokB))
	er, err := as.SetupEnroll(ctx, tok, "admin")
	if err != nil {
		t.Fatal(err)
	}
	secret := er.Secret
	raw, err := decodeB32(secret)
	if err != nil {
		t.Fatal(err)
	}
	res, err := as.SetupComplete(ctx, auth.SetupComplete{Token: tok, EnrollmentID: er.EnrollmentID, Username: "admin", Password: "correct horse battery staple",
		Code: auth.TOTPCode(raw, auth.TOTPCounter(time.Now())), IPKey: "203.0.113.5", UserAgent: "ua", HostKind: "ip"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := as.CreatePending(ctx, res.UserID, "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	st.W.Exec(`INSERT INTO auth_known_ips (user_id, ip_key, last_ok_at) VALUES (?, '203.0.113.5', ?)`, res.UserID, time.Now().Unix())
	st.W.Exec(`INSERT INTO api_tokens (user_id, name, prefix, hash, scope, created_at, expires_at) VALUES (?, 'ci', 'abcdefgh', x'0102', 'read', 1, 9999999999), (?, 'ci2', 'ijklmnop', x'0304', 'write', 1, 9999999999)`, res.UserID, res.UserID)
	st.W.Exec(`UPDATE users SET totp_fail_since_login = 3, totp_fail_last_at = 5, totp_fail_last_ip = '1.2.3.4' WHERE id = ?`, res.UserID)
	return seededAdmin{data: data, userID: res.UserID, seed: raw, pending: pending, session: res.SessionSecret, token: tok}
}

func runReset(t *testing.T, data string, extra ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	args := append([]string{"reset", "--data", data}, extra...)
	code = adminReset(args, strings.NewReader(""), false, os.Geteuid(), &o, &e)
	return code, o.String(), e.String()
}

func dbCount(t *testing.T, data, q string, args ...any) int {
	t.Helper()
	ro, err := store.OpenReadOnly(data, store.ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var n int
	if err := ro.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func fileSums(t *testing.T, data string, rels ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, r := range rels {
		b, err := os.ReadFile(filepath.Join(data, r))
		if err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		s := sha256.Sum256(b)
		out[r] = string(s[:])
	}
	return out
}

func TestAdminResetClearsExactlyTheAdminState(t *testing.T) {
	a := seedAdmin(t)
	stale := filepath.Join(a.data, "setup-token") // a leftover from before the reset
	os.WriteFile(stale, []byte("stale\n"), 0o600)
	keep := []string{"pki/ca.key", "pki/ca.crt", "pki/hub.pem", "agent.token", "keys/master.key"}
	before := fileSums(t, a.data, keep...)
	if dbCount(t, a.data, `SELECT count(*) FROM sessions`) != 1 || dbCount(t, a.data, `SELECT count(*) FROM recovery_codes`) != 10 {
		t.Fatal("the fixture is not what the test expects")
	}
	code, out, errOut := runReset(t, a.data, "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	for _, want := range []string{"1 sessions deleted", "1 pending sign-ins deleted", "2 API tokens revoked", "Audit row", "new setup token to " + a.data + "/setup-token"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("the stale setup token file was not removed")
	}
	// The audit row id in the output is the reset's own intent row.
	ro1, _ := store.OpenReadOnly(a.data, store.ReadOnlyOptions{})
	var intentID int64
	ro1.DB.QueryRow(`SELECT id FROM audit_log WHERE action = 'auth.admin_reset' AND phase = 'intent'`).Scan(&intentID)
	ro1.Close()
	if !strings.Contains(out, fmt.Sprintf("Audit row %d (intent)", intentID)) || intentID == 0 {
		t.Errorf("the output does not name the intent row %d:\n%s", intentID, out)
	}
	// Cleared: every credential table, the setup state, the key id.
	for _, q := range []string{`SELECT count(*) FROM sessions`, `SELECT count(*) FROM auth_pending`, `SELECT count(*) FROM auth_known_ips`,
		`SELECT count(*) FROM recovery_codes`, `SELECT count(*) FROM setup_state`,
		`SELECT count(*) FROM api_tokens WHERE revoked_at IS NULL`,
		`SELECT count(*) FROM users WHERE password_hash IS NOT NULL OR totp_seed IS NOT NULL OR totp_confirmed_at IS NOT NULL OR totp_pending_seed IS NOT NULL OR totp_last_counter != 0 OR totp_fail_since_login != 0`} {
		if n := dbCount(t, a.data, q); n != 0 {
			t.Errorf("%s = %d after the reset", q, n)
		}
	}
	// The key id was cleared with the ciphertexts and, because the key file is
	// still there, recorded again from it (the same key: nothing is orphaned).
	keyBytes, _ := os.ReadFile(secretbox.KeyPath(a.data))
	kb, _ := secretbox.New(keyBytes)
	ro0, _ := store.OpenReadOnly(a.data, store.ReadOnlyOptions{})
	var recorded string
	ro0.DB.QueryRow(`SELECT value FROM meta WHERE key = 'secretbox_key_id'`).Scan(&recorded)
	ro0.Close()
	if recorded != kb.KeyID() {
		t.Errorf("recorded key id %q, want %q", recorded, kb.KeyID())
	}
	// Kept: the user row and its id (and name), the revoked tokens, the keys, the CA, the leaf, the agent token.
	if dbCount(t, a.data, `SELECT count(*) FROM users WHERE id = ? AND username = 'admin'`, a.userID) != 1 {
		t.Error("the user row or its id changed")
	}
	if dbCount(t, a.data, `SELECT count(*) FROM api_tokens`) != 2 {
		t.Error("API tokens were deleted, not revoked")
	}
	after := fileSums(t, a.data, keep...)
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed", k)
		}
	}
	if _, err := os.Stat(filepath.Join(a.data, "pki", "panel.pem")); err == nil {
		// not created here (no proxy), but a leaf that exists must be untouched
		t.Log("panel.pem present")
	}
	// Nothing secret is printed.
	for _, s := range []string{a.token, auth.TOTPSecret(a.seed), "correct horse", a.session, a.pending} {
		if strings.Contains(out+errOut, s) {
			t.Errorf("the output contains a secret: %q", s)
		}
	}
	// Intent and outcome rows with actor system, for the console action.
	ro, _ := store.OpenReadOnly(a.data, store.ReadOnlyOptions{})
	defer ro.Close()
	rows, _ := ro.DB.Query(`SELECT phase, result, actor_kind, actor_id, risk FROM audit_log WHERE action = 'auth.admin_reset' ORDER BY id`)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var phase, result, kind, id, risk string
		rows.Scan(&phase, &result, &kind, &id, &risk)
		got = append(got, phase+"/"+result+"/"+kind+"/"+id+"/"+risk)
	}
	if strings.Join(got, " ") != "intent/pending/system/system/destructive outcome/ok/system/system/destructive" {
		t.Errorf("audit rows: %v", got)
	}
}

// A pending password-only sign-in and a session from before the reset can neither
// complete nor authenticate afterwards.
func TestPreResetPendingRowCannotComplete(t *testing.T) {
	a := seedAdmin(t)
	if code, out, e := runReset(t, a.data, "--yes"); code != 0 {
		t.Fatalf("%d %s%s", code, out, e)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, a.data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key, _ := os.ReadFile(secretbox.KeyPath(a.data))
	box, _ := secretbox.New(key)
	as := auth.NewStore(st, box, auth.NewHasher(), auth.Options{})
	code := auth.TOTPCode(a.seed, auth.TOTPCounter(time.Now().Add(31*time.Second)))
	if _, err := as.CompleteLogin(ctx, auth.LoginInput{PendingSecret: a.pending, Code: code, IPKey: "x"}); err == nil {
		t.Fatal("a pre-reset pending row completed")
	}
	if _, err := as.LookupSession(ctx, a.session); err == nil {
		t.Fatal("a pre-reset session authenticated")
	}
	if ok, _ := as.AdminExists(ctx); ok {
		t.Fatal("an admin exists after the reset")
	}
}

// After the reset, only a NEW setup token (written by the next start) reopens setup.
func TestSetupReopensOnlyWithNewTokenAfterReset(t *testing.T) {
	a := seedAdmin(t)
	if code, out, e := runReset(t, a.data, "--yes"); code != 0 {
		t.Fatalf("%d %s%s", code, out, e)
	}
	if _, err := os.Stat(filepath.Join(a.data, "setup-token")); err == nil {
		t.Fatal("the reset wrote a token itself (the next start does)")
	}
	_, stop := startServer(t, serverConfig{Data: a.data, ControlSocket: shortSock(t)})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	nb, err := os.ReadFile(filepath.Join(a.data, "setup-token"))
	if err != nil {
		t.Fatal("the next start did not write a setup token")
	}
	newTok := strings.TrimSpace(string(nb))
	if newTok == a.token {
		t.Fatal("the old setup token came back")
	}
	ctx := context.Background()
	st, _ := store.Open(ctx, a.data)
	defer st.Close()
	key, _ := os.ReadFile(secretbox.KeyPath(a.data))
	box, _ := secretbox.New(key)
	as := auth.NewStore(st, box, auth.NewHasher(), auth.Options{})
	if _, err := as.SetupEnroll(ctx, a.token, "admin"); err == nil {
		t.Error("the old setup token reopened setup")
	}
	if _, err := as.SetupEnroll(ctx, newTok, "admin"); err != nil {
		t.Errorf("the new token does not work: %v", err)
	}
}

// With the key file removed (after the DB recorded its id and holds ciphertexts)
// the server refuses to start; `admin reset` clears the ciphertexts and the id and
// writes a new key, and then the server starts.
func TestResetWithTheKeyFileRemovedThenTheServerStarts(t *testing.T) {
	a := seedAdmin(t)
	os.Remove(secretbox.KeyPath(a.data))
	err := runServer(context.Background(), serverConfig{Data: a.data, ControlSocket: shortSock(t)}, quietLog(), nil)
	if err == nil || !strings.Contains(err.Error(), "admin reset") {
		t.Fatalf("the server started without its key: %v", err)
	}
	code, out, e := runReset(t, a.data, "--yes")
	if code != 0 || !strings.Contains(out, "a new master key created") {
		t.Fatalf("%d %s%s", code, out, e)
	}
	if b, err := os.ReadFile(secretbox.KeyPath(a.data)); err != nil || len(b) != 32 {
		t.Fatalf("no new key: %v", err)
	}
	if st, _ := os.Stat(secretbox.KeyPath(a.data)); st.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", st.Mode())
	}
	_, stop := startServer(t, serverConfig{Data: a.data, ControlSocket: shortSock(t)})
	if err := stop(); err != nil {
		t.Fatalf("the server did not start after the reset: %v", err)
	}
	// The new key's id is recorded again.
	if dbCount(t, a.data, `SELECT count(*) FROM meta WHERE key = 'secretbox_key_id'`) != 1 {
		t.Error("the key id was not recorded after the restart")
	}
}

func TestAdminResetRefusedWhileTheServerRuns(t *testing.T) {
	data := newData(t)
	addrs, stop := startServer(t, serverConfig{Data: data, ControlSocket: shortSock(t)})
	_ = addrs
	code, out, e := runReset(t, data, "--yes")
	if code != 1 || !strings.Contains(e, "ServerDock is running; stop it first") || strings.Contains(out, "Reset done") {
		t.Fatalf("%d %s%s", code, out, e)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestAdminResetNeedsATerminalOrYes(t *testing.T) {
	a := seedAdmin(t)
	var o, e bytes.Buffer
	if code := adminReset([]string{"reset", "--data", a.data}, strings.NewReader("reset\n"), false, os.Geteuid(), &o, &e); code != 2 || !strings.Contains(e.String(), "not a terminal") {
		t.Fatalf("%d %s", code, e.String())
	}
	// On a terminal, the operator must type "reset".
	o.Reset()
	e.Reset()
	if code := adminReset([]string{"reset", "--data", a.data}, strings.NewReader("no\n"), true, os.Geteuid(), &o, &e); code != 1 || !strings.Contains(e.String(), "nothing was changed") {
		t.Fatalf("%d %s", code, e.String())
	}
	if dbCount(t, a.data, `SELECT count(*) FROM sessions`) != 1 {
		t.Fatal("an unconfirmed reset changed state")
	}
	o.Reset()
	e.Reset()
	if code := adminReset([]string{"reset", "--data", a.data}, strings.NewReader("reset\n"), true, os.Geteuid(), &o, &e); code != 0 {
		t.Fatalf("%d %s", code, e.String())
	}
	if dbCount(t, a.data, `SELECT count(*) FROM sessions`) != 0 {
		t.Fatal("a confirmed reset did nothing")
	}
	// Usage errors.
	for _, args := range [][]string{nil, {"wipe"}, {"reset", "extra"}, {"reset", "--data"}} {
		if code := adminReset(args, strings.NewReader(""), false, os.Geteuid(), &o, &e); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if code := adminReset([]string{"reset", "--yes", "--data", filepath.Join(t.TempDir(), "missing")}, strings.NewReader(""), false, os.Geteuid(), &o, &e); code != 1 {
		t.Errorf("a missing data directory: exit %d", code)
	}
}

// Refused for a DATA that is not 0700 or is another user's (as non-root), and, as
// root, for a DATA owned by another uid: no file is created or changed.
func TestAdminResetOwnerRules(t *testing.T) {
	loose := filepath.Join(t.TempDir(), "d")
	os.Mkdir(loose, 0o755)
	os.Chmod(loose, 0o755)
	if code, _, e := runReset(t, loose, "--yes"); code != 1 || !strings.Contains(e, "chmod 700") {
		t.Errorf("a 0755 data dir: %d %s", code, e)
	}
	if ents, _ := os.ReadDir(loose); len(ents) != 0 {
		t.Error("a refused reset wrote into DATA")
	}
}

func TestAdminResetAsRootOnDataOwnedByAnotherUserIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	if os.Getuid() != 0 {
		t.Skip("needs root (run it in a container)")
	}
	const svc = 65534
	base, _ := os.MkdirTemp("", "sdadm")
	t.Cleanup(func() { os.RemoveAll(base) })
	os.Chmod(base, 0o755)
	data := filepath.Join(base, "data")
	os.Mkdir(data, 0o700)
	os.Chown(data, svc, svc)
	// The service user's server creates DATA's contents.
	p := testbin.Start(t, testbin.Options{Uid: svc, Args: []string{"server", "--data", data, "--proxy=false"}})
	if err := p.WaitLog("control plane listening", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	p.Stop()
	snap := func() map[string]string {
		out := map[string]string{}
		filepath.WalkDir(data, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			st, _ := d.Info()
			sys := st.Sys().(*syscall.Stat_t)
			rel, _ := filepath.Rel(data, path)
			out[rel] = strings.Join([]string{st.Mode().String(), itoa(int(sys.Uid)), itoa(int(st.Size())), st.ModTime().String()}, " ")
			if sys.Uid == 0 {
				t.Errorf("%s is owned by root", rel)
			}
			return nil
		})
		return out
	}
	before := snap()
	res := testbin.Run(t, testbin.Options{Args: []string{"admin", "reset", "--yes", "--data", data}})
	if res.Code != 1 || !strings.Contains(res.Stderr, "sudo -u") || !strings.Contains(res.Stderr, "serverdock admin reset") {
		t.Fatalf("exit %d\n%s%s", res.Code, res.Stdout, res.Stderr)
	}
	after := snap()
	if len(before) != len(after) {
		t.Fatalf("files were created or removed: %d -> %d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed: %q -> %q", k, v, after[k])
		}
	}
	// As the owner it works, and leaves nothing root-owned.
	own := testbin.Run(t, testbin.Options{Uid: svc, Args: []string{"admin", "reset", "--yes", "--data", data}})
	if own.Code != 0 {
		t.Fatalf("as the owner: exit %d\n%s%s", own.Code, own.Stdout, own.Stderr)
	}
	snap()
}

func itoa(n int) string { return strconv.Itoa(n) }

// A pending migration runs during the reset (with the pre-migration backup), and
// the umask of the reset process is 0077 (checked on the built binary).
func TestAdminResetRunsMigrationsAndKeepsAPrivateUmask(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	data := newData(t)
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "store", "testdata", "v2.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "serverdock.db"), fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	res := testbin.Run(t, testbin.Options{Args: []string{"admin", "reset", "--yes", "--data", data}})
	if res.Code != 0 {
		t.Fatalf("exit %d\n%s%s", res.Code, res.Stdout, res.Stderr)
	}
	ro, err := store.OpenReadOnly(data, store.ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var n int
	ro.DB.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n)
	if n != 4 {
		t.Errorf("%d migrations applied", n)
	}
	bs, _ := filepath.Glob(filepath.Join(data, "backups", "pre-migrate-*-v0002-v0004.db"))
	if len(bs) != 1 {
		t.Fatalf("backups %v", bs)
	}
	// The test process has a 022 umask; everything the reset created is private.
	filepath.WalkDir(data, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		st, _ := d.Info()
		rel, _ := filepath.Rel(data, path)
		if rel != "." && st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v: the reset ran with a loose umask", rel, st.Mode())
		}
		return nil
	})
	// The reset created the master key.
	if _, err := os.Stat(secretbox.KeyPath(data)); err != nil {
		t.Errorf("no master key: %v", err)
	}
}

// auth.admin_reset exists only in the reset process.
func TestServerRegistryHasNoAdminReset(t *testing.T) {
	data := newData(t)
	ctx := context.Background()
	st, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	al := newAuditLog(t, st)
	reg := newServerRegistry(al, quietLog(), actions.Deps{Version: "t", Started: time.Now(), Audit: al, DB: st.R}, auth.NewStore(st, nil, nil, auth.Options{}), auth.ActionDeps{})
	names := map[string]bool{}
	for _, a := range reg.List() {
		if a.Name == "auth.admin_reset" || a.ConsoleOnly {
			t.Fatalf("the server's registry has %s", a.Name)
		}
		names[a.Name] = true
	}
	for _, want := range []string{"tokens.create", "tokens.list", "tokens.revoke", "auth.password_change", "auth.totp_reenroll_begin", "auth.totp_reenroll_confirm",
		"auth.recovery_regenerate", "auth.sessions_list", "auth.sessions_revoke", "auth.session_info"} {
		if !names[want] {
			t.Errorf("the server's registry lacks %s", want)
		}
	}
	// And the server's source never registers it.
	src, _ := os.ReadFile("server.go")
	if strings.Contains(string(src), "RegisterAdminReset") {
		t.Error("cmd/serverdock/server.go registers auth.admin_reset")
	}
	// The reset process's registry has it, and only it.
	rr := actions.New(al, quietLog())
	auth.RegisterAdminReset(rr, auth.NewStore(st, nil, nil, auth.Options{}))
	list := rr.List()
	if len(list) != 1 || list[0].Name != "auth.admin_reset" || !list[0].ConsoleOnly || list[0].System == "" || list[0].Risk != actions.RiskDestructive {
		t.Fatalf("%+v", list)
	}
}

// The unattended-destructive check covers the admin-reset registry too: the action
// must be listed in docs/security.md.
func TestAdminResetIsListedAsAnUnattendedDestructiveAction(t *testing.T) {
	data := newData(t)
	st, err := store.Open(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rr := actions.New(newAuditLog(t, st), quietLog())
	auth.RegisterAdminReset(rr, auth.NewStore(st, nil, nil, auth.Options{}))
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "security.md"))
	if err != nil {
		t.Fatal(err)
	}
	if m := actions.UnlistedUnattendedDestructive(rr.List(), string(doc)); len(m) != 0 {
		t.Errorf("not listed under \"Unattended destructive actions\" in docs/security.md: %v", m)
	}
	// The check has teeth: it does flag the action in a document that lacks it.
	if m := actions.UnlistedUnattendedDestructive(rr.List(), "## Unattended destructive actions\n\nnone\n"); len(m) != 1 {
		t.Errorf("the check did not flag the action: %v", m)
	}
}

func decodeB32(s string) ([]byte, error) {
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
}

func newAuditLog(t *testing.T, st *store.Store) *audit.Logger {
	t.Helper()
	al, err := audit.New(st, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	return al
}

// The HTTP layer never refers to the reset action or the system invoker.
func TestAPIPackageNeverReferencesAdminReset(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "internal", "api", "*.go"))
	if len(files) == 0 {
		t.Fatal("no files found")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, bad := range []string{"AdminReset", "admin_reset", "SystemInvoker"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s mentions %s", f, bad)
			}
		}
	}
}

// The sign-in handlers and the auth actions must share one set of limiters: the
// server builds exactly one.
func TestServerBuildsOneLimitsInstance(t *testing.T) {
	src, _ := os.ReadFile("server.go")
	if n := strings.Count(string(src), "api.NewLimits("); n != 1 {
		t.Errorf("server.go builds %d api.Limits, want 1 (shared by the handlers and the actions)", n)
	}
	if !strings.Contains(string(src), "Limits: lim,") || !strings.Contains(string(src), "PasswordGate: lim.AccountPassword") {
		t.Error("the handlers and the password gate do not both use lim")
	}
}
