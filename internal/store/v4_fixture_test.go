package store_test

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

var updateV4 = flag.Bool("update-v4", false, "regenerate testdata/v4.db from testdata/v3.db (a new migration never edits it: add v5.db instead)")

const fixtureInstallID = "0123456789abcdef0123456789abcdef"

// TestGenerateV4Fixture rebuilds testdata/v4.db: the v3 database after migration
// 0004, with one project, one service with an active deploy and its stages, one
// finished job, the app subnet and a fixed install id. Run it with -update-v4
// only to create the file; the file is then frozen.
func TestGenerateV4Fixture(t *testing.T) {
	if !*updateV4 {
		t.Skip("pass -update-v4 to regenerate testdata/v4.db")
	}
	ctx := context.Background()
	dir := fixtureDir(t, "v3.db")
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`UPDATE meta SET value = 'v4' WHERE key = 'fixture'`)
	exec(`UPDATE meta SET value = ? WHERE key = 'install_id'`, fixtureInstallID)
	exec(`INSERT INTO meta(key, value) VALUES ('app_subnet', '10.250.0.0/24')`)
	exec(`INSERT INTO projects(id, name, created_at) VALUES (1, 'demo', ?)`, ts)
	exec(`INSERT INTO jobs(id, kind, lock_key, state, actor_kind, actor_id, actor_blob, enqueued_at, payload_json, run_after, timeout_s, request_id, created_at, started_at, finished_at)
		VALUES (1, 'deploy', 'service:1', 'succeeded', 'user', '1', '{}', ?, '{"deploy":1}', ?, 400, 'job-1-a1', ?, ?, ?)`, ts, ts, ts, ts, ts)
	exec(`INSERT INTO services(id, project_id, name, image_ref, container_port, public_port, health_kind, health_path, health_timeout_s, health_passes,
		strategy, drain_s, stop_grace_s, created_at, updated_at) VALUES (1, 1, 'web', 'nginx:1.27', 80, 8080, 'http', '/', 60, 3, 'blue_green', 10, 10, ?, ?)`, ts, ts)
	exec(`INSERT INTO deploys(id, service_id, number, job_id, kind, snapshot_json, ip, image_id, status, stage, committed_at, actor_kind, actor_id, created_at, started_at, finished_at)
		VALUES (1, 1, 1, 1, 'deploy', '{"v":1,"image_ref":"nginx:1.27"}', '10.250.0.2', 'sha256:`+fmt.Sprintf("%064x", 1)+`', 'succeeded', 'done', ?, 'user', '1', ?, ?, ?)`, ts, ts, ts, ts)
	exec(`UPDATE services SET active_deploy_id = 1 WHERE id = 1`)
	exec(`INSERT INTO deploy_stages(deploy_id, stage, state, started_at, finished_at) VALUES (1, 'pull', 'ok', ?, ?), (1, 'health', 'ok', ?, ?)`, ts, ts, ts, ts)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := filepath.Join("testdata", "v4.db")
	_ = os.Remove(out)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, out); err != nil {
		t.Fatal(err)
	}
}

// testdata/v4.db is a database as the end of M3a step 4a leaves a server with one
// deployed service. It must keep opening with the real migration list and every
// row must survive whatever migrations come after 0004.
func TestV4FixtureOpensAndKeepsItsRows(t *testing.T) {
	ctx := context.Background()
	dir := fixtureDir(t, "v4.db")
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("the v4 fixture no longer opens with the builtin migrations: %v", err)
	}
	defer s.Close()
	var maxV int
	if err := s.R.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&maxV); err != nil || maxV < 4 {
		t.Fatalf("max version %d %v", maxV, err)
	}
	get := func(q string) string {
		t.Helper()
		var v string
		if err := s.R.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return v
	}
	if v := get(`SELECT value FROM meta WHERE key = 'fixture'`); v != "v4" {
		t.Errorf("fixture row %q", v)
	}
	if v := get(`SELECT value FROM meta WHERE key = 'install_id'`); v != fixtureInstallID {
		t.Errorf("install_id %q changed", v)
	}
	if v := get(`SELECT value FROM meta WHERE key = 'app_subnet'`); v != "10.250.0.0/24" {
		t.Errorf("app_subnet %q", v)
	}
	for tbl, want := range map[string]int{"projects": 1, "services": 1, "deploys": 1, "jobs": 1, "deploy_stages": 2, "users": 1, "api_tokens": 1} {
		var n int
		if err := s.R.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows, want %d (%v)", tbl, n, want, err)
		}
	}
	if v := get(`SELECT active_deploy_id FROM services WHERE id = 1`); v != "1" {
		t.Errorf("active_deploy_id %q", v)
	}
	if v := get(`SELECT job_id FROM deploys WHERE id = 1`); v != "1" {
		t.Errorf("deploys.job_id %q", v)
	}
	if _, err := s.W.ExecContext(ctx, `UPDATE deploys SET snapshot_json = '{}' WHERE id = 1`); err == nil {
		t.Error("the snapshot trigger is gone")
	}
	if _, err := s.W.ExecContext(ctx, `DELETE FROM audit_log WHERE id = 1`); err == nil {
		t.Error("audit_log lost its append-only trigger")
	}
	ro, err := store.OpenReadOnly(dir, store.ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if res, err := audit.Verify(ctx, ro.DB); err != nil || !res.OK || res.Rows != 5 {
		t.Errorf("audit chain: %+v %v", res, err)
	}
}

// A v3 database upgrades to v4 with exactly one verified backup of v3 and gets an
// install id; the audit chain and the admin survive.
func TestUpgradeFromV3AddsTheDeployTablesAndAnInstallID(t *testing.T) {
	ctx := context.Background()
	dir := fixtureDir(t, "v3.db")
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := s.R.QueryRow(`SELECT value FROM meta WHERE key = 'install_id'`).Scan(&id); err != nil || len(id) != 32 {
		t.Fatalf("install_id %q %v", id, err)
	}
	for _, tbl := range []string{"projects", "services", "deploys", "deploy_stages", "jobs"} {
		var n int
		if err := s.R.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d %v", tbl, n, err)
		}
	}
	var users int
	s.R.QueryRow(`SELECT count(*) FROM users`).Scan(&users)
	if users != 1 {
		t.Errorf("users %d", users)
	}
	s.Close()
	bs, _ := filepath.Glob(filepath.Join(dir, "backups", "pre-migrate-*-v0003-v*.db"))
	if len(bs) != 1 {
		t.Errorf("backups %v", bs)
	}
}
