package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

var ctx = context.Background()

func init() { lockWait = 100 * time.Millisecond } // keep "second server refused" tests quick

func list(extra ...Migration) []Migration { return append([]Migration{builtin[0]}, extra...) }

var (
	mParentChild = Migration{2, "0002_parent_child", `
		CREATE TABLE parent (id INTEGER PRIMARY KEY, name TEXT NOT NULL) STRICT;
		CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER NOT NULL REFERENCES parent(id) ON DELETE CASCADE) STRICT;`}
	// Rebuilds the parent table (create new, copy, drop old, rename): with
	// foreign_keys=1 the DROP would cascade-delete every child row.
	mRebuild = Migration{3, "0003_rebuild_parent", `
		CREATE TABLE parent_new (id INTEGER PRIMARY KEY, name TEXT NOT NULL, extra TEXT) STRICT;
		INSERT INTO parent_new (id, name) SELECT id, name FROM parent;
		DROP TABLE parent;
		ALTER TABLE parent_new RENAME TO parent;`}
	mBadFK = Migration{3, "0003_orphan", `INSERT INTO child (id, pid) VALUES (999, 424242);`}
)

// makePhase0 writes a database exactly as the Phase 0 stub left it.
func makePhase0(t *testing.T, dir string, extraRows int) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, fileName), true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT`,
		`INSERT INTO meta (key, value) VALUES ('schema_version', '1')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < extraRows; i++ {
		if _, err := db.Exec(`INSERT INTO meta VALUES (?, 'x')`, "row"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
}

func openWithT(t *testing.T, dir string, opts Options) (*Store, error) {
	t.Helper()
	s, err := openWith(ctx, dir, opts)
	if err == nil {
		t.Cleanup(func() { s.Close() })
	}
	return s, err
}

func versions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		rows.Scan(&v)
		out = append(out, v)
	}
	return out
}

func writerFK(t *testing.T, s *Store) string {
	t.Helper()
	c, err := s.W.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return pragma(t, c, "foreign_keys")
}

func ls(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestBuiltinMigrationList(t *testing.T) {
	if len(builtin) == 0 || builtin[0].Version != 1 || !strings.HasPrefix(builtin[0].Name, "0001_") {
		t.Fatalf("builtin: %+v", builtin)
	}
	if err := checkList(builtin); err != nil {
		t.Fatal(err)
	}
	if err := checkList([]Migration{{Version: 1}, {Version: 3}}); err == nil {
		t.Error("a gap in the list was accepted")
	}
}

func TestFreshDBAndReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := openWithT(t, dir, Options{Migrations: list(mParentChild)})
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(t, s.R); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("versions %v", got)
	}
	var name, sha string
	if err := s.R.QueryRow(`SELECT name, sha256 FROM schema_migrations WHERE version=1`).Scan(&name, &sha); err != nil || name != builtin[0].Name || sha != sum(builtin[0]) {
		t.Fatalf("row: %q %q %v", name, sha, err)
	}
	if _, err := os.Stat(filepath.Join(dir, backupDir)); err == nil {
		t.Error("a fresh database was backed up (there is nothing to lose)")
	}
	s.Close()
	s2, err := openWithT(t, dir, Options{Migrations: list(mParentChild)})
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(t, s2.R); len(got) != 2 {
		t.Fatalf("re-open changed versions: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, backupDir)); err == nil {
		t.Error("re-open with nothing pending took a backup")
	}
}

func TestPhase0AdoptionNeverTouchesMetaSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	makePhase0(t, dir, 3)
	// A trigger records any write to meta after this point.
	raw, err := sql.Open("sqlite", dsn(filepath.Join(dir, fileName), true))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE meta_writes (what TEXT)`,
		`CREATE TRIGGER mi AFTER INSERT ON meta BEGIN INSERT INTO meta_writes VALUES ('insert ' || new.key); END`,
		`CREATE TRIGGER mu AFTER UPDATE ON meta BEGIN INSERT INTO meta_writes VALUES ('update ' || new.key); END`,
		`CREATE TRIGGER md AFTER DELETE ON meta BEGIN INSERT INTO meta_writes VALUES ('delete ' || old.key); END`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	s, err := openWithT(t, dir, Options{Migrations: list(mParentChild, mRebuild)})
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(t, s.R); len(got) != 3 || got[0] != 1 {
		t.Fatalf("versions after adoption: %v", got)
	}
	var v string
	if err := s.R.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err != nil || v != "1" {
		t.Fatalf("meta.schema_version = %q %v", v, err)
	}
	var n int
	s.R.QueryRow(`SELECT count(*) FROM meta_writes`).Scan(&n)
	if n != 0 {
		t.Errorf("meta was written %d times during adoption and migration", n)
	}
	// The adoption recorded the real checksum of 0001 without running it.
	var sha string
	s.R.QueryRow(`SELECT sha256 FROM schema_migrations WHERE version=1`).Scan(&sha)
	if sha != sum(builtin[0]) {
		t.Error("adopted checksum differs from the embedded 0001")
	}
	// Adopted data survived, and the Phase 0 database was backed up first.
	s.R.QueryRow(`SELECT count(*) FROM meta WHERE key LIKE 'row%'`).Scan(&n)
	if n != 3 {
		t.Errorf("rows after adoption: %d", n)
	}
	if bs, _ := filepath.Glob(filepath.Join(dir, backupDir, "pre-migrate-*-v0001-v0003.db")); len(bs) != 1 {
		t.Errorf("backups: %v", bs)
	}
}

func TestPhase0AdoptedEvenWithNothingPending(t *testing.T) {
	dir := t.TempDir()
	makePhase0(t, dir, 1)
	s, err := openWithT(t, dir, Options{Migrations: list()}) // only 0001 exists: nothing pending
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(t, s.R); len(got) != 1 || got[0] != 1 {
		t.Fatalf("versions %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, backupDir)); err == nil {
		t.Error("backup taken with nothing to migrate")
	}
}

func TestChecksumMismatchAndNewerVersionRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := openWithT(t, dir, Options{Migrations: list(mParentChild)})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// An edited migration.
	edited := mParentChild
	edited.SQL += "\n-- edited after it was applied"
	if _, err := openWith(ctx, dir, Options{Migrations: list(edited)}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("edited migration: %v", err)
	}
	// A database written by a newer ServerDock (knows version 2, we know only 1).
	if _, err := openWith(ctx, dir, Options{Migrations: list()}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer database: %v", err)
	}
	// And a version we have never heard of, on the builtin list.
	dir = t.TempDir()
	bs, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	bs.W.Exec(`INSERT INTO schema_migrations VALUES (99, '0099_future', 'x', 'now')`)
	bs.Close()
	if _, err := Open(ctx, dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("future version: %v", err)
	}
	// A refused start must release the lock, so a fixed binary can start.
	if _, err := lockDir(dir); err != nil {
		t.Errorf("lock still held after a refused start: %v", err)
	}
}

func TestRebuildingAParentTableKeepsItsChildren(t *testing.T) {
	dir := t.TempDir()
	s, err := openWithT(t, dir, Options{Migrations: list(mParentChild)})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO parent VALUES (1,'a'), (2,'b')`,
		`INSERT INTO child VALUES (10,1), (11,1), (12,2)`,
	} {
		if _, err := s.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = openWithT(t, dir, Options{Migrations: list(mParentChild, mRebuild)})
	if err != nil {
		t.Fatal(err)
	}
	var kids, parents int
	s.R.QueryRow(`SELECT count(*) FROM child`).Scan(&kids)
	s.R.QueryRow(`SELECT count(*) FROM parent`).Scan(&parents)
	if kids != 3 || parents != 2 {
		t.Fatalf("after the rebuild: %d children, %d parents (cascade deleted them?)", kids, parents)
	}
	if got := writerFK(t, s); got != "1" {
		t.Errorf("writer foreign_keys = %s after migrating", got)
	}
	// FK enforcement is really on again for normal writes.
	if _, err := s.W.Exec(`INSERT INTO child VALUES (50, 777)`); err == nil {
		t.Error("an orphan insert was accepted: foreign keys are off")
	}
}

func TestMigrationThatBreaksForeignKeysIsRolledBack(t *testing.T) {
	dir := t.TempDir()
	s, err := openWithT(t, dir, Options{Migrations: list(mParentChild)})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := openWith(ctx, dir, Options{Migrations: list(mParentChild, mBadFK)}); err == nil || !strings.Contains(err.Error(), "foreign_key_check") {
		t.Fatalf("FK violation: %v", err)
	}
	s, err = openWithT(t, dir, Options{Migrations: list(mParentChild)})
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(t, s.R); len(got) != 2 {
		t.Errorf("the failed migration was recorded: %v", got)
	}
	var n int
	s.R.QueryRow(`SELECT count(*) FROM child`).Scan(&n)
	if n != 0 {
		t.Errorf("the orphan row survived the rollback (%d)", n)
	}
	if got := writerFK(t, s); got != "1" {
		t.Errorf("foreign_keys = %s", got)
	}
}

