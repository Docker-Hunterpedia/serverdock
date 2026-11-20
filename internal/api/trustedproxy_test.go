package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/limits"
)

// keyHandler answers with the limiting key ClientIP computes.
var keyHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, limits.ClientIP(r))
})

func serveOn(t *testing.T, ln net.Listener, opts ...ServeOption) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, keyHandler, time.Second, opts...) }()
	t.Cleanup(func() { cancel(); <-done })
}

func unixListener(t *testing.T) (net.Listener, *http.Client) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sd") // short: socket paths are limited to ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	return ln, &http.Client{Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}}}
}

func do(t *testing.T, c *http.Client, url string, xff ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header["X-Forwarded-For"] = xff
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// Without WithTrustedProxy the header is ignored, including from loopback: a
// local process cannot pick its own limiting key.
func TestXFFIgnoredWithoutSocketMark(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveOn(t, ln)
	for _, xff := range [][]string{nil, {"6.6.6.6"}, {"6.6.6.6", "7.7.7.7"}, {"junk"}} {
		if code, body := do(t, http.DefaultClient, "http://"+ln.Addr().String()+"/", xff...); code != 200 || body != "127.0.0.1" {
			t.Errorf("xff %q: %d %q, want 200 and the peer 127.0.0.1", xff, code, body)
		}
	}
	http.DefaultClient.CloseIdleConnections()
}

// On the marked listener exactly one X-Forwarded-For value holding one IP is
// the client; anything else is a 400 that never reaches the handler.
func TestMultipleXFFValuesRejected(t *testing.T) {
	ln, client := unixListener(t)
	var logs lockedBuf
	serveOn(t, ln, WithTrustedProxy(slog.New(slog.NewTextHandler(&logs, nil))))

	if code, body := do(t, client, "http://cp/", "198.51.100.7"); code != 200 || body != "198.51.100.7" {
		t.Fatalf("one value: %d %q", code, body)
	}
	if code, body := do(t, client, "http://cp/", "2001:db8:1:2:3:4:5:6"); code != 200 || body != "2001:db8:1:2::/64" {
		t.Fatalf("v6: %d %q", code, body)
	}
	for _, xff := range [][]string{
		nil,
		{""},
		{"6.6.6.6", "198.51.100.7"},
		{"6.6.6.6, 198.51.100.7"},
		{"junk"},
		{"198.51.100.7:80"},
	} {
		code, body := do(t, client, "http://cp/", xff...)
		if code != http.StatusBadRequest || !strings.Contains(body, "invalid_forwarded_for") {
			t.Errorf("xff %q: %d %q, want 400", xff, code, body)
		}
	}
	// Logged once a minute, not once per request.
	if n := strings.Count(logs.String(), "X-Forwarded-For"); n != 1 {
		t.Errorf("%d log lines for 6 bad requests, want 1:\n%s", n, logs.String())
	}
}

// The mark belongs to the listener that opted in, not to the process.
func TestProxyMarkIsPerListener(t *testing.T) {
	marked, mc := unixListener(t)
	plain, pc := unixListener(t)
	serveOn(t, marked, WithTrustedProxy(nil))
	serveOn(t, plain)
	if _, body := do(t, mc, "http://cp/", "198.51.100.7"); body != "198.51.100.7" {
		t.Errorf("marked listener: %q", body)
	}
	// The unmarked unix listener has no usable peer address; the header is ignored.
	if code, body := do(t, pc, "http://cp/", "198.51.100.7"); code != 200 || body == "198.51.100.7" {
		t.Errorf("unmarked listener trusted X-Forwarded-For: %d %q", code, body)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }
