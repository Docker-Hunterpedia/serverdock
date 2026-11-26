//go:build integration

package deploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A container that logs 50 MiB (12,800 lines of 4 KiB) is read through the real agent
// and Docker with follow:false and tail:2000: the file reads back at no more than
// 1 MiB, with the cut marked, and its mode and its directory's are private.
func TestDeployLogCapHoldsForAContainerThatLogs50MiB(t *testing.T) {
	g, ref := newSwitchRig(t)
	st := openStore(t)
	svc := addService(t, st, "web")
	d := addDeploy(t, st, svc, 1, "failed", nil)
	name := "sd-it-log-" + g.inst[:8]
	script := `head -c 52428800 /dev/zero | tr '\0' 'a' | fold -w 4095 | sed 's/$/ end/'; echo LAST-LINE-MARKER; sleep 600`
	itDocker(t, "run", "-d", "--name", name, "--label", "serverdock.instance="+g.inst, "--entrypoint", "sh", ref, "-c", script)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })
	// Wait until the output has been written (the marker is the last line).
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(itDocker(t, "logs", "--tail", "1", name), "LAST-LINE-MARKER") {
		if time.Now().After(deadline) {
			t.Fatal("the container did not finish writing")
		}
		time.Sleep(200 * time.Millisecond)
	}
	root := filepath.Join(t.TempDir(), "deploy-logs")
	l := &DeployLogs{Root: root, DB: st, Src: g.sess}
	ctx, cancel := context.WithTimeout(g.ctx, 60*time.Second)
	defer cancel()
	if err := l.Keep(ctx, d, name); err != nil {
		t.Fatal(err)
	}
	text, truncated, err := l.Read(svc, d)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(text) > LogMaxBytes || len(text) < LogMaxBytes-8192 {
		t.Errorf("read back %d bytes (truncated %v), want just under %d", len(text), truncated, LogMaxBytes)
	}
	if !strings.Contains(text, "output cut at") || !strings.Contains(text, "service: p/web") {
		t.Error("the header or the cut marker is missing")
	}
	file := filepath.Join(root, strconv.FormatInt(svc, 10), strconv.FormatInt(d, 10)+".log.gz")
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("%v %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Dir(file)); fi.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v", fi.Mode().Perm())
	}
}
