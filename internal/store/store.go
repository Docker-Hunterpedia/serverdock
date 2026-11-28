// Package store opens the SQLite database (pure Go, WAL) as one writer pool
// and one small reader pool, applies forward-only migrations, and offers a
// read-only open for tools that must never write. Pragmas are set through the
// DSN so that every pooled connection gets them.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const (
	fileName    = "serverdock.db"
	lockName    = "serverdock.lock"
	backupDir   = "backups"
	readerConns = 2
)

// Store holds the writer and reader pools.
type Store struct {
	// W has MaxOpenConns(1) and BEGIN IMMEDIATE transactions.
	W *sql.DB
	// R is a small read-only pool.
	R *sql.DB
	// Dir is the data directory.
	Dir string

	lock *os.File // flock on DATA/serverdock.lock, held until Close
}

var commonPragmas = []string{
	"journal_mode(WAL)",
	"busy_timeout(5000)",
	"synchronous(NORMAL)",
	"foreign_keys(1)",
}

// dsn builds a file: URI for path with the shared pragmas plus extras.
func dsn(path string, txImmediate bool, extra ...string) string {
	q := url.Values{}
	for _, p := range append(append([]string{}, commonPragmas...), extra...) {
		q.Add("_pragma", p)
	}
	if txImmediate {
		q.Set("_txlock", "immediate")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// Open opens (creating if needed) DATA/serverdock.db, takes the one-server
// lock, wipes leftovers from a crashed run, and applies pending migrations
// (see migrate.go). dir must exist.
func Open(ctx context.Context, dir string) (*Store, error) {
	return openWith(ctx, dir, Options{})
}

func openWith(ctx context.Context, dir string, opts Options) (*Store, error) {
	opts.defaults()
	path, err := filepath.Abs(filepath.Join(dir, fileName))
	if err != nil {
		return nil, err
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			unlock(lock)
		}
	}()
	if err := wipeLeftovers(dir); err != nil {
		return nil, err
	}
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	if err := migrate(ctx, w, dir, opts); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path, false, "query_only(1)"))
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	r.SetMaxOpenConns(readerConns)
	if err := r.PingContext(ctx); err != nil {
		w.Close()
		r.Close()
		return nil, fmt.Errorf("store: ping reader: %w", err)
	}
	ok = true
	return &Store{W: w, R: r, Dir: dir, lock: lock}, nil
}

// lockDir takes an exclusive flock on DATA/serverdock.lock. The file is 0600
// and owned by the service user (an existing one is tightened here): if other
// users could open it, any of them could hold a shared lock and keep the server
// from starting. A read-only tool running as another user therefore cannot probe
// it and falls back to the -wal heuristic in OpenReadOnly.
func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: lock file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, fmt.Errorf("store: lock file mode: %w", err)
	}
	// A read-only probe (audit verify from cron) holds a shared lock for a moment;
	// wait it out instead of failing a starting server.
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("store: lock %s: %w", dir, err)
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, fmt.Errorf("%w: %s", ErrLocked, dir)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ErrLocked is returned by Open when another ServerDock process (the server, or
// `admin reset`) holds the data-directory lock.
var ErrLocked = errors.New("store: another ServerDock process is already using the data directory")

// lockWait is how long lockDir waits on a contended lock (a variable for tests).
var lockWait = time.Second

func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// wipeLeftovers removes what a crashed run can leave behind: half-written
// backups (backups/*.tmp) and the scratch directory DATA/tmp.
func wipeLeftovers(dir string) error {
	tmps, err := filepath.Glob(filepath.Join(dir, backupDir, "*.tmp"))
	if err != nil {
		return err
	}
	for _, p := range tmps {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: wipe %s: %w", p, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, "tmp")); err != nil {
		return fmt.Errorf("store: wipe tmp: %w", err)
	}
	return nil
}

// WriteTx runs fn in a BEGIN IMMEDIATE transaction on the single writer,
// committing if fn returns nil and rolling back on an error or a panic.
func (s *Store) WriteTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.W.BeginTx(ctx, nil) // _txlock=immediate
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Close closes both pools and releases the one-server lock.
func (s *Store) Close() error {
	rerr := s.R.Close()
	werr := s.W.Close()
	if s.lock != nil {
		unlock(s.lock)
		s.lock = nil
	}
	if werr != nil {
		return werr
	}
	return rerr
}
