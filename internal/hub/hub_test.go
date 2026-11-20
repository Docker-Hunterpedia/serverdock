package hub

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

const token = "test-token-not-secret"

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeLogs writes one line then blocks until its context is cancelled.
type fakeLogs struct {
	started   chan string
	cancelled chan time.Time
}

func newFakeLogs() *fakeLogs {
	return &fakeLogs{started: make(chan string, 8), cancelled: make(chan time.Time, 8)}
}

func (f *fakeLogs) Logs(ctx context.Context, container string, tail int, w io.Writer) error {
	if _, err := io.WriteString(w, "hello "+container+"\n"); err != nil {
		return err
	}
	f.started <- container
	<-ctx.Done()
	f.cancelled <- time.Now()
	return ctx.Err()
}

type env struct {
	hub    *Hub
	cancel context.CancelFunc // hub lifetime
	srv    *httptest.Server
	active atomic.Int32 // handlers currently running
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e := &env{hub: New(ctx, token, quiet), cancel: cancel}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.active.Add(1)
		defer e.active.Add(-1)
		e.hub.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { cancel(); e.srv.Close() })
	return e
}

func (e *env) wsURL() string { return "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/agent/connect" }

func startAgent(t *testing.T, url, id string, logs agent.LogSource, mut func(*agent.ClientConfig)) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := agent.ClientConfig{
		URL: url, Token: token, ID: id, Logs: logs, Log: quiet,
		Backoff: agent.Backoff{Min: 10 * time.Millisecond, Max: 50 * time.Millisecond},
	}
	if mut != nil {
		mut(&cfg)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = agent.RunClient(ctx, cfg) }()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func waitSession(t *testing.T, h *Hub, id string, notSeq uint64) *Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := h.Get(id); ok && s.Seq != notSeq {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent %q did not connect (seq != %d)", id, notSeq)
	return nil
}

