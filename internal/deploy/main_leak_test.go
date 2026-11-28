package deploy

import (
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"
)

// TestMain checks for leaked goroutines and, in a -tags testhooks run, that no test
// took the service, allocation and route locks out of order (lockcheck_on.go).
func TestMain(m *testing.M) {
	code := m.Run()
	if v := lockOrderReport(); v != "" {
		fmt.Fprintln(os.Stderr, "lock order violations:\n"+v)
		code = 1
	}
	if code == 0 {
		if err := goleak.Find(); err != nil {
			fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}
