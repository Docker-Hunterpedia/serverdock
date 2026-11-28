package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

const (
	appEnvDoc  = `{"v":1,"vars":[{"name":"PLAIN","value":"plain-sentinel-value","secret":false},{"name":"TOKEN","value":"secret-sentinel-value","secret":true}]}`
	appRegPass = "registry-sentinel-password"
	appTS      = "2026-11-28T10:00:00.000Z"
)

// seedAppSecrets adds, to a data directory that has completed first run, one
// service with two env versions (the second current) and one registry credential,
// both sealed under the directory's own master key.
func seedAppSecrets(t *testing.T, data string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key, err := os.ReadFile(secretbox.KeyPath(data))
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO projects(id, name, created_at) VALUES (1, 'demo', ?)`, appTS)
	exec(`INSERT INTO services(id, project_id, name, image_ref, health_kind, health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, created_at, updated_at)
		VALUES (1, 1, 'web', 'nginx:1', 'running', 60, 3, 'blue_green', 10, 10, ?, ?)`, appTS, appTS)
	for v := 1; v <= 2; v++ {
		ct, err := box.Seal("service_env_versions/1/v"+string(rune('0'+v)), []byte(appEnvDoc))
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO service_env_versions(id, service_id, version, ciphertext, names_json, actor_kind, actor_id, created_at) VALUES (?, 1, ?, ?, '[]', 'user', '1', ?)`, v, v, ct, appTS)
	}
	exec(`UPDATE services SET env_version_id = 2 WHERE id = 1`)
	rc, err := box.Seal("registries/registry.example.test/secret", []byte(appRegPass))
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO registries(host, username, secret, created_at, updated_at) VALUES ('registry.example.test', 'ci', ?, ?, ?)`, rc, appTS, appTS)
}

func appCiphertexts(t *testing.T, data string) string {
	t.Helper()
	ro, err := store.OpenReadOnly(data, store.ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var sb strings.Builder
	rows, err := ro.DB.Query(`SELECT hex(ciphertext) FROM service_env_versions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var h string
		rows.Scan(&h)
		sb.WriteString(h)
	}
	rows.Close()
	var r string
	ro.DB.QueryRow(`SELECT hex(secret) FROM registries`).Scan(&r)
	return sb.String() + "|" + r
}

func dbString(t *testing.T, data, q string) string {
	t.Helper()
	ro, err := store.OpenReadOnly(data, store.ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var s *string
	if err := ro.DB.QueryRow(q).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return "<null>"
	}
	return *s
}

func noSecretsIn(t *testing.T, outs ...string) {
	t.Helper()
	for _, o := range outs {
		for _, s := range []string{"plain-sentinel-value", "secret-sentinel-value", appRegPass} {
			if strings.Contains(o, s) {
				t.Errorf("the output contains %q", s)
			}
		}
	}
}

