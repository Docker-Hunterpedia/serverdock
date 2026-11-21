// Package datadir prepares and guards the ServerDock data directory (DATA).
// DATA holds the database, the keys and the private keys of both certificates,
// so it must be owned by the running user and closed to everyone else. This
// package never loosens or silently tightens an existing directory: a wrong
// one stops the server with a message that says how to fix it.
//
// Nothing here runs at package load.
package datadir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Prepare creates dir with mode 0700 if it is missing. An existing dir must be
// a directory owned by the running (effective) uid with no group or other
// permission bits; anything else is an error.
func Prepare(dir string) error { return prepare(dir, os.Geteuid()) }

func prepare(dir string, euid int) error {
	if dir == "" {
		return errors.New("data directory is empty")
	}
	st, err := os.Stat(dir)
	switch {
	case err == nil:
		return check(dir, st, euid)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("data directory %s: %w", dir, err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("data directory %s: %w", dir, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("data directory %s: %w", dir, err)
	}
	// The mode asked for in Mkdir is filtered by the umask; set it exactly.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("data directory %s: %w", dir, err)
	}
	st, err = os.Stat(dir)
	if err != nil {
		return fmt.Errorf("data directory %s: %w", dir, err)
	}
	return check(dir, st, euid)
}

func check(dir string, st fs.FileInfo, euid int) error {
	if !st.IsDir() {
		return fmt.Errorf("data directory %s is not a directory", dir)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != euid {
		return fmt.Errorf("data directory %s is owned by uid %d, not the user ServerDock runs as (uid %d); run ServerDock as the owner, or: chown -R <service user> %s", dir, sys.Uid, euid, dir)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("data directory %s must not be accessible to group or others (mode %04o); fix it with: chmod 700 %s", dir, st.Mode().Perm(), dir)
	}
	return nil
}

// SecretPaths lists, relative to DATA, everything that holds a secret. Backup
// bundles and the support bundle must treat exactly this list as secret
// (docs/plans/0003, G6). A directory entry ends in "/".
func SecretPaths() []string {
	return []string{"keys/", "pki/", "setup-token", "agent.token", "backups/"}
}

// WriteAtomic writes path through a temp file in the same directory, fsync,
// rename and a directory fsync, so a crash leaves either the old file or the
// new one, never half of it. The file has exactly mode.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
