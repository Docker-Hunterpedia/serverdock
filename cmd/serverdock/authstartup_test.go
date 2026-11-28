package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
	"github.com/Docker-Hunterpedia/serverdock/internal/testbin"
)

func readSetupToken(t *testing.T, data string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(data, "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// The setup token is written at start, and the log line has its path, the
// expiry and the panel fingerprint, never the token.
func TestServerWritesSetupTokenAndLogsOnlyItsPath(t *testing.T) {
	data := newData(t)
	_, stop, logs := panelServer(t, data)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	tok := readSetupToken(t, data)
	if !strings.HasPrefix(tok, "sd_setup_") {
		t.Fatalf("token %q", tok)
	}
	out := logs.String()
	fp := panelFingerprintFile(t, data)
	for _, want := range []string{"no admin yet", filepath.Join(data, "setup-token"), "UTC", "panel certificate SHA-256 " + fp, "ssh -L 8443:127.0.0.1:"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, tok) || strings.Contains(out, strings.TrimPrefix(tok, "sd_setup_")) {
		t.Error("the setup token was logged")
	}
	// A restart writes a new token.
	_, stop, _ = panelServer(t, data)
	stop()
	if readSetupToken(t, data) == tok {
		t.Error("the token did not change on restart")
	}
}

// A first-run server's whole tree, from the built binary: every file 0600 and
// every directory 0700 (except the two public CA files), the setup token among
// them, and nothing secret in the log.
func TestFileModesAfterFirstRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	data := filepath.Join(testbin.ShortDir(t), "data")
	ports := testbin.FreeAddrs(t, 2)
	p := testbin.Start(t, testbin.Options{Args: []string{"server", "--data", data, "--listen", ports[0], "--https-listen", ports[1],
		"--agent-listen", "127.0.0.1:0", "--agent-san", "127.0.0.1", "--panel-host", "panel.example.com"}})
	if err := p.WaitLog("control plane listening", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := p.WaitLog("proxy listening", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	// Let SQLite create its sidecars, then look at everything.
	want := map[string]os.FileMode{
		"":                      0o700,
		"keys":                  0o700,
		"keys/master.key":       0o600,
		"pki":                   0o700,
		"pki/ca.key":            0o600,
		"pki/hub.pem":           0o600,
		"pki/panel.pem":         0o600,
		"pki/panel.fingerprint": 0o600,
		"run":                   0o700,
		"run/cp.sock":           0o600 | fs.ModeSocket,
		"setup-token":           0o600,
		"agent.token":           0o600,
		"serverdock.db":         0o600,
		"serverdock.lock":       0o600,
	}
	seen := map[string]bool{}
	filepath.WalkDir(data, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(data, path)
		if rel == "." {
			rel = ""
		}
		st, _ := d.Info()
		seen[rel] = true
		switch rel {
		case "pki/ca.crt", "pki/ca.fingerprint": // public by design, inside the 0700 tree
			return nil
		}
		mode := st.Mode().Perm() | st.Mode()&fs.ModeSocket
		if w, ok := want[rel]; ok {
			if mode != w {
				t.Errorf("%s: mode %v, want %v", rel, mode, w)
			}
		} else if d.IsDir() && mode != 0o700 || !d.IsDir() && st.Mode().Perm()&0o077 != 0 {
			t.Errorf("unexpected entry %s with mode %v", rel, st.Mode())
		}
		return nil
	})
	for rel := range want {
		if !seen[rel] {
			t.Errorf("%s was not created", rel)
		}
	}
	tok := readSetupToken(t, data)
	if strings.Contains(p.Logs(), tok) || strings.Contains(p.Logs(), strings.TrimPrefix(tok, "sd_setup_")) {
		t.Error("the setup token is in the server log")
	}
	if code := p.Stop(); code != 0 {
		t.Fatalf("exit %d\n%s", code, p.Logs())
	}
}

// With a recorded key id, or with ciphertexts in the database, a missing master
// key stops the start and nothing is created; with neither it is created again.
func TestMissingKeyWithCiphertextsRefusesStart(t *testing.T) {
	data := newData(t)
	cfg := func() serverConfig { return serverConfig{Data: data, ControlSocket: shortSock(t)} }
	_, stop := startServer(t, cfg())
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	// Put a sealed pending seed in the database through the real code path.
	ctx := context.Background()
	st, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(secretbox.KeyPath(data))
	box, _ := secretbox.New(key)
	as := auth.NewStore(st, box, auth.NewHasher(), auth.Options{})
	if _, err := as.SetupEnroll(ctx, readSetupToken(t, data), "admin"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	keyPath := secretbox.KeyPath(data)
	os.Remove(keyPath)
	// 1. The recorded id alone refuses.
	err = runServer(ctx, cfg(), quietLog(), nil)
	if err == nil || !strings.Contains(err.Error(), "records its key") {
		t.Fatalf("with a recorded id: %v", err)
	}
	// 2. With the id row gone, the ciphertext alone refuses.
	st, _ = store.Open(ctx, data)
	st.W.Exec(`DELETE FROM meta WHERE key = 'secretbox_key_id'`)
	st.Close()
	err = runServer(ctx, cfg(), quietLog(), nil)
	if err == nil || !strings.Contains(err.Error(), "missing but encrypted data exists") || !strings.Contains(err.Error(), "admin reset") {
		t.Fatalf("with ciphertexts only: %v", err)
	}
	if _, err := os.Stat(keyPath); err == nil {
		t.Fatal("a key was created although encrypted data exists")
	}
	// 3. With the ciphertext cleared too (what admin reset does), a new key is made.
	st, _ = store.Open(ctx, data)
	st.W.Exec(`UPDATE setup_state SET pending_seed = NULL, enrollment_hash = NULL`)
	st.Close()
	_, stop = startServer(t, cfg())
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(keyPath); err != nil || len(b) != 32 {
		t.Fatalf("no new key: %v", err)
	}
}

// A key file swapped for another key is refused at start, with a clear message.
func TestSwappedKeyRefusesStart(t *testing.T) {
	data := newData(t)
	cfg := serverConfig{Data: data, ControlSocket: shortSock(t)}
	_, stop := startServer(t, cfg)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	other := make([]byte, 32)
	other[0] = 1
	if err := os.WriteFile(secretbox.KeyPath(data), other, 0o600); err != nil {
		t.Fatal(err)
	}
	err := runServer(context.Background(), cfg, quietLog(), nil)
	if err == nil || !strings.Contains(err.Error(), "does not match") || !strings.Contains(err.Error(), "admin reset") {
		t.Fatalf("%v", err)
	}
}