// With app ciphertexts and no key file, a start is refused and creates no key, both
// when the database records the key id and when it does not (a restored database).
func TestMissingKeyWithAppSecretsRefusesStart(t *testing.T) {
	for _, forgetRecordedID := range []bool{false, true} {
		a := seedAdmin(t)
		seedAppSecrets(t, a.data)
		if forgetRecordedID {
			st, err := store.Open(context.Background(), a.data)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.W.Exec(`DELETE FROM meta WHERE key = ?`, secretbox.KeyIDMetaKey); err != nil {
				t.Fatal(err)
			}
			st.Close()
		}
		if err := os.Remove(secretbox.KeyPath(a.data)); err != nil {
			t.Fatal(err)
		}
		err := runServer(context.Background(), serverConfig{Data: a.data, ControlSocket: shortSock(t)}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		if err == nil || !strings.Contains(err.Error(), "is missing") || !strings.Contains(err.Error(), "--forget-app-secrets") {
			t.Fatalf("recorded id removed=%v: %v", forgetRecordedID, err)
		}
		if _, err := os.Stat(secretbox.KeyPath(a.data)); err == nil {
			t.Error("a new key was created next to app ciphertexts")
		}
		noSecretsIn(t, err.Error())
	}
}

func TestAdminResetRefusesWithAppSecretsAndNoKey(t *testing.T) {
	a := seedAdmin(t)
	seedAppSecrets(t, a.data)
	before := appCiphertexts(t, a.data)
	recorded := dbString(t, a.data, `SELECT value FROM meta WHERE key = 'secretbox_key_id'`)
	if err := os.Remove(secretbox.KeyPath(a.data)); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runReset(t, a.data, "--yes")
	if code != 1 || !strings.Contains(errOut, "--forget-app-secrets") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(secretbox.KeyPath(a.data)); err == nil {
		t.Error("a key was created")
	}
	if appCiphertexts(t, a.data) != before {
		t.Error("app ciphertexts changed")
	}
	if dbCount(t, a.data, `SELECT count(*) FROM sessions`) != 1 || dbCount(t, a.data, `SELECT count(*) FROM api_tokens WHERE revoked_at IS NULL`) != 2 {
		t.Error("the admin state was cleared although the reset was refused")
	}
	if got := dbString(t, a.data, `SELECT value FROM meta WHERE key = 'secretbox_key_id'`); got != recorded {
		t.Errorf("the recorded key id changed: %q -> %q", recorded, got)
	}
	noSecretsIn(t, out, errOut)
}

func TestAdminResetRefusesWithAppSecretsAndAWrongKey(t *testing.T) {
	a := seedAdmin(t)
	seedAppSecrets(t, a.data)
	before := appCiphertexts(t, a.data)
	recorded := dbString(t, a.data, `SELECT value FROM meta WHERE key = 'secretbox_key_id'`)
	other := make([]byte, 32)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretbox.KeyPath(a.data), other, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runReset(t, a.data, "--yes")
	if code != 1 || !strings.Contains(errOut, "--forget-app-secrets") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got, _ := os.ReadFile(secretbox.KeyPath(a.data)); !bytes.Equal(got, other) {
		t.Error("the key file was replaced")
	}
	if got := dbString(t, a.data, `SELECT value FROM meta WHERE key = 'secretbox_key_id'`); got != recorded {
		t.Errorf("the wrong key was adopted: recorded id %q -> %q", recorded, got)
	}
	if appCiphertexts(t, a.data) != before || dbCount(t, a.data, `SELECT count(*) FROM sessions`) != 1 {
		t.Error("something changed although the reset was refused")
	}
	noSecretsIn(t, out, errOut)
}

func TestAdminResetForgetAppSecretsListsThenDropsAndFlagsEnvLost(t *testing.T) {
	a := seedAdmin(t)
	seedAppSecrets(t, a.data)
	if err := os.Remove(secretbox.KeyPath(a.data)); err != nil {
		t.Fatal(err)
	}
	// Declining the second question changes nothing.
	var o, e bytes.Buffer
	if code := adminReset([]string{"reset", "--data", a.data, "--forget-app-secrets"}, strings.NewReader("reset\nno\n"), true, os.Geteuid(), &o, &e); code != 1 {
		t.Fatalf("declined: exit %d\n%s%s", code, o.String(), e.String())
	}
	if dbCount(t, a.data, `SELECT count(*) FROM service_env_versions`) != 2 || dbCount(t, a.data, `SELECT count(*) FROM sessions`) != 1 {
		t.Fatal("something was deleted although the second question was declined")
	}
	if !strings.Contains(o.String(), "demo/web") {
		t.Errorf("the list was not shown before the question:\n%s", o.String())
	}
	// Confirming lists, then deletes.
	o.Reset()
	e.Reset()
	if code := adminReset([]string{"reset", "--data", a.data, "--forget-app-secrets"}, strings.NewReader("reset\nforget\n"), true, os.Geteuid(), &o, &e); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, o.String(), e.String())
	}
	out := o.String()
	li, di := strings.Index(out, "demo/web"), strings.Index(out, "Reset done")
	if li < 0 || di < 0 || li > di {
		t.Errorf("the list must come before the result:\n%s", out)
	}
	for _, want := range []string{"2 environment version(s)", "plain values included", "registry credential for registry.example.test", "1 service(s)", "1 registry credential(s) cleared", "a new master key created"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	noSecretsIn(t, out, e.String())
	if n := dbCount(t, a.data, `SELECT count(*) FROM service_env_versions`); n != 0 {
		t.Errorf("%d env versions left", n)
	}
	if got := dbString(t, a.data, `SELECT COALESCE(CAST(env_version_id AS TEXT), '<null>') FROM services WHERE id = 1`); got != "<null>" {
		t.Errorf("env_version_id %s", got)
	}
	if got := dbString(t, a.data, `SELECT CAST(env_lost AS TEXT) || '/' || COALESCE(needs_attention, '<null>') FROM services WHERE id = 1`); got != "1/<null>" {
		t.Errorf("env_lost/needs_attention %q", got)
	}
	if got := dbString(t, a.data, `SELECT host || '/' || COALESCE(username, '<null>') || '/' || CAST(secret IS NULL AS TEXT) FROM registries`); got != "registry.example.test/<null>/1" {
		t.Errorf("registry row %q", got)
	}
	// The reset itself went through: the admin state is cleared and a new key matches the recorded id.
	if dbCount(t, a.data, `SELECT count(*) FROM sessions`) != 0 {
		t.Error("sessions left")
	}
	key, err := os.ReadFile(secretbox.KeyPath(a.data))
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secretbox.New(key)
	if got := dbString(t, a.data, `SELECT value FROM meta WHERE key = 'secretbox_key_id'`); got != box.KeyID() {
		t.Errorf("recorded key id %q, new key %q", got, box.KeyID())
	}
	// The service, its project and the audit rows survive.
	if dbCount(t, a.data, `SELECT count(*) FROM services`) != 1 {
		t.Error("the service row is gone")
	}
	if dbCount(t, a.data, `SELECT count(*) FROM audit_log WHERE action = 'auth.admin_reset'`) != 2 {
		t.Error("the intent and outcome rows are missing")
	}
	// A second reset has nothing to forget and succeeds without the flag.
	if code, out, errOut := runReset(t, a.data, "--yes"); code != 0 {
		t.Fatalf("second reset: exit %d\n%s%s", code, out, errOut)
	}
}

