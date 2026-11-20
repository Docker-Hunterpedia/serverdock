package store_test

import (
	"context"
	"database/sql"
	"flag"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

var updateV3 = flag.Bool("update-v3", false, "regenerate testdata/v3.db from testdata/v2.db (a new migration never edits it: add v4.db instead)")

const dbFile = "serverdock.db"

func fixtureDir(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dbFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestGenerateV3Fixture rebuilds testdata/v3.db: the v2 database (an audit chain
// and a meta row) after migration 0003, as the end of M2 leaves it. Run it with
// -update-v3 only to create the file; the file is then frozen.
func TestGenerateV3Fixture(t *testing.T) {
	if !*updateV3 {
		t.Skip("pass -update-v3 to regenerate testdata/v3.db")
	}
	ctx := context.Background()
	dir := fixtureDir(t, "v2.db")
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.W.ExecContext(ctx, `UPDATE meta SET value = 'v3' WHERE key = 'fixture'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A single clean file: VACUUM INTO leaves no WAL behind.
	db, err := sql.Open("sqlite", filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := filepath.Join("testdata", "v3.db")
	_ = os.Remove(out)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, out); err != nil {
		t.Fatal(err)
	}
}

// testdata/v3.db is a database as the end of M2 leaves it (migrations 0001 to
// 0003, the auth tables empty, the M1 audit chain, a meta row). It must keep
// opening with the real migration list, with nothing pending, and its audit chain
// must still verify: a 0004 migration builds on this file, and editing an applied
// migration changes its checksum and fails here.
func TestV3FixtureOpensMigratesAndTheAuditChainVerifies(t *testing.T) {
	ctx := context.Background()
	dir := fixtureDir(t, "v3.db")
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatalf("the v3 fixture no longer opens with the builtin migrations: %v", err)
	}
	var versions []int
	rows, err := s.R.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v int
		rows.Scan(&v)
		versions = append(versions, v)
	}
	rows.Close()
	if len(versions) < 3 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 {
		t.Fatalf("versions %v", versions)
	}
	var v string
	if err := s.R.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'fixture'`).Scan(&v); err != nil || v != "v3" {
		t.Fatalf("meta fixture row: %q %v", v, err)
	}
	for _, tbl := range []string{"users", "recovery_codes", "sessions", "auth_pending", "auth_known_ips", "api_tokens", "setup_state"} {
		var n int
		if err := s.R.QueryRowContext(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d %v", tbl, n, err)
		}
	}
	if _, err := s.W.ExecContext(ctx, `DELETE FROM audit_log WHERE id = 1`); err == nil {
		t.Error("audit_log lost its append-only trigger")
	}
	s.Close()
	if bs, _ := filepath.Glob(filepath.Join(dir, "backups", "pre-migrate-*")); len(bs) != 0 {
		t.Errorf("a v3 fixture needs no migration, but a backup was taken: %v", bs)
	}

	// The audit chain verifies through the read-only handle `serverdock audit verify` uses.
	ro, err := store.OpenReadOnly(dir, store.ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	res, err := audit.Verify(ctx, ro.DB)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Rows != 5 {
		t.Errorf("audit chain: %+v", res)
	}
}
