package main

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package's tests if any goroutine outlives them.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// certmagic's lock-file refresher sleeps one interval (5 s) after the
		// lock is released before it notices and exits; it cannot be stopped
		// from outside the library.
		goleak.IgnoreAnyFunction("github.com/caddyserver/certmagic.keepLockfileFresh"),
	)
}
