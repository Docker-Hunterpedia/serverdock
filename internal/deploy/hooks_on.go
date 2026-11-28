//go:build testhooks

package deploy

import (
	"os"
	"path/filepath"
	"strings"
)

// hooksCompiled is true only in a build made with -tags testhooks (internal/testbin
// builds the server that way so a test can SIGKILL it at a stage boundary; plan
// 0004 step 10). It is never in a shipped binary.
const hooksCompiled = true

// pausePoint is called at every stage boundary of a deploy. When the process
// environment names the point in SERVERDOCK_TESTHOOK_PAUSE (a comma separated list
// of "<stage>:<before|after>" names) and gives a directory in
// SERVERDOCK_TESTHOOK_DIR, it writes the file <dir>/paused-<name> (so the test
// knows the process is there) and then blocks forever: the test kills the process.
// pauseRelease lets an in-process test free a paused goroutine; it is nil (blocks
// forever) in a server binary.
var pauseRelease chan struct{}

func pausePoint(name string) {
	list := os.Getenv("SERVERDOCK_TESTHOOK_PAUSE")
	dir := os.Getenv("SERVERDOCK_TESTHOOK_DIR")
	if list == "" || dir == "" {
		return
	}
	for _, n := range strings.Split(list, ",") {
		if n == name {
			_ = os.WriteFile(filepath.Join(dir, "paused-"+strings.ReplaceAll(name, ":", "-")), []byte("1"), 0o600)
			<-pauseRelease // nil in the binary: blocks until the test SIGKILLs the process
		}
	}
}
