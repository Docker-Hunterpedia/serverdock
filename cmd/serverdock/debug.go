package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// listenDebugSocket creates the debug unix socket with mode 0600 inside a 0700
// directory owned by the current user. There is no TCP debug listener: access
// is the filesystem permission of that directory. A stale socket left by a
// crash is replaced; any other file at the path is an error.
func listenDebugSocket(path string) (net.Listener, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("--debug-socket %q must be an absolute path", path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("debug socket dir: %w", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("debug socket dir: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("debug socket dir %s is not a directory", dir)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("debug socket dir %s must not be accessible to group or others (mode %v); chmod 700 it", dir, st.Mode().Perm())
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return nil, fmt.Errorf("debug socket dir %s is owned by uid %d, not the service user (%d)", dir, sys.Uid, os.Getuid())
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen debug socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
