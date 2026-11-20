// Package store opens the SQLite database (pure Go, WAL) as one writer pool
// and one small reader pool. Pragmas are set through the DSN so that every
// pooled connection gets them.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const (
	fileName    = "serverdock.db"
	readerConns = 2
)

// Store holds the writer and reader pools.
type Store struct {
	// W has MaxOpenConns(1) and BEGIN IMMEDIATE transactions.
	W *sql.DB
	// R is a small read-only pool.
	R *sql.DB
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

// Open opens (creating if needed) DATA/serverdock.db and runs the migrations
// stub. dir must exist.
func Open(ctx context.Context, dir string) (*Store, error) {
	path, err := filepath.Abs(filepath.Join(dir, fileName))
	if err != nil {
		return nil, err
	}
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	if err := migrate(ctx, w); err != nil {
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
	return &Store{W: w, R: r}, nil
}

// Close closes both pools.
func (s *Store) Close() error {
	rerr := s.R.Close()
	if werr := s.W.Close(); werr != nil {
		return werr
	}
	return rerr
}

// migrate is a stub: it creates the meta table and records schema version 1.
func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', '1')`); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return tx.Commit()
}
