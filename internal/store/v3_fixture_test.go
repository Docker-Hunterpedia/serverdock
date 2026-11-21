package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

var updateV3 = flag.Bool("update-v3", false, "regenerate testdata/v3.db from testdata/v2.db (a new migration never edits it: add v4.db instead)")

const dbFile = "serverdock.db"

// The fixture's secrets are dummy and fixed, for tests only: a made-up master key,
// a made-up PHC string and a made-up TOTP seed sealed under that key.
var (
	fixtureKey  = bytes.Repeat([]byte{0x42}, 32)
	fixtureSeed = []byte("0123456789abcdefghij")
)

const fixturePHC = "$argon2id$v=19$m=19456,t=2,p=1$ZHVtbXktZml4ZWQtc2FsdA$ZHVtbXktZml4ZWQtaGFzaC1mb3ItdGVzdHMtb25seS0wMDAwMDAwMDAwMDA"

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

func h32(n byte) []byte { return bytes.Repeat([]byte{n}, 32) }

// TestGenerateV3Fixture rebuilds testdata/v3.db: the v2 database (an audit chain
// and a meta row) after migration 0003, with an admin and its child rows as the
// end of M2 leaves a configured server. Run it with -update-v3 only to create
// the file; the file is then frozen.
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
	box, err := secretbox.New(fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal("users/1/totp_seed", fixtureSeed)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`UPDATE meta SET value = 'v3' WHERE key = 'fixture'`)
	exec(`INSERT INTO meta(key, value) VALUES ('secretbox_key_id', ?)`, box.KeyID())
	exec(`INSERT INTO users(id, username, password_hash, totp_seed, totp_confirmed_at, totp_last_counter, totp_enrolled_host, scope,
		setup_ip, setup_at, password_changed_at, created_at) VALUES (1, 'admin', ?, ?, 1700000000, 56666666, 'ip', 'admin', '203.0.113.9', 1700000000, 1700000000, 1700000000)`,
		fixturePHC, sealed)
	for i := 0; i < 10; i++ {
		exec(`INSERT INTO recovery_codes(user_id, hash) VALUES (1, ?)`, h32(byte(0x10+i)))
	}
	exec(`INSERT INTO sessions(id_hash, public_id, user_id, created_at, last_seen_at, idle_expires_at, abs_expires_at, mfa_fresh_until, ip_key, user_agent)
		VALUES (?, 'fixture1', 1, 1700000000, 1700000000, 4102444800, 4102444800, 1700000300, '203.0.113.9', 'fixture-agent')`, h32(0x20))
	exec(`INSERT INTO auth_pending(id_hash, user_id, expires_at, tries_left, ip_key, created_at) VALUES (?, 1, 4102444800, 5, '203.0.113.9', 1700000000)`, h32(0x30))
	exec(`INSERT INTO auth_known_ips(user_id, ip_key, last_ok_at) VALUES (1, '203.0.113.9', 1700000000)`)
	exec(`INSERT INTO api_tokens(user_id, name, prefix, hash, scope, created_at, expires_at) VALUES (1, 'fixture token', 'abcd1234', ?, 'read', 1700000000, 4102444800)`, h32(0x40))
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

// testdata/v3.db is a database as the end of M2 leaves a configured server:
// migrations 0001 to 0003, the M1 audit chain, a meta row, and an admin with its
// child rows (a sealed TOTP seed under a fixed test key, ten recovery codes, one
// session, one pending sign-in, one known IP, one API token, and
// meta.secretbox_key_id). It must keep opening with the real migration list, the
// rows must survive (a migration 0004 that rebuilds `users` would drop its
// children if it ignored foreign keys), and the audit chain must still verify.
// Editing an applied migration changes its checksum and fails here.
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
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(versions) < 3 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 {
		t.Fatalf("versions %v", versions)
	}
	last := versions[len(versions)-1]

	var v string
	if err := s.R.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'fixture'`).Scan(&v); err != nil || v != "v3" {
		t.Fatalf("meta fixture row: %q %v", v, err)
	}
	// The admin and every child row survive whatever migrations come after 0003.
	for tbl, want := range map[string]int{"users": 1, "recovery_codes": 10, "sessions": 1, "auth_pending": 1, "auth_known_ips": 1, "api_tokens": 1, "setup_state": 0} {
		var n int
		if err := s.R.QueryRowContext(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows, want %d (%v)", tbl, n, want, err)
		}
	}
	box, err := secretbox.New(fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	var id, sealed = "", []byte(nil)
	if err := s.R.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'secretbox_key_id'`).Scan(&id); err != nil || id != box.KeyID() {
		t.Errorf("meta.secretbox_key_id %q (%v), want %q", id, err, box.KeyID())
	}
	if err := s.R.QueryRowContext(ctx, `SELECT totp_seed FROM users WHERE id = 1`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if got, err := box.Open("users/1/totp_seed", sealed); err != nil || !bytes.Equal(got, fixtureSeed) {
		t.Errorf("the sealed seed no longer opens under the fixed test key: %v", err)
	}
	var phc string
	if err := s.R.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = 1`).Scan(&phc); err != nil || phc != fixturePHC {
		t.Errorf("password hash %q (%v)", phc, err)
	}
	if _, err := s.W.ExecContext(ctx, `DELETE FROM audit_log WHERE id = 1`); err == nil {
		t.Error("audit_log lost its append-only trigger")
	}
	s.Close()

	// Backups: when the builtin list has grown past 0003, opening the v3 file must
	// have taken exactly one verified backup of v3 first; otherwise there is nothing
	// pending and no backup.
	bs, _ := filepath.Glob(filepath.Join(dir, "backups", "pre-migrate-*"))
	if last > 3 {
		want, _ := filepath.Glob(filepath.Join(dir, "backups", fmt.Sprintf("pre-migrate-*-v0003-v%04d.db", last)))
		if len(bs) != 1 || len(want) != 1 {
			t.Fatalf("migrating v3 to v%d: backups %v", last, bs)
		}
		bdb, err := sql.Open("sqlite", want[0])
		if err != nil {
			t.Fatal(err)
		}
		defer bdb.Close()
		var qc string
		if err := bdb.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&qc); err != nil || qc != "ok" {
			t.Errorf("backup quick_check: %q %v", qc, err)
		}
	} else if len(bs) != 0 {
		t.Errorf("nothing is pending on the v3 fixture, but a backup was taken: %v", bs)
	}

	// The audit chain verifies through the read-only handle `serverdock audit verify`
	// uses. Rows == 5 assumes that no future migration writes audit rows (a
	// migration is schema and data movement, not an audited action).
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
