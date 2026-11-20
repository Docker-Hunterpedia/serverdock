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
)

// ReadOnlyOptions tunes OpenReadOnly.
type ReadOnlyOptions struct {
	// Immutable opens with immutable=1 even if a -wal file exists. Use it only
	// on a private copy nothing else is writing (rehearse restore).
	Immutable bool
}

// ReadOnly is a database opened for reading only.
type ReadOnly struct {
	DB *sql.DB
	// Schema is "v<N>", or "v1 (unadopted)" for a Phase 0 database that has
	// not been through a migrating start yet.
	Schema string
	// AutoImmutable is true when OpenReadOnly chose immutable=1 itself (no -wal,
	// or an empty one, and no server) rather than being told to. A server may
	// start between that check and the read, so a caller that reports corruption
	// should re-open once before believing it.
	AutoImmutable bool
}

// Close closes the handle.
func (r *ReadOnly) Close() error { return r.DB.Close() }

func roDSN(path string, immutable bool) string {
	q := url.Values{}
	q.Set("mode", "ro")
	if immutable {
		q.Set("immutable", "1")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// serverRunning reports whether a server holds DATA/serverdock.lock. It only
// opens the lock file read-only (never creates it). The file is 0600, so as
// another user the open fails with a permission error; then the best evidence
// left is the write-ahead log: a non-empty -wal means a server may be running,
// and the caller reads with mode=ro&readonly_shm=1, which cannot write.
func serverRunning(dir string, walSize int64) (bool, error) {
	f, err := os.Open(filepath.Join(dir, lockName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if errors.Is(err, os.ErrPermission) {
		return walSize > 0, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

// OpenReadOnly opens DATA/serverdock.db for reading without ever writing: no
// pragmas that write (in particular not journal_mode=WAL), no migrations, no
// flock, and it never creates a file (a -wal or -shm created by a different
// user would break the server's next start).
//
//   - a server is running (it holds the lock; -wal and -shm exist): mode=ro;
//   - no -wal file, or opts.Immutable: immutable=1 (nothing can be changing);
//   - a non-empty -wal and no server: refused, because reading it would need a
//     writable -shm. Start the server once, or copy the database to scratch.
//
// The schema must be one this version knows; a newer one is an error.
func OpenReadOnly(dir string, opts ReadOnlyOptions) (*ReadOnly, error) {
	return openReadOnly(dir, opts, builtin)
}

func openReadOnly(dir string, opts ReadOnlyOptions, ms []Migration) (*ReadOnly, error) {
	path, err := filepath.Abs(filepath.Join(dir, fileName))
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	immutable := opts.Immutable
	auto := false
	if !immutable {
		wal, werr := os.Stat(path + "-wal")
		switch {
		case errors.Is(werr, os.ErrNotExist):
			immutable, auto = true, true
		case werr != nil:
			return nil, fmt.Errorf("store: %w", werr)
		default:
			running, err := serverRunning(dir, wal.Size())
			if err != nil {
				return nil, fmt.Errorf("store: cannot tell whether a server is running: %w", err)
			}
			switch {
			case running:
			case wal.Size() == 0:
				immutable, auto = true, true
			default:
				return nil, errors.New("store: the database has an unmerged write-ahead log and no server is running; start the server once, or copy the database to a scratch directory and open that")
			}
		}
	}
	dsnRO := roDSN(path, immutable) + "&_pragma=query_only(1)"
	if !immutable {
		// A live server owns the -shm; never try to initialise or write it.
		dsnRO += "&readonly_shm=1"
	}
	db, err := sql.Open("sqlite", dsnRO)
	if err != nil {
		return nil, fmt.Errorf("store: open read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	schema, err := roSchema(context.Background(), db, ms)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &ReadOnly{DB: db, Schema: schema, AutoImmutable: auto}, nil
}

func roSchema(ctx context.Context, db *sql.DB, ms []Migration) (string, error) {
	fresh, phase0, rows, err := schemaState(ctx, db)
	if err != nil {
		return "", fmt.Errorf("store: reading the schema: %w", err)
	}
	switch {
	case fresh:
		return "", errors.New("store: the database is empty (no schema)")
	case phase0:
		return "v1 (unadopted)", nil
	}
	max, err := verifyApplied(ms, rows)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("v%d", max), nil
}
