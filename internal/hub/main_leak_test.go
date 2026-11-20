package hub

import (
	"net/http"
	"testing"

	"go.uber.org/goleak"
)

// certmagic's lock-file refresher sleeps one interval (5 s) after the lock is
// released before it notices and exits; it cannot be stopped from outside the
// library.
var ignoreCertmagic = goleak.IgnoreAnyFunction("github.com/caddyserver/certmagic.keepLockfileFresh")

// closeIdle drops the idle keep-alive connections that tests leave in the
// shared http.DefaultTransport (their read/write loops are not leaks of ours).
type closeIdle struct{ m *testing.M }

func (c closeIdle) Run() int {
	code := c.m.Run()
	http.DefaultClient.CloseIdleConnections()
	return code
}

// TestMain fails the package's tests if any goroutine outlives them.
func TestMain(m *testing.M) { goleak.VerifyTestMain(closeIdle{m}, ignoreCertmagic) }
