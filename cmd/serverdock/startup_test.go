package main

import (
	"context"
	"crypto/sha256"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/testbin"
)

// dataFingerprint hashes every regular file under dir (except the database,
// its sidecars and the lock, which a running server changes by itself) with its
// mode, so a test can prove nothing else was touched.
func dataFingerprint(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if strings.HasPrefix(rel, "serverdock.") { // db, -wal, -shm, lock
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		st, _ := d.Info()
		out[rel] = st.Mode().String() + " " + string(sha256.New().Sum(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A second server on the same DATA fails on the data-directory lock before it
// writes anything: the first server's token file and keys are unchanged and the
// first still answers.
func TestSecondServerFailsOnLockAndFirstStillAnswers(t *testing.T) {
	data := newData(t)
	addrs, stop := startServer(t, serverConfig{Data: data, Internal: "127.0.0.1:0",
		AgentListen: "127.0.0.1:0", AgentSANs: []string{"127.0.0.1"}})
	before := dataFingerprint(t, data)
	for _, want := range []string{"agent.token", "pki/ca.key", "pki/hub.pem", "keys/master.key"} {
		if _, ok := before[want]; !ok {
			t.Fatalf("first server did not create %s: %v", want, before)
		}
	}

	// A different SAN would make an unlocked second server rewrite pki/hub.pem.
	err := runServer(context.Background(), serverConfig{Data: data, Internal: "127.0.0.1:0",
		AgentListen: "127.0.0.1:0", AgentSANs: []string{"127.0.0.2", "other.example"}},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err == nil || !strings.Contains(err.Error(), "already using") {
		t.Fatalf("second server: %v", err)
	}

	after := dataFingerprint(t, data)
	if len(before) != len(after) {
		t.Fatalf("files changed: %v -> %v", before, after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed by the second server", k)
		}
	}
	resp, err := http.Get("http://" + addrs.Internal + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("first server stopped answering: %v %v", resp, err)
	}
	resp.Body.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestMasterKeyCreatedAtStartAndKept(t *testing.T) {
	data := newData(t)
	_, stop := startServer(t, serverConfig{Data: data, Internal: "127.0.0.1:0"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(data, "keys", "master.key"))
	if err != nil || len(key) != 32 {
		t.Fatalf("%v %d", err, len(key))
	}
	_, stop = startServer(t, serverConfig{Data: data, Internal: "127.0.0.1:0"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(data, "keys", "master.key"))
	if string(again) != string(key) {
		t.Fatal("the master key changed on restart")
	}
	// A damaged key stops the server and is not replaced.
	os.WriteFile(filepath.Join(data, "keys", "master.key"), key[:10], 0o600)
	err = runServer(context.Background(), serverConfig{Data: data, Internal: "127.0.0.1:0"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err == nil || !strings.Contains(err.Error(), "wrong size") {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "keys", "master.key")); len(b) != 10 {
		t.Fatal("the damaged key was replaced")
	}
}

// The umask is process-wide, so this runs the built binary: everything the
// server creates under a DATA it created itself is private to the owner.
func TestUmaskKeepsNewFilesPrivate(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	data := filepath.Join(testbin.ShortDir(t), "data") // does not exist yet: Prepare creates it
	p := testbin.Start(t, testbin.Options{Args: []string{"server", "--data", data, "--internal", testbin.FreeAddr(t),
		"--listen", testbin.FreeAddr(t), "--agent-listen", "127.0.0.1:0", "--agent-san", "127.0.0.1"}})
	if err := p.WaitLog("control plane listening", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if code := p.Stop(); code != 0 {
		t.Fatalf("exit %d\n%s", code, p.Logs())
	}
	if st, err := os.Stat(data); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("DATA: %v %v", st, err)
	}
	n := 0
	filepath.WalkDir(data, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Error(err)
			return nil
		}
		st, _ := d.Info()
		n++
		rel, _ := filepath.Rel(data, path)
		// The hub CA certificate and its fingerprint are public by design (agents
		// pin them) and are chmod-ed explicitly; they sit inside the 0700 DATA.
		if rel == "pki/ca.crt" || rel == "pki/ca.fingerprint" {
			return nil
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v: group or others can access it", rel, st.Mode())
		}
		return nil
	})
	if n < 8 {
		t.Errorf("only %d entries under DATA; the walk missed something", n)
	}
	for _, name := range []string{"keys/master.key", "agent.token", "pki/ca.key", "pki/hub.pem"} {
		if st, err := os.Stat(filepath.Join(data, name)); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", name, st, err)
		}
	}
}

func TestBinaryRefusesLooseDataDirWithoutTouchingIt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	data := filepath.Join(testbin.ShortDir(t), "data")
	os.Mkdir(data, 0o755)
	os.Chmod(data, 0o755)
	res := testbin.Run(t, testbin.Options{Args: []string{"server", "--data", data, "--internal", testbin.FreeAddr(t), "--proxy=false"}})
	if res.Code != 1 || !strings.Contains(res.Stderr, "chmod 700 "+data) {
		t.Fatalf("exit %d\n%s", res.Code, res.Stderr)
	}
	if ents, _ := os.ReadDir(data); len(ents) != 0 {
		t.Fatalf("a refused start wrote into DATA: %v", ents)
	}
	if st, _ := os.Stat(data); st.Mode().Perm() != 0o755 {
		t.Fatalf("the directory was tightened silently: %v", st.Mode())
	}
}
