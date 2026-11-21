package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
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
		// Only a dead socket is stale. If something still accepts on it, another
		// server (or process) owns the path: refuse rather than steal it.
		c, derr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if derr == nil {
			c.Close()
			return nil, fmt.Errorf("socket %s is in use by another process", path)
		}
		if !errors.Is(derr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("socket %s exists and could not be checked (%v); remove it if no server is using it", path, derr)
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
	ul := ln.(*net.UnixListener)
	// Unlinking is ours to do, and only if the path is still our own socket.
	ul.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		ul.Close()
		_ = os.Remove(path)
		return nil, err
	}
	pl := &privateListener{UnixListener: ul, path: path}
	if st, err := os.Lstat(path); err == nil {
		pl.id, pl.ok = fileID(st)
	}
	return pl, nil
}

type fileKey struct{ dev, ino uint64 }

func fileID(st os.FileInfo) (fileKey, bool) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}, false
	}
	return fileKey{uint64(sys.Dev), uint64(sys.Ino)}, true //nolint:unconvert // types differ by OS
}

// privateListener removes its socket file on Close only if the path still holds
// the socket it created, so a server that lost the path to another one never
// deletes the other's socket.
type privateListener struct {
	*net.UnixListener
	path string
	id   fileKey
	ok   bool
	once sync.Once
}

func (l *privateListener) Close() error {
	err := l.UnixListener.Close()
	l.once.Do(func() {
		st, serr := os.Lstat(l.path)
		if serr != nil || st.Mode()&os.ModeSocket == 0 || !l.ok {
			return
		}
		if id, ok := fileID(st); ok && id == l.id {
			_ = os.Remove(l.path)
		}
	})
	return err
}
