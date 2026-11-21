package main

import "syscall"

// restrictUmask sets the process umask to 0077, so every file ServerDock (and
// SQLite's -wal/-shm, and Caddy's storage) creates is private to the service
// user. It is process-wide, so it is called once at the top of each command that
// writes under DATA, before anything is created. File-mode tests therefore run
// the built binary (internal/testbin) instead of runServer in-process.
func restrictUmask() { syscall.Umask(0o077) }
