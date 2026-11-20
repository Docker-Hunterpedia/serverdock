package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
)

// tlsEnv is a hub behind its own TLS listener (ADR 0009), as runServer builds it.
type tlsEnv struct {
	hub    *Hub
	pki    *pki.Manager
	url    string // wss://127.0.0.1:port/agent/connect
	fp     string
	cancel context.CancelFunc
}

func newTLSEnv(t *testing.T) *tlsEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	mgr, err := pki.Open(pki.Options{Dir: t.TempDir(), SANs: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	e := &tlsEnv{hub: New(ctx, token, quiet), pki: mgr, fp: mgr.CAFingerprint(), cancel: cancel}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+agent.ConnectPath, e.hub)
	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(tls.NewListener(raw, mgr.TLSConfig())) }()
	e.url = "wss://" + raw.Addr().String() + agent.ConnectPath
	t.Cleanup(func() { cancel(); srv.Close(); <-done })
	return e
}

func (e *tlsEnv) agent(t *testing.T, id string, mut func(*agent.ClientConfig)) func() {
	return startAgent(t, e.url, id, newFakeLogs(), func(c *agent.ClientConfig) {
		c.CAFingerprint = e.fp
		if mut != nil {
			mut(c)
		}
	})
}

func TestAgentOverTLSStreamsAndReconnects(t *testing.T) {
	e := newTLSEnv(t)
	var connects atomic.Int32
	e.agent(t, "t1", func(c *agent.ClientConfig) { c.OnConnect = func() { connects.Add(1) } })
	s1 := waitSession(t, e.hub, "t1", 0)
	rc, err := s1.Logs(context.Background(), "web-1", 5)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := rc.Read(buf); err != nil || !strings.HasPrefix(string(buf[:n]), "hello web-1") {
		t.Fatalf("over TLS: %q %v", buf[:n], err)
	}
	rc.Close()

	s1.ys.Close() // force the session down: the agent reconnects over TLS
	s2 := waitSession(t, e.hub, "t1", s1.Seq)
	if connects.Load() < 2 {
		t.Fatalf("connects = %d", connects.Load())
	}
	rc, err = s2.Logs(context.Background(), "after", 5)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if n, err := rc.Read(buf); err != nil || !strings.HasPrefix(string(buf[:n]), "hello after") {
		t.Fatalf("after reconnect: %q %v", buf[:n], err)
	}
}

func TestAgentWithWrongPinNeverConnects(t *testing.T) {
	e := newTLSEnv(t)
	var attempts atomic.Int32
	e.agent(t, "bad", func(c *agent.ClientConfig) {
		c.CAFingerprint = "sha256:" + strings.Repeat("11", 32)
		c.Sleep = func(ctx context.Context, d time.Duration) error { attempts.Add(1); return sleepShort(ctx) }
	})
	deadline := time.Now().Add(5 * time.Second)
	for attempts.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if attempts.Load() < 3 {
		t.Fatal("agent did not retry")
	}
	if _, ok := e.hub.Get("bad"); ok {
		t.Fatal("agent with the wrong pin has a session")
	}
}

func sleepShort(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Millisecond):
		return nil
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// A pin mismatch is reported as one actionable line, not a raw TLS error.
func TestPinMismatchIsExplained(t *testing.T) {
	e := newTLSEnv(t)
	var buf lockedBuf
	e.agent(t, "bad", func(c *agent.ClientConfig) {
		c.CAFingerprint = "sha256:" + strings.Repeat("22", 32)
		c.Log = slog.New(slog.NewTextHandler(&buf, nil))
	})
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "check --ca-fingerprint") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "check --ca-fingerprint") {
		t.Fatalf("no actionable pin message in: %s", buf.String())
	}
}

// A wrong token is reported as a 401 hint.
func TestWrongTokenIsExplained(t *testing.T) {
	e := newTLSEnv(t)
	var buf lockedBuf
	e.agent(t, "bad", func(c *agent.ClientConfig) {
		c.Token = "definitely-not-the-token"
		c.Log = slog.New(slog.NewTextHandler(&buf, nil))
	})
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "check SD_AGENT_TOKEN") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "check SD_AGENT_TOKEN") {
		t.Fatalf("no token hint in: %s", buf.String())
	}
	if strings.Contains(buf.String(), "definitely-not-the-token") {
		t.Error("token leaked into the log")
	}
}

// A second connection under the same id replaces the first and the hub says so,
// naming both peers (never a token).
func TestReplacedSessionIsLogged(t *testing.T) {
	var buf lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := New(ctx, token, slog.New(slog.NewTextHandler(&buf, nil)))
	for _, remote := range []string{"203.0.113.1", "203.0.113.2"} {
		a, b := net.Pipe()
		go func() { _ = h.attach(ctx, "dup", remote, a, 0) }()
		ys, err := yamux.Server(b, agent.YamuxConfig(0))
		if err != nil {
			t.Fatal(err)
		}
		defer ys.Close()
		time.Sleep(100 * time.Millisecond)
	}
	out := buf.String()
	for _, want := range []string{"replaced", "old_remote=203.0.113.1", "new_remote=203.0.113.2"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
}