func TestAdminResetWithTheKeyNeverTouchesAppSecrets(t *testing.T) {
	for _, extra := range [][]string{{"--yes"}, {"--yes", "--forget-app-secrets"}} {
		a := seedAdmin(t)
		seedAppSecrets(t, a.data)
		before := appCiphertexts(t, a.data)
		key, _ := os.ReadFile(secretbox.KeyPath(a.data))
		code, out, errOut := runReset(t, a.data, extra...)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s%s", extra, code, out, errOut)
		}
		if appCiphertexts(t, a.data) != before {
			t.Errorf("%v: app ciphertexts changed", extra)
		}
		if got := dbString(t, a.data, `SELECT COALESCE(CAST(env_version_id AS TEXT), '<null>') || '/' || env_lost || '/' || COALESCE(needs_attention, '<null>') FROM services WHERE id = 1`); got != "2/0/<null>" {
			t.Errorf("%v: service row %q", extra, got)
		}
		if got := dbString(t, a.data, `SELECT username FROM registries`); got != "ci" {
			t.Errorf("%v: registry username %q", extra, got)
		}
		if after, _ := os.ReadFile(secretbox.KeyPath(a.data)); !bytes.Equal(after, key) {
			t.Errorf("%v: the key changed", extra)
		}
		if !strings.Contains(out, "app secrets kept") {
			t.Errorf("%v: the output does not say the app secrets were kept:\n%s", extra, out)
		}
		if dbCount(t, a.data, `SELECT count(*) FROM sessions`) != 0 {
			t.Errorf("%v: the admin state was not cleared", extra)
		}
		noSecretsIn(t, out, errOut)
	}
}

