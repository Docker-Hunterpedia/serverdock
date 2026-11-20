package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one forward-only schema step.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Options tunes Open; the zero value is production behaviour. Tests inject the
// migration list, the free-space probe and the clock.
type Options struct {
	Migrations []Migration
	// FreeSpace returns the bytes available to us on the filesystem holding dir.
	FreeSpace func(dir string) (uint64, error)
	Now       func() time.Time

	// afterBackup is a test hook run on the finished backup before it is verified.
	afterBackup func(path string)
}

func (o *Options) defaults() {
	if o.Migrations == nil {
		o.Migrations = builtin
	}
	if o.FreeSpace == nil {
		o.FreeSpace = freeSpace
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// builtin is the embedded migration list (migrations/NNNN_name.sql).
var builtin = mustLoad(migrationFS)

func mustLoad(fsys embed.FS) []Migration {
	ents, err := fsys.ReadDir("migrations")
	if err != nil {
		panic(err)
	}
	var out []Migration
	for _, e := range ents {
		name := strings.TrimSuffix(e.Name(), ".sql")
		num, rest, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || len(num) != 4 || rest == "" {
			panic("store: bad migration file name " + e.Name())
		}
		b, err := fsys.ReadFile("migrations/" + e.Name())
		if err != nil {
			panic(err)
		}
		out = append(out, Migration{Version: v, Name: name, SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	if err := checkList(out); err != nil {
		panic(err)
	}
	return out
}

// checkList requires versions 1..n with no gaps or repeats.
func checkList(ms []Migration) error {
	for i, m := range ms {
		if m.Version != i+1 {
			return fmt.Errorf("store: migration list must be 1..n without gaps (found %d at position %d)", m.Version, i+1)
		}
	}
	return nil
}

func sum(m Migration) string {
	h := sha256.Sum256([]byte(m.SQL))
	return hex.EncodeToString(h[:])
}

type applied struct {
	version int
	sha     string
}

// schemaState inspects the database: fresh, a Phase 0 database (meta only), or
// one with schema_migrations.
func schemaState(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (fresh, phase0 bool, rows []applied, err error) {
	tr, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return false, false, nil, err
	}
	tables := map[string]bool{}
	for tr.Next() {
		var n string
		if err := tr.Scan(&n); err != nil {
			tr.Close()
			return false, false, nil, err
		}
		tables[n] = true
	}
	if err := tr.Close(); err != nil {
		return false, false, nil, err
	}
	switch {
	case tables["schema_migrations"]:
		ar, err := q.QueryContext(ctx, `SELECT version, sha256 FROM schema_migrations ORDER BY version`)
		if err != nil {
			return false, false, nil, err
		}
		defer ar.Close()
		for ar.Next() {
			var a applied
			if err := ar.Scan(&a.version, &a.sha); err != nil {
				return false, false, nil, err
			}
			rows = append(rows, a)
		}
		return false, false, rows, ar.Err()
	case tables["meta"]:
		var v string
		if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err != nil || v != "1" {
			return false, false, nil, errors.New("store: meta table without schema_version 1: not a ServerDock database this version understands")
		}
		return false, true, nil, nil
	case len(tables) == 0:
		return true, false, nil, nil
	}
	return false, false, nil, errors.New("store: the database has tables but no meta or schema_migrations: not a ServerDock database")
}

// verifyApplied refuses an applied version we do not know (a newer ServerDock
// wrote it) and any checksum mismatch (an edited migration).
func verifyApplied(ms []Migration, rows []applied) (max int, err error) {
	for _, a := range rows {
		if a.version < 1 || a.version > len(ms) {
			return 0, fmt.Errorf("store: the database has migration %d applied, but this ServerDock only knows up to %d: it was written by a newer version; refusing to start", a.version, len(ms))
		}
		if want := sum(ms[a.version-1]); a.sha != want {
			return 0, fmt.Errorf("store: migration %d (%s) was changed after it was applied (checksum mismatch); refusing to start", a.version, ms[a.version-1].Name)
		}
		max = max2(max, a.version)
	}
	// Versions are distinct and all >= 1, so len(rows) == max means exactly 1..max.
	if len(rows) != max {
		return 0, fmt.Errorf("store: the migration history has gaps (%d rows recorded, highest version %d); refusing to start", len(rows), max)
	}
	return max, nil
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// migrate brings the database to the newest version. It pins one writer
// connection, because foreign keys must be switched off on that very
// connection and PRAGMA foreign_keys cannot change inside a transaction.
func migrate(ctx context.Context, db *sql.DB, dir string, opts Options) error {
	ms := opts.Migrations
	if err := checkList(ms); err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer conn.Close()

	fresh, phase0, rows, err := schemaState(ctx, conn)
	if err != nil {
		return err
	}
	cur := 0
	switch {
	case fresh:
	case phase0:
		cur = 1
	default:
		if cur, err = verifyApplied(ms, rows); err != nil {
			return err
		}
	}
	// A database that has data is backed up before anything (adoption
	// included) touches it, but only when a migration is actually pending; a
	// new one has nothing to lose.
	if !fresh && cur < len(ms) {
		if err := backupBeforeMigrating(ctx, conn, dir, cur, len(ms), opts); err != nil {
			return err
		}
	}
	if phase0 {
		if err := adopt(ctx, conn, ms[0], opts.Now()); err != nil {
			return err
		}
	}
	for _, m := range ms[cur:] {
		if err := applyOne(ctx, conn, m, opts.Now()); err != nil {
			return err
		}
	}
	return nil
}

const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY, name TEXT NOT NULL, sha256 TEXT NOT NULL, applied_at TEXT NOT NULL) STRICT`

// adopt records a Phase 0 database as v1 without running 0001 (its tables exist).
func adopt(ctx context.Context, conn *sql.Conn, m Migration, now time.Time) error {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: adopt: %w", err)
	}
	fail := func(err error) error {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return fmt.Errorf("store: adopt: %w", err)
	}
	if _, err := conn.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return fail(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, sha256, applied_at) VALUES (?, ?, ?, ?)`,
		m.Version, m.Name, sum(m), now.UTC().Format(time.RFC3339)); err != nil {
		return fail(err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fail(err)
	}
	return nil
}

// applyOne runs one migration with foreign keys off, checks them before the
// commit, and always turns them back on. A table rebuild (create new, copy,
// drop old, rename) would otherwise cascade-delete child rows under foreign_keys=1.
func applyOne(ctx context.Context, conn *sql.Conn, m Migration, now time.Time) (err error) {
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("store: migration %d: %w", m.Version, err)
	}
	defer func() {
		if _, ferr := conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys=ON"); ferr != nil {
			// Never hand a connection with FK checks off back to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			if err == nil {
				err = fmt.Errorf("store: migration %d: re-enabling foreign keys: %w", m.Version, ferr)
			}
		}
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: migration %d: %w", m.Version, err)
	}
	rollback := func(err error) error {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return fmt.Errorf("store: migration %d (%s): %w", m.Version, m.Name, err)
	}
	if _, err := conn.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return rollback(err)
	}
	if _, err := conn.ExecContext(ctx, m.SQL); err != nil {
		return rollback(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, sha256, applied_at) VALUES (?, ?, ?, ?)`,
		m.Version, m.Name, sum(m), now.UTC().Format(time.RFC3339)); err != nil {
		return rollback(err)
	}
	fk, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return rollback(err)
	}
	bad := fk.Next()
	_ = fk.Close()
	if bad {
		return rollback(errors.New("foreign_key_check found violations; the migration was rolled back and ServerDock will not start"))
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback(err)
	}
	return nil
}

// backupMargin is added to 2.2x the database size when checking free space.
const backupMargin = 64 << 20

func backupBeforeMigrating(ctx context.Context, conn *sql.Conn, dir string, from, to int, opts Options) error {
	size := fileSize(filepath.Join(dir, fileName)) + fileSize(filepath.Join(dir, fileName+"-wal"))
	need := uint64(float64(size)*2.2) + backupMargin
	free, err := opts.FreeSpace(dir)
	if err != nil {
		return fmt.Errorf("store: checking free space: %w", err)
	}
	if free < need {
		return fmt.Errorf("store: not enough free disk space to back up the database before migrating (%d MiB free, need %d MiB); free some space and start again. The database was not touched", free>>20, need>>20)
	}
	bdir := filepath.Join(dir, backupDir)
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(bdir, 0o700); err != nil {
		return err
	}
	// The timestamp comes first and the versions are zero-padded, so names sort
	// by time: pruning keeps the newest even after a restore sends `from`
	// backwards, or once versions pass 9.
	name := fmt.Sprintf("pre-migrate-%s-v%04d-v%04d.db", opts.Now().UTC().Format("20060102T150405.000Z"), from, to)
	final := filepath.Join(bdir, name)
	tmp := final + ".tmp"
	_ = os.Remove(tmp)
	if _, err := conn.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(tmp, "'", "''")+"'"); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: pre-migrate backup: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	if err := syncDir(bdir); err != nil {
		return err
	}
	if opts.afterBackup != nil {
		opts.afterBackup(final)
	}
	if err := quickCheck(ctx, final); err != nil {
		_ = os.Remove(final)
		return fmt.Errorf("store: the pre-migrate backup failed its integrity check, so the migration was not started: %w", err)
	}
	return pruneBackups(bdir, 3)
}

func quickCheck(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", roDSN(path, true))
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("quick_check: %s", res)
	}
	return nil
}

// pruneBackups keeps the newest `keep` pre-migrate backups (the timestamp is
// the first part of the name, so a name sort is a time sort).
func pruneBackups(bdir string, keep int) error {
	files, err := filepath.Glob(filepath.Join(bdir, "pre-migrate-*.db"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for len(files) > keep {
		if err := os.Remove(files[0]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		files = files[1:]
	}
	return nil
}

func fileSize(p string) uint64 {
	if st, err := os.Stat(p); err == nil {
		return uint64(st.Size())
	}
	return 0
}

func freeSpace(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func syncFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncDir(p string) error { return syncFile(p) }
