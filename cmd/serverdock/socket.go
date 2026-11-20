package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// maxSocketPath is the longest unix socket path the OS accepts (sun_path less
// its NUL): 104 bytes on the BSDs and macOS, 108 on Linux.
func maxSocketPath() int {
	switch runtime.GOOS {
	case "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
		return 103
	}
	return 107
}

// listenPrivateSocket creates a unix socket with mode 0600 inside a 0700
// directory owned by the current user. It serves the control plane and the debug
// routes: there is no TCP listener for either, and access is the filesystem
// permission of that directory. A stale socket left by a crash is replaced; any
// other file at the path is an error.
func listenPrivateSocket(path string) (net.Listener, error) {
	return listenPrivate(path, os.Geteuid())
}

func listenPrivate(path string, euid int) (net.Listener, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("socket path %q must be an absolute path", path)
	}
	if n := len(path); n > maxSocketPath() {
		return nil, fmt.Errorf("socket path %q is too long (%d bytes, the limit on this system is %d); use a shorter path", path, n, maxSocketPath())
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("socket dir: %w", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("socket dir: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("socket dir %s is not a directory", dir)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("socket dir %s must not be accessible to group or others (mode %v); chmod 700 it", dir, st.Mode().Perm())
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != euid {
		return nil, fmt.Errorf("socket dir %s is owned by uid %d, not the service user (%d)", dir, sys.Uid, euid)
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
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
