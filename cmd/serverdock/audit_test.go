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

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
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
