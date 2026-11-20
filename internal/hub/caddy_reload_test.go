package hub

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
)

// Caddy keeps global state: this is the only test in the package that uses it.
//
// The hub serves /agent/connect on its own TLS listener (ADR 0009), which Caddy
// does not own, so a Caddy reload cannot touch agent sessions at all: the same
// hub session survives (same Seq), a log stream opened before the reload keeps
// flowing, and a new stream works after it. (Before ADR 0009 the route sat
// behind Caddy and a reload dropped agents after stream_close_delay.)
func TestAgentStaysConnectedAcrossCaddyReload(t *testing.T) {
	e := newEnv(t) // plain control plane: the upstream Caddy proxies to
	te := newTLSEnv(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	cfg := proxy.Config{DataDir: t.TempDir(), Listen: addr, Upstream: strings.TrimPrefix(e.srv.URL, "http://")}
	if err := proxy.Load(cfg); err != nil {
		t.Fatal(err)
	}
	var stopped bool
	stopProxy := func() {
		if !stopped {
			stopped = true
			_ = proxy.Stop()
		}
	}
	defer stopProxy()

	logs := newFakeLogs()
	stopAgent := startAgent(t, te.url, "viaTLS", logs, func(c *agent.ClientConfig) { c.CAFingerprint = te.fp })
	defer stopAgent()
	s1 := waitSession(t, te.hub, "viaTLS", 0)

	// A log stream open across the reload.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc, err := s1.Logs(ctx, "long-lived", 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, 64)
	if n, err := rc.Read(buf); err != nil || !strings.HasPrefix(string(buf[:n]), "hello long-lived") {
		t.Fatalf("pre-reload read: %q %v", buf[:n], err)
	}

	// Reload (Load forces one) and give a drop time to show up.
	if err := proxy.Load(cfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	time.Sleep(2 * time.Second)

	s2, ok := te.hub.Get("viaTLS")
	switch {
	case !ok:
		t.Fatal("agent session gone after the Caddy reload")
	case s2.Seq != s1.Seq:
		t.Fatalf("agent was dropped and reconnected by the reload (seq %d -> %d)", s1.Seq, s2.Seq)
	}

	// The long-lived stream still works: opening a second stream proves the
	// session carries traffic, and the first has not been cancelled.
	select {
	case <-logs.cancelled:
		t.Fatal("pre-reload log stream was cancelled by the reload")
	default:
	}
	rc2, err := s2.Logs(ctx, "after-reload", 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rc2.Close()
	if n, err := rc2.Read(buf); err != nil || !strings.HasPrefix(string(buf[:n]), "hello after-reload") {
		t.Fatalf("post-reload read: %q %v", buf[:n], err)
	}

	cancel()
	stopAgent()
	stopProxy()
}