func TestAuthAndValidation(t *testing.T) {
	e := newEnv(t)
	base := e.srv.URL + "/agent/connect"
	do := func(auth, query string) int {
		req, _ := http.NewRequest("GET", base+query, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := do("", "?id=a"); c != 401 {
		t.Errorf("no auth: %d", c)
	}
	if c := do("Bearer wrong", "?id=a"); c != 401 {
		t.Errorf("wrong token: %d", c)
	}
	if c := do("Basic "+token, "?id=a"); c != 401 {
		t.Errorf("wrong scheme: %d", c)
	}
	for _, id := range []string{"", "-x", "a/b", "a b", strings.Repeat("a", 65)} {
		if c := do("Bearer "+token, "?id="+id); c != 400 {
			t.Errorf("id %q: %d", id, c)
		}
	}
	// A valid request without an upgrade header is rejected by the WebSocket layer, not 101.
	if c := do("Bearer "+token, "?id=a"); c == 101 {
		t.Errorf("unexpected upgrade")
	}
	disabled := httptest.NewServer(New(context.Background(), "", quiet))
	defer disabled.Close()
	resp, err := http.Get(disabled.URL)
	if err != nil || resp.StatusCode != 503 {
		t.Errorf("disabled hub: %v %v", resp, err)
	}
}

func TestLogsStreamAndServerCloseCancels(t *testing.T) {
	e := newEnv(t)
	logs := newFakeLogs()
	startAgent(t, e.wsURL(), "a1", logs, nil)
	s := waitSession(t, e.hub, "a1", 0)

	rc, err := s.Logs(context.Background(), "web-1", 100)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := rc.Read(buf)
	if err != nil || string(buf[:n]) != "hello web-1\n" {
		t.Fatalf("read %q %v", buf[:n], err)
	}
	// Server-side close only sends FIN; the agent's reader goroutine must cancel.
	t0 := time.Now()
	rc.Close()
	select {
	case at := <-logs.cancelled:
		if d := at.Sub(t0); d > 2*time.Second {
			t.Fatalf("cancel took %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not cancel the log source within 2s of server close")
	}

	// Cancelling the caller's context also closes the stream.
	ctx, cancel := context.WithCancel(context.Background())
	rc2, err := s.Logs(ctx, "web-2", 10)
	if err != nil {
		t.Fatal(err)
	}
	<-logs.started
	<-logs.started
	cancel()
	select {
	case <-logs.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("context cancel did not propagate")
	}
	rc2.Close()
}

func TestInvalidLogRequestsRejectedOnBothSides(t *testing.T) {
	e := newEnv(t)
	startAgent(t, e.wsURL(), "a1", newFakeLogs(), nil)
	s := waitSession(t, e.hub, "a1", 0)
	if _, err := s.Logs(context.Background(), "../etc", 10); err == nil {
		t.Error("server accepted bad container")
	}
	if _, err := s.Logs(context.Background(), "ok", 99999); err == nil {
		t.Error("server accepted bad tail")
	}
	// A hostile server writing a bad op directly is rejected by the agent.
	st, err := s.ys.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	io.WriteString(st, `{"op":"logs","container":"a;rm -rf /","tail":5}`+"\n")
	st.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(st)
	if !strings.Contains(string(b), "invalid container id") {
		t.Errorf("agent reply %q", b)
	}
}

func TestForcedDisconnectReconnects(t *testing.T) {
	e := newEnv(t)
	logs := newFakeLogs()
	var connects atomic.Int32
	startAgent(t, e.wsURL(), "a1", logs, func(c *agent.ClientConfig) { c.OnConnect = func() { connects.Add(1) } })
	s1 := waitSession(t, e.hub, "a1", 0)
	s1.ys.Close() // server forces the session down
	s2 := waitSession(t, e.hub, "a1", s1.Seq)
	if connects.Load() < 2 {
		t.Fatalf("connects = %d", connects.Load())
	}
	rc, err := s2.Logs(context.Background(), "after", 5)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, 64)
	if n, err := rc.Read(buf); err != nil || !strings.HasPrefix(string(buf[:n]), "hello after") {
		t.Fatalf("after reconnect: %q %v", buf[:n], err)
	}
}

func TestAgentReconnectsAfterServerRestart(t *testing.T) {
	e := newEnv(t)
	addr := e.srv.Listener.Addr().String()
	startAgent(t, e.wsURL(), "a1", newFakeLogs(), nil)
	s1 := waitSession(t, e.hub, "a1", 0)
	// Kill the server entirely, then bring a new one up on the same address.
	e.cancel()
	e.srv.CloseClientConnections()
	e.srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h2 := New(ctx, token, quiet)
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Skipf("could not rebind %s: %v", addr, err)
	}
	srv2 := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h2}}
	srv2.Start()
	defer srv2.Close()
	s2 := waitSession(t, h2, "a1", 0)
	if s2 == nil || s1 == nil {
		t.Fatal("no session")
	}
}

func TestShutdownClosesHijackedSessions(t *testing.T) {
	e := newEnv(t)
	var connects atomic.Int32
	startAgent(t, e.wsURL(), "a1", newFakeLogs(), func(c *agent.ClientConfig) {
		c.Backoff = agent.Backoff{Min: time.Hour, Max: time.Hour} // do not reconnect during the test
		c.OnConnect = func() { connects.Add(1) }
	})
	waitSession(t, e.hub, "a1", 0)
	if e.active.Load() != 1 {
		t.Fatalf("active handlers = %d", e.active.Load())
	}
	e.cancel() // server lifetime context: simulates shutdown
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(e.hub.List()) == 0 && e.active.Load() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("shutdown left sessions=%d active handlers=%d", len(e.hub.List()), e.active.Load())
}

