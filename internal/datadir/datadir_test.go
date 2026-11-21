package datadir

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDataDirCreated0700(t *testing.T) {
	// Even with a loose umask the new directory is exactly 0700.
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	dir := filepath.Join(t.TempDir(), "a", "data")
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", st, err)
	}
	// An existing 0700 directory owned by us is accepted and left alone.
	if err := Prepare(dir); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
}

func TestLooserDataDirRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o750, 0o705, 0o755, 0o770, 0o777, 0o701} {
		dir := filepath.Join(t.TempDir(), "d")
		os.Mkdir(dir, 0o700)
		os.Chmod(dir, mode)
		err := Prepare(dir)
		if err == nil || !strings.Contains(err.Error(), "chmod 700 "+dir) {
			t.Errorf("mode %o: %v", mode, err)
		}
		// Never tightened silently.
		if st, _ := os.Stat(dir); st.Mode().Perm() != mode {
			t.Errorf("mode %o was changed to %o", mode, st.Mode().Perm())
		}
	}
}

func TestForeignOwnedDataDirRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	os.Mkdir(dir, 0o700)
	// Pretend to be a different user: portable, no root needed.
	err := prepare(dir, os.Geteuid()+1)
	if err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("foreign owner accepted: %v", err)
	}
	if err := prepare(dir, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareRefusesAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	os.WriteFile(f, nil, 0o600)
	if err := Prepare(f); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("%v", err)
	}
	if err := Prepare(""); err == nil {
		t.Fatal("empty path accepted")
	}
}

func TestWriteAtomic(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	for _, content := range []string{"one", "two"} {
		if err := WriteAtomic(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		st, _ := os.Stat(p)
		if string(b) != content || st.Mode().Perm() != 0o600 {
			t.Fatalf("%q %v", b, st.Mode())
		}
	}
	// No temp file is left behind.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("leftovers: %v", ents)
	}
}

func TestSecretPaths(t *testing.T) {
	got := strings.Join(SecretPaths(), " ")
	for _, want := range []string{"keys/", "pki/", "setup-token", "agent.token", "backups/"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
}
