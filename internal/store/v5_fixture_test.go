package store_test

import (
	"context"
	"database/sql"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

var updateV5 = flag.Bool("update-v5", false, "regenerate testdata/v5.db from testdata/v4.db (a new migration never edits it: add v6.db instead)")

const (
	fixtureEnvDoc  = `{"v":1,"vars":[{"name":"GREETING","value":"hello","secret":false},{"name":"API_KEY","value":"fixture-not-a-real-secret","secret":true}]}`
	fixtureRegPass = "fixture-registry-password"
)

// TestGenerateV5Fixture rebuilds testdata/v5.db the way v4.db was made: it opens a
// copy of the frozen previous fixture (v4.db) with the real migration list, so
// migration 0005 runs, adds the rows below with plain SQL, and writes one clean
// file with VACUUM INTO. Run it with -update-v5 only to create the file; the file
// is then frozen. The rows: project "demo" with a subnet, service "web" with env
// version 1 (the current version; no deploy uses it), memory and CPU limits and no-new-privileges,
// one named volume and one bind mount, and one registry with a sealed password.
// The two ciphertexts are sealed under the fixed test key of v3.db (a made-up key,
// which is also the key id recorded in meta).
func TestGenerateV5Fixture(t *testing.T) {
	if !*updateV5 {
		t.Skip("pass -update-v5 to regenerate testdata/v5.db")
	}
	ctx := context.Background()
	dir := fixtureDir(t, "v4.db")
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	env, err := box.Seal("service_env_versions/1/v1", []byte(fixtureEnvDoc))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := box.Seal("registries/registry.example.test/secret", []byte(fixtureRegPass))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`UPDATE meta SET value = 'v5' WHERE key = 'fixture'`)
	exec(`UPDATE projects SET subnet = '10.250.0.0/25' WHERE id = 1`)
	exec(`INSERT INTO service_env_versions(id, service_id, version, ciphertext, names_json, actor_kind, actor_id, created_at)
		VALUES (1, 1, 1, ?, '[{"name":"GREETING","secret":false},{"name":"API_KEY","secret":true}]', 'user', '1', ?)`, env, ts)
	exec(`UPDATE services SET env_version_id = 1, memory_mb = 256, cpus_milli = 500, no_new_privileges = 1 WHERE id = 1`)
	exec(`INSERT INTO service_volumes(service_id, kind, name, target, read_only, created_at) VALUES (1, 'volume', 'data', '/data', 0, ?)`, ts)
	exec(`INSERT INTO service_volumes(service_id, kind, host_path, target, read_only, created_at) VALUES (1, 'bind', '/srv/binds/web', '/files', 1, ?)`, ts)
	exec(`INSERT INTO registries(host, username, secret, allow_private, created_at, updated_at) VALUES ('registry.example.test', 'ci', ?, 0, ?, ?)`, reg, ts, ts)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := filepath.Join("testdata", "v5.db")
	_ = os.Remove(out)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, out); err != nil {
		t.Fatal(err)
	}
}

// testdata/v5.db is a database as the end of M3b step 1 leaves a configured
// server. It must keep opening with the real migration list, every row must
// survive later migrations, and its two app ciphertexts must open with the fixture
// key under their own AAD.
func TestV5FixtureOpensAndKeepsItsRows(t *testing.T) {
	ctx := context.Background()
	dir := fixtureDir(t, "v5.db")
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("the v5 fixture no longer opens with the builtin migrations: %v", err)
	}
	defer s.Close()
	var maxV int
	if err := s.R.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&maxV); err != nil || maxV < 5 {
		t.Fatalf("max version %d %v", maxV, err)
	}
	for tbl, want := range map[string]int{"projects": 1, "services": 1, "service_env_versions": 1, "service_volumes": 2, "registries": 1, "deploys": 1} {
		var n int
		if err := s.R.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows, want %d (%v)", tbl, n, want, err)
		}
	}
	var subnet string
	var mem, cpus, nnp, envID int
	if err := s.R.QueryRow(`SELECT p.subnet, s.memory_mb, s.cpus_milli, s.no_new_privileges, s.env_version_id FROM services s JOIN projects p ON p.id = s.project_id`).
		Scan(&subnet, &mem, &cpus, &nnp, &envID); err != nil || subnet != "10.250.0.0/25" || mem != 256 || cpus != 500 || nnp != 1 || envID != 1 {
		t.Errorf("%q %d %d %d %d %v", subnet, mem, cpus, nnp, envID, err)
	}
	box, err := secretbox.New(fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	var env, reg []byte
	if err := s.R.QueryRow(`SELECT ciphertext FROM service_env_versions WHERE id = 1`).Scan(&env); err != nil {
		t.Fatal(err)
	}
	if pt, err := box.Open("service_env_versions/1/v1", env); err != nil || string(pt) != fixtureEnvDoc {
		t.Errorf("env ciphertext: %v", err)
	}
	if err := s.R.QueryRow(`SELECT secret FROM registries WHERE host = 'registry.example.test'`).Scan(&reg); err != nil {
		t.Fatal(err)
	}
	if pt, err := box.Open("registries/registry.example.test/secret", reg); err != nil || string(pt) != fixtureRegPass {
		t.Errorf("registry secret: %v", err)
	}
	if ok, err := secretbox.CiphertextsExist(ctx, s.R, secretbox.AppColumns()); err != nil || !ok {
		t.Errorf("AppColumns do not see the fixture's app ciphertexts: %v %v", ok, err)
	}
	assertEnvTriggers(t, s)
	if _, err := s.W.ExecContext(ctx, `UPDATE service_env_versions SET names_json = '[]' WHERE id = 1`); err == nil {
		t.Error("the env version trigger is gone")
	}
	ro, err := store.OpenReadOnly(dir, store.ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if res, err := audit.Verify(ctx, ro.DB); err != nil || !res.OK {
		t.Errorf("audit chain: %+v %v", res, err)
	}
}
