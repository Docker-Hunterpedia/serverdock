//go:build integration

package dockerapi

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Needs a running Docker and the busybox image: go test -tags integration ./internal/agent/dockerapi
func TestRealDockerLogsBusybox(t *testing.T) {
	sock := os.Getenv("DOCKER_SOCK")
	if sock == "" {
		sock = DefaultSocket
	}
	name := "sd-itest-busybox"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", name, "busybox:1",
		"sh", "-c", "i=0; while true; do echo line-$i; echo err-$i >&2; i=$((i+1)); sleep 0.2; done").CombinedOutput()
	if err != nil {
		t.Skipf("docker run failed: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pr, pw := ioPipe()
	done := make(chan error, 1)
	c := New(sock)
	t.Cleanup(c.Close)
	go func() { done <- c.Logs(ctx, name, 10, pw) }()
	sc := bufio.NewScanner(pr)
	var sawOut, sawErr bool
	for sc.Scan() && !(sawOut && sawErr) {
		l := sc.Text()
		sawOut = sawOut || strings.HasPrefix(l, "line-")
		sawErr = sawErr || strings.HasPrefix(l, "err-")
	}
	if !sawOut || !sawErr {
		t.Fatalf("stdout seen=%v stderr seen=%v", sawOut, sawErr)
	}
	go io.Copy(io.Discard, pr) // keep the pipe drained so Logs is not blocked in Write
	t0 := time.Now()
	cancel()
	select {
	case <-done:
		pw.Close() // ends the drain goroutine
		if time.Since(t0) > 2*time.Second {
			t.Error("cancel took too long")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Logs did not stop after cancel")
	}
}

func ioPipe() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

// stop?t=N against a real daemon, through the negotiated client.
func TestRealDockerStopBusybox(t *testing.T) {
	sock := os.Getenv("DOCKER_SOCK")
	if sock == "" {
		sock = DefaultSocket
	}
	name := "sd-itest-stop"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "busybox:1", "sleep", "300").CombinedOutput(); err != nil {
		t.Skipf("docker run failed: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	c := New(sock)
	t.Cleanup(c.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.Stop(ctx, name, 2); err != nil {
		t.Fatal(err)
	}
	if c.NegotiatedAPI() == "" {
		t.Error("no API negotiated")
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	if err != nil || strings.TrimSpace(string(out)) != "false" {
		t.Errorf("container still running: %q %v", out, err)
	}
}
