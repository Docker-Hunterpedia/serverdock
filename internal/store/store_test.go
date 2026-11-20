package store

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func pragma(t *testing.T, c *sql.Conn, name string) string {
	t.Helper()
	var v string
	if err := c.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(v)
}

func checkPragmas(t *testing.T, c *sql.Conn, label string) {
	t.Helper()
	want := map[string]string{
		"journal_mode": "wal",
		"busy_timeout": "5000",
		"synchronous":  "1",
		"foreign_keys": "1",
	}
	for k, v := range want {
		if got := pragma(t, c, k); got != v {
			t.Errorf("%s: PRAGMA %s = %q, want %q", label, k, got, v)
		}
	}
}

func TestPragmasOnEveryPooledConnection(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	wc, err := s.W.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkPragmas(t, wc, "writer")
	wc.Close()

	// Hold both reader connections at once so they are distinct.
	var conns []*sql.Conn
	for i := 0; i < readerConns; i++ {
		c, err := s.R.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		checkPragmas(t, c, "reader")
		if got := pragma(t, c, "query_only"); got != "1" {
			t.Errorf("reader query_only = %q", got)
		}
	}
	for _, c := range conns {
		c.Close()
	}
}

func TestWriterPoolIsSingleConnection(t *testing.T) {
	s := open(t)
	if got := s.W.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("writer MaxOpenConnections = %d", got)
	}
	if got := s.R.Stats().MaxOpenConnections; got != readerConns {
		t.Fatalf("reader MaxOpenConnections = %d", got)
	}
}

func TestMetaCreated(t *testing.T) {
	s := open(t)
	var v string
	if err := s.R.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err != nil || v != "1" {
		t.Fatalf("schema_version = %q, %v", v, err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.W.Exec(`INSERT INTO meta VALUES ('k','v')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v string
	if err := s.R.QueryRow(`SELECT value FROM meta WHERE key='k'`).Scan(&v); err != nil || v != "v" {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestReaderCannotWrite(t *testing.T) {
	s := open(t)
	if _, err := s.R.Exec(`INSERT INTO meta VALUES ('x','y')`); err == nil {
		t.Fatal("reader pool accepted a write")
	}
}

func TestConcurrentReadsDuringWrite(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	tx, err := s.W.BeginTx(ctx, nil) // BEGIN IMMEDIATE via _txlock
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO meta VALUES ('pending','1')`); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			var n int
			if err := s.R.QueryRowContext(rctx, `SELECT count(*) FROM meta WHERE key='pending'`).Scan(&n); err != nil {
				errs <- err
				return
			}
			if n != 0 {
				errs <- sql.ErrNoRows // uncommitted data visible
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("reader during write: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.R.QueryRow(`SELECT count(*) FROM meta WHERE key='pending'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("after commit n=%d err=%v", n, err)
	}
}

func TestConcurrentWritersSerialize(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := s.W.Begin()
			if err != nil {
				t.Error(err)
				return
			}
			defer tx.Rollback()
			if _, err := tx.Exec(`INSERT INTO meta VALUES (?, 'x')`, "w"+string(rune('a'+i))); err != nil {
				t.Error(err)
				return
			}
			if err := tx.Commit(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n int
	if err := s.R.QueryRow(`SELECT count(*) FROM meta`).Scan(&n); err != nil || n != 11 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestDSNEscapesPath(t *testing.T) {
	d := dsn("/tmp/with space/db?x#.db", true)
	if !strings.Contains(d, "%20") || strings.Contains(d, "db?x") {
		t.Fatalf("path not escaped: %s", d)
	}
}
