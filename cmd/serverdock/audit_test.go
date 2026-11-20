package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
	"github.com/Docker-Hunterpedia/serverdock/internal/testbin"
)

func listDir(t *testing.T, dir string) []string {
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

func liveStoreWithAudit(t *testing.T, dir string, rows int) (*store.Store, *audit.Logger) {
	t.Helper()
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	l, err := audit.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := l.Append(context.Background(), audit.Entry{ActorKind: "user", ActorID: "u", Action: "app.x", Target: "t", Risk: "mutate",
			Phase: audit.PhaseIntent, Result: audit.ResultPending, ArgsJSON: "{}", RequestID: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	return st, l
}

func TestAuditVerifyExitCodes(t *testing.T) {
	dir := t.TempDir()
	st, _ := liveStoreWithAudit(t, dir, 4)
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := auditMain(args, &out, &errb)
		return code, out.String(), errb.String()
	}

	// 0: valid, against a RUNNING server's data dir; no files created; the server keeps writing.
	before := listDir(t, dir)
	code, out, errs := run("verify", "--data", dir)
	if code != 0 || !strings.Contains(out, "ok: 4 rows, head ") {
		t.Fatalf("valid chain: %d %q %q", code, out, errs)
	}
	if after := listDir(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("verify created files: %v -> %v", before, after)
	}
	if _, err := st.W.Exec(`INSERT INTO meta VALUES ('still','writing')`); err != nil {
		t.Errorf("server cannot write after verify: %v", err)
	}

	// 1: broken.
	for _, q := range []string{`DROP TRIGGER audit_log_no_update`, `UPDATE audit_log SET args_json='{"x":1}' WHERE id=2`} {
		if _, err := st.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ = run("verify", "--data", dir)
	if code != 1 || !strings.Contains(out, "BROKEN") || !strings.Contains(out, "row id 2") {
		t.Fatalf("broken chain: %d %q", code, out)
	}

	// 2: could not check.
	for _, args := range [][]string{
		{"verify", "--data", filepath.Join(dir, "missing")},
		{},
		{"nope"},
		{"verify", "extra"},
		{"verify", "--bogus"},
	} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestAuditVerifyOnAPhase0DatabaseCannotCheck(t *testing.T) {
	dir := t.TempDir()
	// A database as Phase 0 left it (meta only), not yet migrated.
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	st.W.Exec(`DROP TABLE audit_log`)
	st.W.Exec(`DROP TABLE schema_migrations`)
	st.Close()
	var out, errb bytes.Buffer
	if code := auditMain([]string{"verify", "--data", dir}, &out, &errb); code != 2 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errb.String())
	}
}

func TestRunDispatchesAudit(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"audit"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "audit verify") {
		t.Errorf("code %d %q", code, errb.String())
	}
}

// As root, run verify as uid 65534 against a live root-owned server data dir:
// it must succeed, create no files, and leave the server able to write.
func TestAuditVerifyAsAnotherUser(t *testing.T) {
	if os.Getenv("SD_AUDIT_HELPER") != "" {
		return
	}
	if os.Getuid() != 0 {
		t.Skip("needs root to run verify as another uid (see the store package test for how to run it)")
	}
	base, err := os.MkdirTemp("", "sdau")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	os.Chmod(base, 0o755)
	dir := filepath.Join(base, "data")
	os.Mkdir(dir, 0o755)
	st, _ := liveStoreWithAudit(t, dir, 3)
	bin := filepath.Join(base, "serverdock.test")
	src, _ := os.Open(os.Args[0])
	dst, _ := os.OpenFile(bin, os.O_CREATE|os.O_WRONLY, 0o755)
	io.Copy(dst, src)
	dst.Close()
	src.Close()
	before := listDir(t, dir)
	cmd := exec.Command(bin, "-test.run=^TestAuditVerifyHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "SD_AUDIT_HELPER="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "VERIFY-OK ok: 3 rows") {
		t.Fatalf("helper as uid 65534: %v\n%s", err, out)
	}
	if after := listDir(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("the other user created files: %v -> %v", before, after)
	}
	if _, err := st.W.Exec(`INSERT INTO meta VALUES ('after','verify')`); err != nil {
		t.Errorf("server cannot write: %v", err)
	}
}

func TestAuditVerifyHelper(t *testing.T) {
	dir := os.Getenv("SD_AUDIT_HELPER")
	if dir == "" {
		t.Skip("helper for TestAuditVerifyAsAnotherUser")
	}
	var out, errb bytes.Buffer
	if code := auditMain([]string{"verify", "--data", dir}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errb.String())
	}
	os.Stdout.WriteString("VERIFY-OK " + out.String())
}

// An immutable open (chosen because no server was running) that sees a broken
// chain is repeated once before exit 1: a server starting in between can look
// like corruption. A normal open is not repeated.
func TestAuditVerifyRechecksAnAutomaticImmutableOpen(t *testing.T) {
	dir := t.TempDir()
	st, _ := liveStoreWithAudit(t, dir, 3)
	st.Close() // a quiet database: OpenReadOnly will choose immutable on its own

	// A tampered copy, standing in for a half-written view of the live database.
	bad := t.TempDir()
	copyDB(t, dir, bad)
	tamper := mustOpenStore(t, bad)
	tamper.W.Exec(`DROP TRIGGER audit_log_no_update`)
	tamper.W.Exec(`UPDATE audit_log SET args_json='{"x":1}' WHERE id=2`)
	tamper.Close()

	opens := 0
	open := func(auto bool, dirs ...string) openRO {
		return func(_ string, o store.ReadOnlyOptions) (*store.ReadOnly, error) {
			d := dirs[min(opens, len(dirs)-1)]
			opens++
			ro, err := store.OpenReadOnly(d, o)
			if ro != nil {
				ro.AutoImmutable = auto
			}
			return ro, err
		}
	}
	run := func(o openRO) (int, string) {
		var out, errb bytes.Buffer
		code := auditVerifyCmd([]string{"verify", "--data", dir}, &out, &errb, o)
		return code, out.String() + errb.String()
	}

	// First read looks broken, the second is clean: exit 0, opened twice.
	opens = 0
	if code, out := run(open(true, bad, dir)); code != 0 || opens != 2 || !strings.Contains(out, "ok: 3 rows") {
		t.Fatalf("recheck: code %d opens %d: %s", code, opens, out)
	}
	// Broken both times: exit 1 after exactly one repeat.
	opens = 0
	if code, out := run(open(true, bad)); code != 1 || opens != 2 || !strings.Contains(out, "BROKEN") {
		t.Fatalf("still broken: code %d opens %d: %s", code, opens, out)
	}
	// Not an automatic immutable open (a live server): no repeat.
	opens = 0
	if code, _ := run(open(false, bad)); code != 1 || opens != 1 {
		t.Fatalf("live open: code %d opens %d, want 1 and 1", code, opens)
	}
}

func mustOpenStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func copyDB(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(from, "serverdock.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to, "serverdock.db"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The production case: the server runs as the service user on a real 0700 DATA,
// and an operator runs `audit verify` with sudo (root). Verify must succeed and
// must not leave a root-owned file (a -shm, say) that would later stop the
// service user's server from opening its own database.
func TestAuditVerifyAsRootOnServiceOwnedData(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	if os.Getuid() != 0 {
		t.Skip("needs root (run it in a container: docker run --rm -v $PWD:/src -w /src golang:1.27 go test ./cmd/serverdock -run AuditVerifyAsRoot)")
	}
	const svc = 65534
	base, err := os.MkdirTemp("", "sdar")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	os.Chmod(base, 0o755)
	data := filepath.Join(base, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(data, svc, svc); err != nil {
		t.Fatal(err)
	}
	p := testbin.Start(t, testbin.Options{Uid: svc, Args: []string{"server", "--data", data, "--internal", testbin.FreeAddr(t), "--proxy=false"}})
	if err := p.WaitLog("control plane listening", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	before := listDir(t, data)
	res := testbin.Run(t, testbin.Options{Args: []string{"audit", "verify", "--data", data}})
	if res.Code != 0 || !strings.HasPrefix(res.Stdout, "ok:") {
		t.Fatalf("verify as root: exit %d\n%s%s", res.Code, res.Stdout, res.Stderr)
	}
	if after := listDir(t, data); !reflect.DeepEqual(before, after) {
		t.Errorf("verify created files: %v -> %v", before, after)
	}
	filepath.WalkDir(data, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if st, err := d.Info(); err == nil {
			if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Uid == 0 {
				t.Errorf("%s is owned by root", path)
			}
		}
		return nil
	})
	if code := p.Stop(); code != 0 {
		t.Fatalf("server exit %d\n%s", code, p.Logs())
	}
}
