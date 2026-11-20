package hub

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
)

// Caddy keeps global state: this is the only test in the package that uses it.
//
// Observed behaviour asserted here: a Caddy reload (forced, with a new config)
// while an agent is connected through /agent/connect does not drop the agent
// IMMEDIATELY, thanks to stream_close_delay on that route: the same hub session
// survives the reload (same Seq), a log stream opened before it keeps flowing,
// and a new log stream works after it. Caddy still closes the old upgraded
// connection when the delay (5m) expires, after which the agent reconnects with
// backoff; this test only covers the first 2 seconds.
func TestAgentStaysConnectedAcrossCaddyReload(t *testing.T) {
	e := newEnv(t)
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
	stopAgent := startAgent(t, "ws://"+addr+"/agent/connect", "viaCaddy", logs, nil)
	defer stopAgent()
	s1 := waitSession(t, e.hub, "viaCaddy", 0)

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

	// Reload with a different config (a bogus-but-valid second upstream is not
	// needed; Load forces a reload, and the route is rebuilt).
	reloadDone := time.Now()
	if err := proxy.Load(cfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Give a drop the whole first backoff window plus slack to show up.
	time.Sleep(2 * time.Second)

	s2, ok := e.hub.Get("viaCaddy")
	switch {
	case !ok:
		t.Fatal("agent session gone after reload and not reconnected within 2s")
	case s2.Seq == s1.Seq:
		t.Logf("RESULT: agent kept its session through the Caddy reload (%v after reload); Caddy will still close it when stream_close_delay expires", time.Since(reloadDone).Round(time.Millisecond))
	default:
		t.Fatalf("RESULT: agent was dropped by the reload and reconnected (seq %d -> %d); stream_close_delay did not hold the connection", s1.Seq, s2.Seq)
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

	// New agent connections work through the reloaded proxy.
	resp, err := http.Get("http://" + addr + "/healthz-not-found")
	if err == nil {
		resp.Body.Close()
	}

	cancel()
	stopAgent()
	stopProxy()
}
