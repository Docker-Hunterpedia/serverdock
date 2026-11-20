package secretbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newBox(t *testing.T, fill byte) *Box {
	t.Helper()
	b, err := New(bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSecretboxRoundTrip(t *testing.T) {
	b := newBox(t, 1)
	for _, pt := range [][]byte{nil, []byte("x"), bytes.Repeat([]byte("seed"), 100)} {
		ct, err := b.Seal("users/1/totp_seed", pt)
		if err != nil {
			t.Fatal(err)
		}
		if ct[0] != 1 || len(ct) != 1+4+12+len(pt)+16 {
			t.Fatalf("format: version %d len %d", ct[0], len(ct))
		}
		if len(pt) > 3 && bytes.Contains(ct, pt[:4]) {
			t.Fatal("plaintext visible in the ciphertext")
		}
		got, err := b.Open("users/1/totp_seed", ct)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("%q %v", got, err)
		}
	}
	// A fresh nonce each time.
	a, _ := b.Seal("x", []byte("same"))
	c, _ := b.Seal("x", []byte("same"))
	if bytes.Equal(a, c) {
		t.Fatal("two seals of the same value are identical")
	}
	if _, err := New(make([]byte, 31)); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestSecretboxAADBinding(t *testing.T) {
	b := newBox(t, 1)
	ct, _ := b.Seal("users/1/totp_seed", []byte("secret"))
	for _, aad := range []string{"users/2/totp_seed", "users/1/totp_pending_seed", "setup_state/1/pending_seed", ""} {
		if _, err := b.Open(aad, ct); !errors.Is(err, ErrCorrupt) {
			t.Errorf("aad %q: %v", aad, err)
		}
	}
	// A flipped bit anywhere after the key id fails.
	for i := 1 + keyIDLen; i < len(ct); i++ {
		bad := append([]byte(nil), ct...)
		bad[i] ^= 1
		if _, err := b.Open("users/1/totp_seed", bad); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("byte %d: %v", i, err)
		}
	}
	for _, bad := range [][]byte{nil, {1}, ct[:20], append([]byte{2}, ct[1:]...)} {
		if _, err := b.Open("users/1/totp_seed", bad); !errors.Is(err, ErrCorrupt) {
			t.Errorf("malformed %x: %v", bad, err)
		}
	}
}

func TestSecretboxWrongKeyIDRefused(t *testing.T) {
	ct, _ := newBox(t, 1).Seal("a/1/b", []byte("Zq9plain"))
	_, err := newBox(t, 2).Open("a/1/b", ct)
	if !errors.Is(err, ErrWrongKey) {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "Zq9plain") {
		t.Fatal("error carries plaintext")
	}
}

func dataDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	os.Chmod(d, 0o700)
	return d
}

func TestKeyCreatedAndReloaded(t *testing.T) {
	d := dataDir(t)
	none := func() (bool, error) { return false, nil }
	b1, created, err := LoadOrCreate(d, none)
	if err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	st, err := os.Stat(KeyPath(d))
	if err != nil || st.Mode().Perm() != 0o600 || st.Size() != 32 {
		t.Fatalf("key file: %v %v", st, err)
	}
	if ds, _ := os.Stat(filepath.Dir(KeyPath(d))); ds.Mode().Perm() != 0o700 {
		t.Fatalf("keys dir mode %v", ds.Mode().Perm())
	}
	// Atomic write: nothing but master.key in keys/.
	if ents, _ := os.ReadDir(filepath.Dir(KeyPath(d))); len(ents) != 1 {
		t.Fatalf("leftovers: %v", ents)
	}
	ct, _ := b1.Seal("a/1/b", []byte("v"))
	b2, created, err := LoadOrCreate(d, func() (bool, error) { t.Fatal("asked after the key exists"); return false, nil })
	if err != nil || created {
		t.Fatalf("%v %v", created, err)
	}
	if got, err := b2.Open("a/1/b", ct); err != nil || string(got) != "v" {
		t.Fatalf("reloaded key differs: %q %v", got, err)
	}
}

func TestMissingKeyWithCiphertextsRefusesStart(t *testing.T) {
	d := dataDir(t)
	_, _, err := LoadOrCreate(d, func() (bool, error) { return true, nil })
	if err == nil || !strings.Contains(err.Error(), "missing but encrypted data exists") || !strings.Contains(err.Error(), "admin reset") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(KeyPath(d)); err == nil {
		t.Fatal("a key was created anyway")
	}
	// A failing check also refuses (fail closed).
	if _, _, err := LoadOrCreate(d, func() (bool, error) { return false, errors.New("db down") }); err == nil {
		t.Fatal("check error ignored")
	}
	if _, err := os.Stat(KeyPath(d)); err == nil {
		t.Fatal("a key was created after a failed check")
	}
}

func TestSecretboxRefusesBadKeyFile(t *testing.T) {
	none := func() (bool, error) { return false, nil }
	cases := map[string]func(p string){
		"short":      func(p string) { os.WriteFile(p, make([]byte, 31), 0o600) },
		"long":       func(p string) { os.WriteFile(p, make([]byte, 33), 0o600) },
		"empty":      func(p string) { os.WriteFile(p, nil, 0o600) },
		"group":      func(p string) { os.WriteFile(p, make([]byte, 32), 0o640); os.Chmod(p, 0o640) },
		"other":      func(p string) { os.WriteFile(p, make([]byte, 32), 0o604); os.Chmod(p, 0o604) },
		"directory":  func(p string) { os.Mkdir(p, 0o700) },
		"symlink":    func(p string) { os.Symlink("/etc/hostname", p) },
		"unreadable": func(p string) { os.WriteFile(p, make([]byte, 32), 0o000); os.Chmod(p, 0o000) },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "unreadable" && os.Geteuid() == 0 {
				t.Skip("root reads everything")
			}
			d := dataDir(t)
			os.Mkdir(filepath.Join(d, "keys"), 0o700)
			p := KeyPath(d)
			mk(p)
			before, _ := os.Lstat(p)
			_, created, err := LoadOrCreate(d, none)
			if err == nil || created {
				t.Fatalf("accepted a %s key file", name)
			}
			after, lerr := os.Lstat(p)
			if lerr != nil || after.Mode() != before.Mode() || after.Size() != before.Size() {
				t.Fatal("the bad key file was replaced or changed")
			}
		})
	}
}

func TestLooseKeysDirRefused(t *testing.T) {
	d := dataDir(t)
	os.Mkdir(filepath.Join(d, "keys"), 0o755)
	os.Chmod(filepath.Join(d, "keys"), 0o755)
	if _, _, err := LoadOrCreate(d, nil); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("%v", err)
	}
}

func TestCiphertextsExist(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cols := []Column{{"users", "totp_seed"}, {"setup_state", "pending_seed"}}
	// No tables yet: empty.
	if ok, err := CiphertextsExist(ctx, db, cols); err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
	db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, totp_seed BLOB)`)
	db.Exec(`CREATE TABLE setup_state (id INTEGER PRIMARY KEY, pending_seed BLOB)`)
	db.Exec(`INSERT INTO users (id) VALUES (1)`)
	if ok, err := CiphertextsExist(ctx, db, cols); err != nil || ok {
		t.Fatalf("NULL counted: %v %v", ok, err)
	}
	db.Exec(`INSERT INTO setup_state (id, pending_seed) VALUES (1, x'01')`)
	if ok, err := CiphertextsExist(ctx, db, cols); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if _, err := CiphertextsExist(ctx, db, []Column{{"users; DROP", "x"}}); err == nil {
		t.Fatal("bad identifier accepted")
	}
	if len(Columns()) < 3 {
		t.Fatal("the M2 columns are not listed")
	}
}