func TestPooledWriterConnectionHasForeignKeysOnAfterAFailedMigration(t *testing.T) {
	dir := t.TempDir()
	s, _ := openWithT(t, dir, Options{Migrations: list(mParentChild)})
	s.Close()
	db, _ := sql.Open("sqlite", dsn(filepath.Join(dir, fileName), true))
	db.SetMaxOpenConns(1)
	defer db.Close()
	err := migrate(ctx, db, dir, Options{Migrations: list(mParentChild, mBadFK), FreeSpace: func(string) (uint64, error) { return 1 << 40, nil }, Now: time.Now})
	if err == nil {
		t.Fatal("expected a failure")
	}
	c, err := db.Conn(ctx) // the very connection the runner used (pool of one)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := pragma(t, c, "foreign_keys"); got != "1" {
		t.Errorf("pooled connection foreign_keys = %s", got)
	}
}

func TestTooLittleFreeSpaceRefusesAndTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	makePhase0(t, dir, 2)
	var asked string
	_, err := openWith(ctx, dir, Options{Migrations: list(mParentChild), FreeSpace: func(d string) (uint64, error) { asked = d; return 10 << 20, nil }})
	if err == nil || !strings.Contains(err.Error(), "free disk space") {
		t.Fatalf("err = %v", err)
	}
	if asked != dir {
		t.Errorf("free space asked for %q", asked)
	}
	// Untouched: still a Phase 0 database (no schema_migrations, no adoption) and no backup.
	ro, err := OpenReadOnly(dir, ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if ro.Schema != "v1 (unadopted)" {
		t.Errorf("schema = %q: the refused start modified the database", ro.Schema)
	}
	if bs, _ := filepath.Glob(filepath.Join(dir, backupDir, "*")); len(bs) != 0 {
		t.Errorf("backups after a refusal: %v", bs)
	}
	// The threshold is 2.2x the database plus 64 MiB.
	need := uint64(float64(fileSize(filepath.Join(dir, fileName))+fileSize(filepath.Join(dir, fileName+"-wal")))*2.2) + 64<<20
	var asks int
	_, err = openWith(ctx, dir, Options{Migrations: list(mParentChild), FreeSpace: func(string) (uint64, error) { asks++; return need - 1, nil }})
	if err == nil {
		t.Error("free space just under the threshold was accepted")
	}
	s, err := openWithT(t, dir, Options{Migrations: list(mParentChild), FreeSpace: func(string) (uint64, error) { return need, nil }})
	if err != nil {
		t.Fatalf("free space at the threshold: %v", err)
	}
	s.Close()
}

