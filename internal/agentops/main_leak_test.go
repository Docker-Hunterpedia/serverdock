package agentops

import (
	"net/http"
	"testing"

	"go.uber.org/goleak"
)

// closeIdle drops the idle keep-alive connections tests leave in the shared
// http.DefaultTransport (their loops are not leaks of ours).
type closeIdle struct{ m *testing.M }

func (c closeIdle) Run() int {
	code := c.m.Run()
	http.DefaultClient.CloseIdleConnections()
	return code
}

// TestMain fails the package's tests if any goroutine outlives them.
func TestMain(m *testing.M) { goleak.VerifyTestMain(closeIdle{m}) }