// env_lost is a column of its own: a forget never touches needs_attention (versions_kept
// and previous_version_down are safety facts; delete_confirm_again is a hint every
// enqueue clears), and env_lost survives that enqueue.
func TestForgetAppSecretsKeepsNeedsAttentionAndSetsEnvLost(t *testing.T) {
	a := seedAdmin(t)
	seedAppSecrets(t, a.data)
	ctx := context.Background()
	st, err := store.Open(ctx, a.data)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(secretbox.KeyPath(a.data))
	box, _ := secretbox.New(key)
	for id, name := range map[int]string{2: "api", 3: "db"} {
		if _, err := st.W.Exec(`INSERT INTO services(id, project_id, name, image_ref, health_kind, health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, created_at, updated_at)
			VALUES (?, 1, ?, 'nginx:1', 'running', 60, 3, 'blue_green', 10, 10, ?, ?)`, id, name, appTS, appTS); err != nil {
			t.Fatal(err)
		}
		ct, _ := box.Seal("service_env_versions/"+string(rune('0'+id))+"/v1", []byte(appEnvDoc))
		if _, err := st.W.Exec(`INSERT INTO service_env_versions(id, service_id, version, ciphertext, names_json, actor_kind, actor_id, created_at) VALUES (?, ?, 1, ?, '[]', 'user', '1', ?)`, 10+id, id, ct, appTS); err != nil {
			t.Fatal(err)
		}
		if _, err := st.W.Exec(`UPDATE services SET env_version_id = ? WHERE id = ?`, 10+id, id); err != nil {
			t.Fatal(err)
		}
	}
	for id, flag := range map[int]string{1: "versions_kept", 2: "previous_version_down", 3: "delete_confirm_again"} {
		if _, err := st.W.Exec(`UPDATE services SET needs_attention = ? WHERE id = ?`, flag, id); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	os.Remove(secretbox.KeyPath(a.data))
	if code, out, errOut := runReset(t, a.data, "--yes", "--forget-app-secrets"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for id, flag := range map[int]string{1: "versions_kept", 2: "previous_version_down", 3: "delete_confirm_again"} {
		got := dbString(t, a.data, `SELECT CAST(env_lost AS TEXT) || '/' || COALESCE(needs_attention, '<null>') || '/' || COALESCE(CAST(env_version_id AS TEXT), '<null>') FROM services WHERE id = `+string(rune('0'+id)))
		if got != "1/"+flag+"/<null>" {
			t.Errorf("service %d: %q", id, got)
		}
	}
	// An enqueue clears delete_confirm_again, and env_lost stays.
	st, err = store.Open(ctx, a.data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := jobs.NewStore(st, nil).Enqueue(ctx, jobs.NewJob{Kind: jobs.KindRestart, LockKey: jobs.LockKey(3),
		Actor: jobs.Actor{Kind: "user", ID: "1", Blob: `{"k":"user"}`}, Payload: []byte(`{}`), TimeoutS: 60}); err != nil {
		t.Fatal(err)
	}
	var att *string
	var lost int
	if err := st.R.QueryRow(`SELECT needs_attention, env_lost FROM services WHERE id = 3`).Scan(&att, &lost); err != nil || att != nil || lost != 1 {
		t.Errorf("after the enqueue: %v %d %v", att, lost, err)
	}
}

// No recorded key id, app ciphertexts and a different key: refused at start and in
// the reset, and the wrong key is not adopted.
func TestNoRecordedKeyIDAndAWrongKeyIsRefused(t *testing.T) {
	a := seedAdmin(t)
	seedAppSecrets(t, a.data)
	st, err := store.Open(context.Background(), a.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.W.Exec(`DELETE FROM meta WHERE key = ?`, secretbox.KeyIDMetaKey); err != nil {
		t.Fatal(err)
	}
	st.Close()
	other := make([]byte, 32)
	rand.Read(other)
	if err := os.WriteFile(secretbox.KeyPath(a.data), other, 0o600); err != nil {
		t.Fatal(err)
	}
	before := appCiphertexts(t, a.data)
	err = runServer(context.Background(), serverConfig{Data: a.data, ControlSocket: shortSock(t)}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("start: %v", err)
	}
	code, out, errOut := runReset(t, a.data, "--yes")
	if code != 1 || !strings.Contains(errOut, "--forget-app-secrets") {
		t.Fatalf("reset: exit %d\n%s%s", code, out, errOut)
	}
	if got := dbCount(t, a.data, `SELECT count(*) FROM meta WHERE key = 'secretbox_key_id'`); got != 0 {
		t.Error("the wrong key's id was recorded")
	}
	if appCiphertexts(t, a.data) != before || dbCount(t, a.data, `SELECT count(*) FROM sessions`) != 1 {
		t.Error("something changed")
	}
}

// The reset with the right key keeps the recorded id (the key is not changing).
func TestResetWithTheKeyKeepsTheRecordedKeyID(t *testing.T) {
	a := seedAdmin(t)
	seedAppSecrets(t, a.data)
	want := dbString(t, a.data, `SELECT value FROM meta WHERE key = 'secretbox_key_id'`)
	if code, out, errOut := runReset(t, a.data, "--yes"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := dbString(t, a.data, `SELECT value FROM meta WHERE key = 'secretbox_key_id'`); got != want {
		t.Errorf("%q -> %q", want, got)
	}
}

// A key file that cannot be used (mode 0644, wrong size, a symlink) with app
// ciphertexts: the reset is refused with and without the flag, the start is refused,
// and the key file, ciphertexts, sessions and recorded id stay as they were.
func TestUnusableKeyWithAppSecretsRefusesResetAndStart(t *testing.T) {
	good := make([]byte, 32)
	rand.Read(good)
	cases := map[string]func(t *testing.T, path string){
		"mode 0644": func(t *testing.T, path string) {
			os.Remove(path)
			if err := os.WriteFile(path, good, 0o644); err != nil {
				t.Fatal(err)
			}
			os.Chmod(path, 0o644)
		},
		"wrong size": func(t *testing.T, path string) {
			os.Remove(path)
			if err := os.WriteFile(path, good[:10], 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, path string) {
			target := path + ".real"
			os.Remove(path)
			if err := os.WriteFile(target, good, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			a := seedAdmin(t)
			seedAppSecrets(t, a.data)
			path := secretbox.KeyPath(a.data)
			mk(t, path)
			before := appCiphertexts(t, a.data)
			recorded := dbString(t, a.data, `SELECT value FROM meta WHERE key = 'secretbox_key_id'`)
			lbefore, lerr := os.Lstat(path)
			bytesBefore, _ := os.ReadFile(path)
			for _, extra := range [][]string{{"--yes"}, {"--yes", "--forget-app-secrets"}} {
				code, out, errOut := runReset(t, a.data, extra...)
				if code != 1 || !strings.Contains(errOut, "key file cannot be used") {
					t.Fatalf("%v: exit %d\n%s%s", extra, code, out, errOut)
				}
			}
			err := runServer(context.Background(), serverConfig{Data: a.data, ControlSocket: shortSock(t)}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if err == nil {
				t.Fatal("the server started")
			}
			lafter, aerr := os.Lstat(path)
			bytesAfter, _ := os.ReadFile(path)
			if lerr != nil || aerr != nil || lafter.Mode() != lbefore.Mode() || !bytes.Equal(bytesBefore, bytesAfter) {
				t.Error("the key file changed")
			}
			if appCiphertexts(t, a.data) != before || dbCount(t, a.data, `SELECT count(*) FROM sessions`) != 1 ||
				dbString(t, a.data, `SELECT value FROM meta WHERE key = 'secretbox_key_id'`) != recorded {
				t.Error("the database changed")
			}
		})
	}
}
