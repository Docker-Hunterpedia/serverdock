package store_test

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

func wantErr(t *testing.T, s *store.Store, what, q string, args ...any) {
	t.Helper()
	if _, err := s.W.Exec(q, args...); err == nil {
		t.Errorf("%s: the database accepted it", what)
	}
}

// seedService inserts one project and one service (ids 1) and returns the store.
func seedService(t *testing.T) *store.Store {
	t.Helper()
	s := openFresh(t)
	mustExec(t, s.W, `INSERT INTO projects(id, name, created_at) VALUES (1, 'demo', ?)`, ts)
	mustExec(t, s.W, `INSERT INTO services(id, project_id, name, image_ref, health_kind, health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, created_at, updated_at)
		VALUES (1, 1, 'web', 'nginx:1', 'running', 60, 3, 'blue_green', 10, 10, ?, ?)`, ts, ts)
	return s
}

func addEnvVersion(t *testing.T, s *store.Store, id, service, version int, ct []byte) {
	t.Helper()
	mustExec(t, s.W, `INSERT INTO service_env_versions(id, service_id, version, ciphertext, names_json, actor_kind, actor_id, created_at) VALUES (?, ?, ?, ?, '[]', 'user', '1', ?)`,
		id, service, version, ct, ts)
}

// Migration 0005 on a fresh database creates the tables, the columns and the indexes.
func TestFreshDatabaseHasTheServiceConfigSchema(t *testing.T) {
	s := openFresh(t)
	for _, tbl := range []string{"service_env_versions", "service_volumes", "registries"} {
		var n int
		if err := s.R.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?`, tbl).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s: %d %v", tbl, n, err)
		}
	}
	for _, col := range [][2]string{{"services", "env_version_id"}, {"services", "memory_mb"}, {"services", "cpus_milli"}, {"services", "no_new_privileges"},
		{"services", "last_oom_at"}, {"projects", "subnet"}} {
		var n int
		if err := s.R.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, col[0], col[1]).Scan(&n); err != nil || n != 1 {
			t.Errorf("column %s.%s: %d %v", col[0], col[1], n, err)
		}
	}
	var maxV int
	if err := s.R.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&maxV); err != nil || maxV < 5 {
		t.Errorf("max version %d %v", maxV, err)
	}
}

// A v4 database upgrades to v5 with one verified backup, keeps its rows and gets
// the new columns with their defaults.
func TestUpgradeFromV4AddsTheServiceConfigSchema(t *testing.T) {
	ctx := context.Background()
	dir := fixtureDir(t, "v4.db")
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	var memNull, nnp, envNull, subnetNull int
	if err := s.R.QueryRow(`SELECT memory_mb IS NULL, no_new_privileges, env_version_id IS NULL FROM services WHERE id = 1`).Scan(&memNull, &nnp, &envNull); err != nil ||
		memNull != 1 || nnp != 0 || envNull != 1 {
		t.Errorf("%d %d %d %v", memNull, nnp, envNull, err)
	}
	if err := s.R.QueryRow(`SELECT subnet IS NULL FROM projects WHERE id = 1`).Scan(&subnetNull); err != nil || subnetNull != 1 {
		t.Errorf("%d %v", subnetNull, err)
	}
	for tbl, want := range map[string]int{"projects": 1, "services": 1, "deploys": 1, "service_env_versions": 0, "service_volumes": 0, "registries": 0} {
		var n int
		if err := s.R.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows, want %d (%v)", tbl, n, want, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	bs, _ := filepath.Glob(filepath.Join(dir, "backups", "pre-migrate-*-v0004-v0005.db"))
	if len(bs) != 1 {
		t.Errorf("backups %v", bs)
	}
}

func TestProjectsSubnetUniqueOnlyWhenSet(t *testing.T) {
	s := openFresh(t)
	for i, n := range []string{"a", "b", "c"} {
		mustExec(t, s.W, `INSERT INTO projects(id, name, created_at) VALUES (?, ?, ?)`, i+1, n, ts) // subnet NULL, three times
	}
	mustExec(t, s.W, `UPDATE projects SET subnet = '10.250.0.0/25' WHERE id = 1`)
	wantErr(t, s, "a second project with the same subnet", `UPDATE projects SET subnet = '10.250.0.0/25' WHERE id = 2`)
	mustExec(t, s.W, `UPDATE projects SET subnet = '10.250.0.128/25' WHERE id = 2`)
	// The allocation statement of the plan: a project that already has one is not changed.
	r, err := s.W.Exec(`UPDATE projects SET subnet = '10.250.1.0/25' WHERE id = 1 AND subnet IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := r.RowsAffected(); n != 0 {
		t.Error("an allocated subnet was replaced")
	}
}

func TestEnvVersionsAreImmutable(t *testing.T) {
	s := seedService(t)
	addEnvVersion(t, s, 1, 1, 1, []byte("x"))
	for _, q := range []string{
		`UPDATE service_env_versions SET ciphertext = x'00' WHERE id = 1`,
		`UPDATE service_env_versions SET names_json = '[1]' WHERE id = 1`,
		`UPDATE service_env_versions SET version = 2 WHERE id = 1`,
		`UPDATE service_env_versions SET service_id = 1 WHERE id = 1`,
	} {
		wantErr(t, s, q, q)
	}
	wantErr(t, s, "version 0", `INSERT INTO service_env_versions(service_id, version, ciphertext, names_json, actor_kind, actor_id, created_at) VALUES (1, 0, x'01', '[]', 'user', '1', ?)`, ts)
	wantErr(t, s, "a repeated version number", `INSERT INTO service_env_versions(service_id, version, ciphertext, names_json, actor_kind, actor_id, created_at) VALUES (1, 1, x'01', '[]', 'user', '1', ?)`, ts)
	var ct []byte
	if err := s.R.QueryRow(`SELECT ciphertext FROM service_env_versions WHERE id = 1`).Scan(&ct); err != nil || string(ct) != "x" {
		t.Errorf("%q %v", ct, err)
	}
}

// The AAD "service_env_versions/<service_id>/v<version>" binds a ciphertext to its
// service and version: copied to another service or version, it does not open.
func TestEnvCiphertextOpensOnlyForItsServiceAndVersion(t *testing.T) {
	s := seedService(t)
	mustExec(t, s.W, `INSERT INTO services(id, project_id, name, image_ref, health_kind, health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, created_at, updated_at)
		VALUES (2, 1, 'api', 'nginx:1', 'running', 60, 3, 'blue_green', 10, 10, ?, ?)`, ts, ts)
	box, err := secretbox.New(fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	aad := func(service, version int) string {
		return "service_env_versions/" + strconv.Itoa(service) + "/v" + strconv.Itoa(version)
	}
	ct, err := box.Seal(aad(1, 1), []byte(fixtureEnvDoc))
	if err != nil {
		t.Fatal(err)
	}
	addEnvVersion(t, s, 1, 1, 1, ct)
	var got []byte
	if err := s.R.QueryRow(`SELECT ciphertext FROM service_env_versions WHERE service_id = 1 AND version = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if pt, err := box.Open(aad(1, 1), got); err != nil || string(pt) != fixtureEnvDoc {
		t.Fatalf("its own place: %v", err)
	}
	for _, other := range [][2]int{{2, 1}, {1, 2}, {2, 2}} {
		if _, err := box.Open(aad(other[0], other[1]), got); err == nil {
			t.Errorf("a ciphertext of service 1 v1 opened as service %d v%d", other[0], other[1])
		}
	}
	if _, err := box.Open("service_env_versions/1/v1/ciphertext", got); err == nil {
		t.Error("opened under a longer AAD")
	}
}

func TestRegistrySecretOpensOnlyForItsHost(t *testing.T) {
	s := openFresh(t)
	box, err := secretbox.New(fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := box.Seal("registries/a.example.test/secret", []byte(fixtureRegPass))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.W, `INSERT INTO registries(host, username, secret, created_at, updated_at) VALUES ('a.example.test', 'u', ?, ?, ?)`, ct, ts, ts)
	var got []byte
	if err := s.R.QueryRow(`SELECT secret FROM registries WHERE host = 'a.example.test'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if pt, err := box.Open("registries/a.example.test/secret", got); err != nil || string(pt) != fixtureRegPass {
		t.Fatalf("its own host: %v", err)
	}
	if _, err := box.Open("registries/b.example.test/secret", got); err == nil {
		t.Error("a credential of one host opened for another")
	}
	if _, err := box.Open("registries/1/secret", got); err == nil {
		t.Error("opened under a row-id AAD")
	}
	wantErr(t, s, "a second row for the same host", `INSERT INTO registries(host, created_at, updated_at) VALUES ('a.example.test', ?, ?)`, ts, ts)
	wantErr(t, s, "a username without a secret", `INSERT INTO registries(host, username, created_at, updated_at) VALUES ('b.example.test', 'u', ?, ?)`, ts, ts)
	wantErr(t, s, "a secret without a username", `INSERT INTO registries(host, secret, created_at, updated_at) VALUES ('c.example.test', x'01', ?, ?)`, ts, ts)
	wantErr(t, s, "allow_private 2", `INSERT INTO registries(host, allow_private, created_at, updated_at) VALUES ('d.example.test', 2, ?, ?)`, ts, ts)
	mustExec(t, s.W, `INSERT INTO registries(host, created_at, updated_at) VALUES ('anon.example.test', ?, ?)`, ts, ts)
}

func TestServiceVolumesChecksTieKindToNameOrPath(t *testing.T) {
	s := seedService(t)
	ins := `INSERT INTO service_volumes(service_id, kind, name, host_path, target, read_only, created_at) VALUES (1, ?, ?, ?, ?, ?, ?)`
	mustExec(t, s.W, ins, "volume", "data", nil, "/data", 0, ts)
	mustExec(t, s.W, ins, "bind", nil, "/srv/binds/x", "/files", 1, ts)
	wantErr(t, s, "a volume without a name", ins, "volume", nil, nil, "/a", 0, ts)
	wantErr(t, s, "a volume with a host path", ins, "volume", "n", "/srv/x", "/b", 0, ts)
	wantErr(t, s, "a bind without a path", ins, "bind", nil, nil, "/c", 0, ts)
	wantErr(t, s, "a bind with a name", ins, "bind", "n", "/srv/x", "/d", 0, ts)
	wantErr(t, s, "an unknown kind", ins, "tmpfs", "n", nil, "/e", 0, ts)
	wantErr(t, s, "read_only 2", ins, "volume", "ro", nil, "/f", 2, ts)
	wantErr(t, s, "a second mount at the same target", ins, "volume", "other", nil, "/data", 0, ts)
	wantErr(t, s, "the same volume name twice", ins, "volume", "data", nil, "/g", 0, ts)
	// A detached row frees its target but keeps its name (re-adding it reattaches).
	mustExec(t, s.W, `UPDATE service_volumes SET detached_at = ? WHERE name = 'data'`, ts)
	mustExec(t, s.W, ins, "volume", "scratch", nil, "/data", 0, ts)
	wantErr(t, s, "the name of a detached volume as a new row", ins, "volume", "data", nil, "/h", 0, ts)
}

func TestLimitsChecks(t *testing.T) {
	s := seedService(t)
	ok := func(set string) {
		t.Helper()
		mustExec(t, s.W, `UPDATE services SET `+set+` WHERE id = 1`)
	}
	bad := func(set string) {
		t.Helper()
		wantErr(t, s, set, `UPDATE services SET `+set+` WHERE id = 1`)
	}
	ok(`memory_mb = 32`)
	ok(`memory_mb = 4096`)
	ok(`memory_mb = NULL`)
	bad(`memory_mb = 31`)
	bad(`memory_mb = 0`)
	bad(`memory_mb = -1`)
	ok(`cpus_milli = 50`)
	ok(`cpus_milli = 1500`)
	ok(`cpus_milli = NULL`)
	bad(`cpus_milli = 0`)
	bad(`cpus_milli = 49`)
	bad(`cpus_milli = 75`)
	bad(`cpus_milli = 1001`)
	ok(`no_new_privileges = 1`)
	ok(`no_new_privileges = 0`)
	bad(`no_new_privileges = 2`)
	bad(`no_new_privileges = NULL`)
}

// The current version is protected (ON DELETE RESTRICT), so pruning cannot take it.
func TestCurrentEnvVersionCannotBeDeleted(t *testing.T) {
	s := seedService(t)
	addEnvVersion(t, s, 1, 1, 1, []byte("a"))
	addEnvVersion(t, s, 2, 1, 2, []byte("b"))
	mustExec(t, s.W, `UPDATE services SET env_version_id = 2 WHERE id = 1`)
	wantErr(t, s, "deleting the current version", `DELETE FROM service_env_versions WHERE id = 2`)
	wantErr(t, s, "deleting all versions of the service", `DELETE FROM service_env_versions WHERE service_id = 1`)
	mustExec(t, s.W, `DELETE FROM service_env_versions WHERE id = 1`) // an older one can go
	var n int
	if err := s.R.QueryRow(`SELECT count(*) FROM service_env_versions`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d %v", n, err)
	}
	wantErr(t, s, "pointing at another service's version", `UPDATE services SET env_version_id = 999 WHERE id = 1`)
}

// Deleting a service removes its versions even though services.env_version_id
// restricts deleting the current one on its own.
func TestDeletingAServiceWithEnvVersionsSucceeds(t *testing.T) {
	s := seedService(t)
	addEnvVersion(t, s, 1, 1, 1, []byte("a"))
	addEnvVersion(t, s, 2, 1, 2, []byte("b"))
	mustExec(t, s.W, `UPDATE services SET env_version_id = 2 WHERE id = 1`)
	mustExec(t, s.W, `INSERT INTO service_volumes(service_id, kind, name, target, created_at) VALUES (1, 'volume', 'data', '/data', ?)`, ts)
	mustExec(t, s.W, `DELETE FROM services WHERE id = 1`)
	for _, tbl := range []string{"services", "service_env_versions", "service_volumes"} {
		var n int
		if err := s.R.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows left (%v)", tbl, n, err)
		}
	}
}

func TestAppCiphertextColumnsAreListed(t *testing.T) {
	var got []string
	for _, c := range secretbox.AppColumns() {
		got = append(got, c.Table+"."+c.Column)
	}
	if strings.Join(got, ",") != "service_env_versions.ciphertext,registries.secret" {
		t.Errorf("%v", got)
	}
	all := map[string]bool{}
	for _, c := range secretbox.Columns() {
		all[c.Table+"."+c.Column] = true
	}
	for _, g := range got {
		if !all[g] {
			t.Errorf("%s is missing from Columns()", g)
		}
	}
}
