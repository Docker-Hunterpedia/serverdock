package main

import (
	"net/http"
	"testing"

	"go.uber.org/goleak"

	"github.com/Docker-Hunterpedia/serverdock/internal/testbin"
)

// closeIdle drops the keep-alive connections tests leave in the shared
// http.DefaultTransport before goleak looks (their loops are not leaks of ours).
type closeIdle struct{ m *testing.M }

func (c closeIdle) Run() int {
	testbin.Init()
	code := c.m.Run()
	testbin.Cleanup()
	http.DefaultClient.CloseIdleConnections()
	return code
}

// TestMain fails the package's tests if any goroutine outlives them.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(closeIdle{m},
		// certmagic's lock-file refresher sleeps one interval (5 s) after the
		// lock is released before it notices and exits; it cannot be stopped
		// from outside the library.
		goleak.IgnoreAnyFunction("github.com/caddyserver/certmagic.keepLockfileFresh"),
	)
}
