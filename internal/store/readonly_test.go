package store

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestReadOnlyNextToALiveWriter(t *testing.T) {
	dir := t.TempDir()
	s, err := openWithT(t, dir, Options{Migrations: list(mParentChild)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.W.Exec(`INSERT INTO parent VALUES (1,'live')`); err != nil {
		t.Fatal(err)
	}
	before := ls(t, dir)
	ro, err := openReadOnly(dir, ReadOnlyOptions{}, list(mParentChild))
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := ro.DB.QueryRow(`SELECT name FROM parent WHERE id=1`).Scan(&name); err != nil || name != "live" {
		t.Fatalf("read next to the writer: %q %v", name, err)
	}
	if ro.Schema != "v2" {
		t.Errorf("schema %q", ro.Schema)
	}
	if _, err := ro.DB.Exec(`INSERT INTO parent VALUES (2,'nope')`); err == nil {
		t.Error("the read-only handle accepted a write")
	}
	// The writer keeps working, and the reader sees later commits.
	if _, err := s.W.Exec(`INSERT INTO parent VALUES (3,'later')`); err != nil {
		t.Fatalf("writer after the read-only open: %v", err)
	}
	var n int
	ro.DB.QueryRow(`SELECT count(*) FROM parent`).Scan(&n)
	if n != 2 {
		t.Errorf("reader sees %d rows, want 2", n)
	}
	ro.Close()
	if after := ls(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("files changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestReadOnlyUsesImmutableWithoutAWAL(t *testing.T) {
	dir := t.TempDir()
	s, _ := openWithT(t, dir, Options{Migrations: list(mParentChild)})
	s.Close()
	if _, err := os.Stat(filepath.Join(dir, fileName+"-wal")); err == nil {
		t.Skip("SQLite left a -wal file after the last close")
	}
	before := ls(t, dir)
	ro, err := openReadOnly(dir, ReadOnlyOptions{}, list(mParentChild))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var one int
	if err := ro.DB.QueryRow(`SELECT 1 FROM schema_migrations LIMIT 1`).Scan(&one); err != nil {
		t.Fatal(err)
	}
	if after := ls(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("a file was created: before %v after %v", before, after)
	}
}

func TestReadOnlyRefusesADirtyWALWithNoServer(t *testing.T) {
	live := t.TempDir()
	s, err := openWithT(t, live, Options{Migrations: list(mParentChild)})
	if err != nil {
		t.Fatal(err)
	}
	s.W.Exec(`INSERT INTO parent VALUES (1,'in the wal')`)
	// Copy the database and its WAL while the server is up: a crash image.
	scratch := t.TempDir()
	for _, f := range []string{fileName, fileName + "-wal"} {
		b, err := os.ReadFile(filepath.Join(live, f))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(scratch, f), b, 0o644)
	}
	if st, _ := os.Stat(filepath.Join(scratch, fileName+"-wal")); st.Size() == 0 {
		t.Skip("the WAL was already checkpointed")
	}
	before := ls(t, scratch)
	if _, err := OpenReadOnly(scratch, ReadOnlyOptions{}); err == nil || !strings.Contains(err.Error(), "write-ahead log") {
		t.Fatalf("dirty WAL, no server: %v", err)
	}
	if after := ls(t, scratch); !reflect.DeepEqual(before, after) {
		t.Errorf("the refusal created files: %v -> %v", before, after)
	}
}

func TestReadOnlyReportsAnUnadoptedPhase0Database(t *testing.T) {
	dir := t.TempDir()
	makePhase0(t, dir, 2)
	before := ls(t, dir)
	ro, err := OpenReadOnly(dir, ReadOnlyOptions{Immutable: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if ro.Schema != "v1 (unadopted)" {
		t.Fatalf("schema = %q", ro.Schema)
	}
	if after := ls(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("files changed: %v -> %v", before, after)
	}
}

func TestReadOnlyNeverCreatesAMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenReadOnly(dir, ReadOnlyOptions{}); err == nil {
		t.Fatal("opened a missing database")
	}
	if got := ls(t, dir); len(got) != 1 { // just "."
		t.Errorf("files created: %v", got)
	}
}

func TestReadOnlyRefusesANewerSchema(t *testing.T) {
	dir := t.TempDir()
	s, _ := openWithT(t, dir, Options{})
	s.W.Exec(`INSERT INTO schema_migrations VALUES (42, '0042_future', 'x', 'now')`)
	s.Close()
	if _, err := OpenReadOnly(dir, ReadOnlyOptions{Immutable: true}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema: %v", err)
	}
}

func TestReadOnlyRejectsAnEmptyOrForeignDatabase(t *testing.T) {
	dir := t.TempDir()
	db, _ := sql.Open("sqlite", "file:"+filepath.Join(dir, fileName))
	db.Exec(`CREATE TABLE unrelated (x)`)
	db.Close()
	if _, err := OpenReadOnly(dir, ReadOnlyOptions{Immutable: true}); err == nil {
		t.Error("a foreign database was accepted")
	}
}

// A directory the caller cannot write to (as for another user): nothing is
// created, and the server can write afterwards.
func TestReadOnlyOnAnUnwritableDirectory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions; TestReadOnlyAsAnotherUser covers root")
	}
	dir := t.TempDir()
	s, err := openWithT(t, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	before := ls(t, dir)
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o700)
	ro, err := OpenReadOnly(dir, ReadOnlyOptions{})
	if err != nil {
		t.Fatalf("read-only open in an unwritable directory: %v", err)
	}
	ro.Close()
	os.Chmod(dir, 0o700)
	if after := ls(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("files changed: %v -> %v", before, after)
	}
	if _, err := s.W.Exec(`INSERT INTO meta VALUES ('still','writes')`); err != nil {
		t.Errorf("server cannot write afterwards: %v", err)
	}
}

// The real thing: a live server owned by root, the read-only open run as uid
// 65534. Needs root (the CI runners and a dev laptop are not root, so it skips;
// run it with `docker run --rm -v $PWD:/src -w /src golang go test ./internal/store`).
func TestReadOnlyAsAnotherUser(t *testing.T) {
	if os.Getenv("SD_RO_HELPER") != "" {
		return // the helper process runs TestReadOnlyHelper instead
	}
	if os.Getuid() != 0 {
		t.Skip("needs root to run the read-only open as another uid")
	}
	// Another uid must be able to traverse to the data dir and the helper binary,
	// and t.TempDir's parent is 0700, so build a world-searchable tree.
	base, err := os.MkdirTemp("", "sdro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	os.Chmod(base, 0o755)
	dir := filepath.Join(base, "data")
	os.Mkdir(dir, 0o755)
	s, err := openWithT(t, dir, Options{Migrations: list(mParentChild)})
	if err != nil {
		t.Fatal(err)
	}
	s.W.Exec(`INSERT INTO parent VALUES (1,'root wrote this')`)
	// The helper is this test binary, copied somewhere another uid can execute.
	bin := filepath.Join(base, "store.test")
	src, _ := os.Open(os.Args[0])
	dst, _ := os.OpenFile(bin, os.O_CREATE|os.O_WRONLY, 0o755)
	io.Copy(dst, src)
	dst.Close()
	src.Close()
	before := ls(t, dir)
	cmd := exec.Command(bin, "-test.run=^TestReadOnlyHelper$")
	cmd.Env = append(os.Environ(), "SD_RO_HELPER="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "RO-OK v2 1") {
		t.Fatalf("helper as uid 65534: %v\n%s", err, out)
	}
	if after := ls(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("the other user created files: %v -> %v", before, after)
	}
	if _, err := s.W.Exec(`INSERT INTO parent VALUES (2,'still writable')`); err != nil {
		t.Errorf("the server cannot write after the other user's read: %v", err)
	}
}

func TestReadOnlyHelper(t *testing.T) {
	dir := os.Getenv("SD_RO_HELPER")
	if dir == "" {
		t.Skip("helper for TestReadOnlyAsAnotherUser")
	}
	ro, err := openReadOnly(dir, ReadOnlyOptions{}, list(mParentChild))
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	var n int
	if err := ro.DB.QueryRow(`SELECT count(*) FROM parent`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("RO-OK %s %d\n", ro.Schema, n)
}
