package api

import (
	"context"
	"encoding/json"
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
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	h := New(web.FS()).Handler()
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
	h := New(web.FS()).Handler()
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
	h := New(spa).Handler()
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

func TestExtraRoute(t *testing.T) {
	s := New(web.FS())
	s.Handle("GET /agent/connect", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "hub")
	}))
	if rec := get(t, s.Handler(), "GET", "/agent/connect"); rec.Body.String() != "hub" {
		t.Fatalf("got %q", rec.Body)
	}
}

func TestDebugMem(t *testing.T) {
	rec := get(t, DebugHandler(), "GET", "/debug/mem")
	var m map[string]any
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &m) != nil || m["goroutines"] == nil {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
	if rec := get(t, DebugHandler(), "GET", "/healthz"); rec.Code != 404 {
		t.Errorf("debug mux leaks other routes: %d", rec.Code)
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
	srv := New(fstest.MapFS{"index.html": {Data: []byte("<html>spa</html>")}})
	for _, p := range []string{"/agent/connect", "/agent/", "/agent/x/y"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/some/route", nil))
	if rec.Code != 200 {
		t.Errorf("SPA fallback broken: %d", rec.Code)
	}
}
