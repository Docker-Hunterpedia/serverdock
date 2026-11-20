package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/web"
)

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	req.Host = "127.0.0.1:8443"
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	h := New(Config{SPA: web.FS()}).Handler()
	rec := get(t, h, "GET", "/healthz")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing security headers")
	}
	if rec := get(t, h, "POST", "/healthz"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d", rec.Code)
	}
}

func TestEmbeddedSPA(t *testing.T) {
	h := New(Config{SPA: web.FS()}).Handler()
	for _, p := range []string{"/", "/services/abc"} {
		rec := get(t, h, "GET", p)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>ServerDock</title>") {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Body)
		}
	}
}

func TestSPAFallbackAndAssets(t *testing.T) {
	spa := fstest.MapFS{
		"index.html":    {Data: []byte("INDEX")},
		"assets/app.js": {Data: []byte("JS")},
	}
	h := New(Config{SPA: spa}).Handler()
	if rec := get(t, h, "GET", "/assets/app.js"); rec.Code != 200 || rec.Body.String() != "JS" {
		t.Errorf("asset: %d %q", rec.Code, rec.Body)
	}
	if rec := get(t, h, "GET", "/assets/missing.js"); rec.Code != 404 {
		t.Errorf("missing asset = %d", rec.Code)
	}
	if rec := get(t, h, "GET", "/deep/route"); rec.Code != 200 || rec.Body.String() != "INDEX" {
		t.Errorf("fallback: %d %q", rec.Code, rec.Body)
	}
	// ServeMux cleans the path with a redirect; nothing outside the FS is reachable.
	if rec := get(t, h, "GET", "/../../etc/passwd"); rec.Code != 307 || rec.Header().Get("Location") != "/etc/passwd" {
		t.Errorf("traversal: %d %v", rec.Code, rec.Header())
	}
}

func TestDebugMem(t *testing.T) {
	srv := New(Config{SPA: web.FS()})
	rec := get(t, srv.DebugHandler(), "GET", "/debug/mem")
	var m map[string]any
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &m) != nil || m["goroutines"] == nil {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
	if rec := get(t, srv.DebugHandler(), "GET", "/healthz"); rec.Code != 404 {
		t.Errorf("debug mux leaks other routes: %d", rec.Code)
	}
	// ...and the control plane never serves the debug routes (not the SPA either).
	for _, p := range []string{"/debug/mem", "/debug/agents", "/debug/agents/x/logs/y", "/debug/"} {
		if rec := get(t, srv.Handler(), "GET", p); rec.Code != 404 {
			t.Errorf("GET %s on the control plane = %d, want 404", p, rec.Code)
		}
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, "done")
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, h, 5*time.Second) }()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			body <- "ERR " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	<-started
	cancel() // SIGTERM equivalent while a request is in flight
	if got := <-body; got != "done" {
		t.Fatalf("in-flight request: %q", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	if _, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
		t.Error("listener still accepting")
	}
}

// Agents connect to the hub's own TLS listener; the control plane must answer
// 404 for /agent/*, not the SPA fallback.
func TestAgentPathsAre404(t *testing.T) {
	srv := New(Config{SPA: fstest.MapFS{"index.html": {Data: []byte("<html>spa</html>")}}})
	for _, p := range []string{"/agent/connect", "/agent/", "/agent/x/y", "/api/v1/nothing"} {
		if rec := get(t, srv.Handler(), "GET", p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	rec := get(t, srv.Handler(), "GET", "/some/route")
	if rec.Code != 200 {
		t.Errorf("SPA fallback broken: %d", rec.Code)
	}
}

// Serve refuses oversized headers (431) and bodies (reads fail past 1 MiB), and
// sets no global read/write timeout (listeners carry upgrades and long streams).
func TestServeLimits(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		fmt.Fprint(w, n)
	})
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, mux, time.Second) }()
	defer func() { cancel(); <-done }()
	base := "http://" + ln.Addr().String()
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	post := func(n int) int {
		resp, err := c.Post(base+"/echo", "application/octet-stream", io.LimitReader(zeros{}, int64(n)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post(MaxBodyBytes); got != 200 {
		t.Errorf("body at the cap: %d", got)
	}
	if got := post(MaxBodyBytes + 1); got != http.StatusRequestEntityTooLarge {
		t.Errorf("body over the cap: %d, want 413", got)
	}

	big, _ := http.NewRequest("GET", base+"/ok", nil)
	big.Header.Set("X-Big", strings.Repeat("a", MaxHeaderBytes+8<<10))
	resp, err := c.Do(big)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("oversized header: %d, want 431", resp.StatusCode)
	}
	small, _ := http.NewRequest("GET", base+"/ok", nil)
	small.Header.Set("X-Big", strings.Repeat("a", 8<<10))
	if resp, err := c.Do(small); err != nil || resp.StatusCode != 200 {
		t.Errorf("an 8 KiB header was refused: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
}

// The agent-listener options: no keep-alives, and a short header/handshake
// timeout that drops a silent connection.
func TestServeOptions(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, mux, time.Second, WithReadHeaderTimeout(300*time.Millisecond), WithoutKeepAlives())
	}()
	defer func() { cancel(); <-done }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !resp.Close {
		t.Error("keep-alives are still on (response lacks Connection: close)")
	}
	// A client that connects and sends nothing is dropped near the timeout.
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t0 := time.Now()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("server answered a silent connection")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("silent connection was not dropped by the header timeout")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("dropped after %v, want about 300ms", d)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