func TestSameIDReplacesOldSession(t *testing.T) {
	e := newEnv(t)
	startAgent(t, e.wsURL(), "dup", newFakeLogs(), nil)
	s1 := waitSession(t, e.hub, "dup", 0)
	startAgent(t, e.wsURL(), "dup", newFakeLogs(), nil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cur, ok := e.hub.Get("dup"); ok && cur.Seq != s1.Seq {
			if l := e.hub.List(); len(l) != 1 {
				t.Fatalf("list = %+v", l)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("second connection did not replace the first")
}

// A session that lasts >= ResetAfter resets the backoff; short ones do not.
func TestBackoffResetOnlyAfterStableConnection(t *testing.T) {
	run := func(stable bool) []time.Duration {
		e := newEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var clock atomic.Int64
		var sleeps []time.Duration
		cfg := agent.ClientConfig{
			URL: e.wsURL(), Token: token, ID: "b", Logs: newFakeLogs(), Log: quiet,
			Backoff:    agent.Backoff{Min: time.Second, Max: 30 * time.Second, Rand: func() float64 { return 0.999999 }},
			ResetAfter: 60 * time.Second,
			Now: func() time.Time {
				if stable {
					clock.Add(int64(61 * time.Second)) // each connection "lasts" 61 s
				}
				return time.Unix(0, clock.Load())
			},
			Sleep: func(_ context.Context, d time.Duration) error {
				sleeps = append(sleeps, d)
				if len(sleeps) >= 4 {
					cancel()
					return context.Canceled
				}
				// kick the next connection quickly: drop the current session
				if s, ok := e.hub.Get("b"); ok {
					s.ys.Close()
				}
				return nil
			},
		}
		done := make(chan struct{})
		go func() { defer close(done); _ = agent.RunClient(ctx, cfg) }()
		// Drop each new session as soon as it appears.
		go func() {
			for ctx.Err() == nil {
				if s, ok := e.hub.Get("b"); ok {
					s.ys.Close()
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("RunClient did not stop")
		}
		return sleeps
	}
	rounded := func(ds []time.Duration) []time.Duration {
		out := make([]time.Duration, len(ds))
		for i, d := range ds {
			out[i] = d.Round(time.Second)
		}
		return out
	}
	grow := rounded(run(false))
	if len(grow) < 4 || grow[0] != time.Second || grow[1] != 2*time.Second || grow[2] != 4*time.Second || grow[3] != 8*time.Second {
		t.Errorf("unstable connections should back off exponentially: %v", grow)
	}
	flat := rounded(run(true))
	for _, d := range flat {
		if d != time.Second {
			t.Errorf("stable connections should reset the backoff: %v", flat)
			break
		}
	}
}

func TestLocalPipeAttach(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := New(ctx, "", quiet)
	a, b := net.Pipe()
	logs := newFakeLogs()
	go func() { _ = h.Attach(ctx, "local", a, 0) }()
	go func() { _ = agent.ServeSession(ctx, b, logs, 0, quiet) }()
	s := waitSession(t, h, "local", 0)
	rc, err := s.Logs(ctx, "c1", 5)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, 64)
	if n, err := rc.Read(buf); err != nil || string(buf[:n]) != "hello c1\n" {
		t.Fatalf("%q %v", buf[:n], err)
	}
}

func TestDebugRoutes(t *testing.T) {
	e := newEnv(t)
	startAgent(t, e.wsURL(), "dbg", newFakeLogs(), nil)
	waitSession(t, e.hub, "dbg", 0)
	mux := http.NewServeMux()
	e.hub.DebugRoutes(mux)
	ds := httptest.NewServer(mux)
	defer ds.Close()
	resp, err := http.Get(ds.URL + "/debug/agents")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"dbg"`) {
		t.Fatalf("agents: %s", b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ds.URL+"/debug/agents/dbg/logs/web-1?tail=5", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "hello web-1\n" {
		t.Fatalf("log proxy: %q", buf[:n])
	}
	resp2, _ := http.Get(ds.URL + "/debug/agents/nope/logs/x")
	if resp2.StatusCode != 404 {
		t.Errorf("unknown agent: %d", resp2.StatusCode)
	}
	resp2.Body.Close()
	resp3, _ := http.Get(ds.URL + "/debug/agents/dbg/logs/..%2Fx")
	if resp3.StatusCode == 200 {
		t.Errorf("bad container accepted")
	}
	resp3.Body.Close()
}