func TestBackupIsAtomicVerifiedAndPrivate(t *testing.T) {
	dir := t.TempDir()
	makePhase0(t, dir, 4)
	var sawFinal, sawTmp bool
	s, err := openWithT(t, dir, Options{Migrations: list(mParentChild), afterBackup: func(p string) {
		_, e1 := os.Stat(p)
		_, e2 := os.Stat(p + ".tmp")
		sawFinal, sawTmp = e1 == nil, e2 == nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !sawFinal || sawTmp {
		t.Errorf("at verification the backup should be renamed into place (final=%v tmp=%v)", sawFinal, sawTmp)
	}
	bs, _ := filepath.Glob(filepath.Join(dir, backupDir, "*"))
	if len(bs) != 1 || !strings.HasSuffix(bs[0], ".db") || !strings.Contains(filepath.Base(bs[0]), "-v0001-v0002.db") {
		t.Fatalf("backups: %v", bs)
	}
	if st, _ := os.Stat(bs[0]); st.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(dir, backupDir)); st.Mode().Perm() != 0o700 {
		t.Errorf("backups dir mode %v", st.Mode().Perm())
	}
	if err := quickCheck(ctx, bs[0]); err != nil {
		t.Errorf("backup fails quick_check: %v", err)
	}
	// The backup is the pre-migration state: meta rows present, no new tables.
	bdb, _ := sql.Open("sqlite", roDSN(bs[0], true))
	defer bdb.Close()
	var rows, mig int
	bdb.QueryRow(`SELECT count(*) FROM meta`).Scan(&rows)
	bdb.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('parent','schema_migrations')`).Scan(&mig)
	if rows != 5 || mig != 0 {
		t.Errorf("backup content: %d meta rows, %d new tables", rows, mig)
	}
	_ = s
}

func TestBackupThatFailsQuickCheckStopsTheMigration(t *testing.T) {
	dir := t.TempDir()
	makePhase0(t, dir, 2)
	_, err := openWith(ctx, dir, Options{Migrations: list(mParentChild), afterBackup: func(p string) {
		b, _ := os.ReadFile(p)
		for i := 100; i < len(b) && i < 4096; i++ { // scramble the first page's body
			b[i] ^= 0xff
		}
		os.WriteFile(p, b, 0o600)
	}})
	if err == nil {
		t.Fatal("a corrupt backup did not stop the migration")
	}
	ro, err := OpenReadOnly(dir, ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if ro.Schema != "v1 (unadopted)" {
		t.Errorf("schema %q: the migration ran anyway", ro.Schema)
	}
	if bs, _ := filepath.Glob(filepath.Join(dir, backupDir, "*.db")); len(bs) != 0 {
		t.Errorf("the corrupt backup was kept: %v", bs)
	}
}

func TestOnlyTheLastThreeBackupsAreKept(t *testing.T) {
	dir := t.TempDir()
	makePhase0(t, dir, 1)
	clk := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ms := list()
	for i := 2; i <= 6; i++ {
		ms = append(ms, Migration{i, "000" + string(rune('0'+i)) + "_step", "CREATE TABLE t" + string(rune('0'+i)) + " (x INTEGER) STRICT;"})
		clk = clk.Add(time.Minute)
		now := clk
		s, err := openWithT(t, dir, Options{Migrations: ms, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	bs, _ := filepath.Glob(filepath.Join(dir, backupDir, "pre-migrate-*.db"))
	if len(bs) != 3 {
		t.Fatalf("kept %d backups: %v", len(bs), bs)
	}
	sort.Strings(bs)
	if !strings.Contains(bs[0], "-v0003-v0004.db") || !strings.Contains(bs[2], "-v0005-v0006.db") {
		t.Errorf("wrong backups kept: %v", bs)
	}
}

func TestLeftoverTempFilesAreWipedOnBoot(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, backupDir), 0o700)
	os.MkdirAll(filepath.Join(dir, "tmp", "sub"), 0o700)
	os.WriteFile(filepath.Join(dir, backupDir, "pre-migrate-1-2-x.db.tmp"), []byte("half"), 0o600)
	os.WriteFile(filepath.Join(dir, backupDir, "pre-migrate-1-2-keep.db"), []byte("good"), 0o600)
	os.WriteFile(filepath.Join(dir, "tmp", "sub", "scratch"), []byte("x"), 0o600)
	if _, err := openWithT(t, dir, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, backupDir, "pre-migrate-1-2-x.db.tmp")); err == nil {
		t.Error("leftover .tmp survived")
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp")); err == nil {
		t.Error("DATA/tmp survived")
	}
	if _, err := os.Stat(filepath.Join(dir, backupDir, "pre-migrate-1-2-keep.db")); err != nil {
		t.Error("a finished backup was wiped")
	}
}

func TestSecondServerOnTheSameDirIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := openWithT(t, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dir); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second server: %v", err)
	}
	s.Close()
	s2, err := Open(ctx, dir) // the lock is released on Close
	if err != nil {
		t.Fatalf("after Close: %v", err)
	}
	s2.Close()
}

func TestWriteTx(t *testing.T) {
	s := open(t)
	if err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO meta VALUES ('wt','1')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	boom := os.ErrInvalid
	if err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		tx.Exec(`INSERT INTO meta VALUES ('wt2','1')`)
		return boom
	}); err != boom {
		t.Fatalf("err = %v", err)
	}
	func() {
		defer func() { recover() }()
		s.WriteTx(ctx, func(tx *sql.Tx) error {
			tx.Exec(`INSERT INTO meta VALUES ('wt3','1')`)
			panic("boom")
		})
	}()
	var n int
	s.R.QueryRow(`SELECT count(*) FROM meta WHERE key LIKE 'wt%'`).Scan(&n)
	if n != 1 {
		t.Errorf("rows %d, want only the committed one", n)
	}
	// The writer is usable after a panic (the transaction was rolled back).
	if _, err := s.W.Exec(`INSERT INTO meta VALUES ('after','1')`); err != nil {
		t.Errorf("writer stuck after a panic: %v", err)
	}
}

func stepList(n int) []Migration {
	ms := list()
	for i := 2; i <= n; i++ {
		ms = append(ms, Migration{i, fmt.Sprintf("%04d_step", i), fmt.Sprintf("CREATE TABLE s%d (x INTEGER) STRICT;", i)})
	}
	return ms
}

// Backups sort by time, not by name: once versions have two digits
// "pre-migrate-10-11" must not sort before "7-8", and a restore that sends
// `from` backwards must not make the newest backup the first to be pruned.
func TestPruneKeepsTheNewestBackupsAcrossTwoDigitVersionsAndRestores(t *testing.T) {
	dir := t.TempDir()
	makePhase0(t, dir, 1)
	clk := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	step := func(upTo int) {
		t.Helper()
		clk = clk.Add(time.Minute)
		now := clk
		s, err := openWithT(t, dir, Options{Migrations: stepList(upTo), Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	backups := func() []string {
		bs, _ := filepath.Glob(filepath.Join(dir, backupDir, "pre-migrate-*.db"))
		sort.Strings(bs)
		var names []string
		for _, b := range bs {
			names = append(names, filepath.Base(b)[strings.Index(filepath.Base(b), "Z-v")+1:])
		}
		return names
	}
	for _, to := range []int{3, 5, 7, 9, 11} {
		step(to)
	}
	got := backups()
	want := []string{"-v0005-v0007.db", "-v0007-v0009.db", "-v0009-v0011.db"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after 1->3->5->7->9->11 kept %v, want %v (the newest, 9->11, must survive)", got, want)
	}
	// Restore the 7->9 backup (a database at version 7), then migrate again: `from` goes
	// backwards (7) after the last recorded backup's 9, and the new backup is the newest.
	bs, _ := filepath.Glob(filepath.Join(dir, backupDir, "*-v0007-v0009.db"))
	b, err := os.ReadFile(bs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{fileName + "-wal", fileName + "-shm"} {
		os.Remove(filepath.Join(dir, f))
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), b, 0o644); err != nil {
		t.Fatal(err)
	}
	step(12)
	got = backups()
	want = []string{"-v0007-v0009.db", "-v0009-v0011.db", "-v0007-v0012.db"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after the restore and re-migrate kept %v, want %v", got, want)
	}
}

func TestGapsInTheMigrationHistoryAreRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := openWithT(t, dir, Options{Migrations: list(mParentChild, mRebuild)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.W.Exec(`DELETE FROM schema_migrations WHERE version = 2`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := openWith(ctx, dir, Options{Migrations: list(mParentChild, mRebuild)}); err == nil || !strings.Contains(err.Error(), "gaps") {
		t.Fatalf("gap in the history: %v", err)
	}
	if _, err := OpenReadOnly(dir, ReadOnlyOptions{Immutable: true}); err == nil {
		t.Error("the read-only open accepted a history with a gap")
	}
}

// A read-only probe (audit verify from cron) holds a shared lock for a moment;
// a starting server waits it out instead of failing.
func TestStartingServerWaitsOutAReadOnlyProbe(t *testing.T) {
	dir := t.TempDir()
	s, _ := openWithT(t, dir, Options{})
	s.Close()
	probe, err := os.Open(filepath.Join(dir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
		probe.Close()
	}()
	old := lockWait
	lockWait = 3 * time.Second
	defer func() { lockWait = old }()
	t0 := time.Now()
	s2, err := openWithT(t, dir, Options{})
	if err != nil {
		t.Fatalf("a probe made the server fail to start: %v", err)
	}
	if d := time.Since(t0); d < 250*time.Millisecond {
		t.Errorf("returned after %v without waiting for the probe", d)
	}
	s2.Close()
}

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testdata/v2.db is a database as the end of M1 leaves it (migrations 0001 and
// 0002, an audit chain, a meta row). It must keep opening with the real
// migration list: editing an applied migration changes its checksum and fails here.
func TestV2FixtureOpensAndMigrates(t *testing.T) {
	dir := copyFixture(t, "v2.db")
	// 1. The builtin list has grown past v2 (0003 auth): the fixture opens, a
	// verified backup of v2 is taken first, and the audit chain and meta row survive.
	s, err := openWithT(t, dir, Options{})
	if err != nil {
		t.Fatalf("the v2 fixture no longer opens with the builtin migrations: %v", err)
	}
	if got := versions(t, s.R); len(got) != len(builtin) || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("versions %v", got)
	}
	var n int
	s.R.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&n)
	if n != 5 {
		t.Fatalf("audit rows %d", n)
	}
	var v string
	if err := s.R.QueryRow(`SELECT value FROM meta WHERE key='fixture'`).Scan(&v); err != nil || v != "v2" {
		t.Fatalf("meta fixture row: %q %v", v, err)
	}
	bs, _ := filepath.Glob(filepath.Join(dir, backupDir, fmt.Sprintf("pre-migrate-*-v0002-v%04d.db", len(builtin))))
	if len(bs) != 1 {
		t.Fatalf("backups %v", bs)
	}
	if err := quickCheck(ctx, bs[0]); err != nil {
		t.Errorf("backup quick_check: %v", err)
	}
	// The auth tables exist and are empty; the audit triggers survived.
	for _, tbl := range []string{"users", "recovery_codes", "sessions", "auth_pending", "auth_known_ips", "api_tokens", "setup_state"} {
		if err := s.R.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d %v", tbl, n, err)
		}
	}
	if _, err := s.W.Exec(`DELETE FROM audit_log WHERE id = 1`); err == nil {
		t.Error("audit_log lost its append-only trigger")
	}
	s.Close()

	// 2. Reopening: nothing pending, no second backup.
	s, err = openWithT(t, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if bs, _ := filepath.Glob(filepath.Join(dir, backupDir, "pre-migrate-*")); len(bs) != 1 {
		t.Errorf("backups after a plain reopen: %v", bs)
	}

	// 3. A future migration runs on top of v3, with its own backup.
	next := Migration{len(builtin) + 1, "next", "CREATE TABLE next_feature (x INTEGER) STRICT; INSERT INTO next_feature VALUES (1);"}
	s, err = openWithT(t, dir, Options{Migrations: append(append([]Migration{}, builtin...), next)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := versions(t, s.R); len(got) != len(builtin)+1 {
		t.Fatalf("versions after migrating %v", got)
	}
}

// Migration 0003 on a fresh database creates every table and column the plan lists.
func TestAuthMigrationColumns(t *testing.T) {
	s, err := openWithT(t, t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := map[string]string{
		"users":          "id username password_hash totp_seed totp_pending_seed totp_pending_expires_at totp_pending_enrollment_hash totp_pending_tries totp_confirmed_at totp_last_counter totp_enrolled_host scope setup_ip setup_at password_changed_at totp_fail_since_login totp_fail_last_at totp_fail_last_ip created_at",
		"recovery_codes": "user_id hash used_at",
		"sessions":       "id_hash public_id user_id created_at last_seen_at idle_expires_at abs_expires_at mfa_fresh_until reauth_failures ip_key user_agent notice_json",
		"auth_pending":   "id_hash user_id expires_at tries_left ip_key created_at",
		"auth_known_ips": "user_id ip_key last_ok_at",
		"api_tokens":     "id user_id name prefix hash scope created_at expires_at last_used_at revoked_at",
		"setup_state":    "id token_hash expires_at enrollment_hash pending_seed created_at",
	}
	for table, cols := range want {
		rows, err := s.R.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var c string
			rows.Scan(&c)
			got = append(got, c)
		}
		rows.Close()
		if strings.Join(got, " ") != cols {
			t.Errorf("%s columns:\n got %v\nwant %s", table, got, cols)
		}
	}
	// username is unique case-insensitively; setup_state holds one row.
	s.W.Exec(`INSERT INTO users (username, created_at) VALUES ('Admin', 1)`)
	if _, err := s.W.Exec(`INSERT INTO users (username, created_at) VALUES ('admin', 1)`); err == nil {
		t.Error("usernames are case-sensitive")
	}
	s.W.Exec(`INSERT INTO setup_state (id, token_hash, expires_at, created_at) VALUES (1, x'00', 1, 1)`)
	if _, err := s.W.Exec(`INSERT INTO setup_state (id, token_hash, expires_at, created_at) VALUES (2, x'00', 1, 1)`); err == nil {
		t.Error("setup_state accepted a second row")
	}
}

func TestV2FixtureReadOnlyReportsItsSchema(t *testing.T) {
	dir := copyFixture(t, "v2.db")
	ro, err := OpenReadOnly(dir, ReadOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if ro.Schema != "v2" {
		t.Errorf("schema %q", ro.Schema)
	}
}
