package dockerapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func frame(typ byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = typ
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestDemux(t *testing.T) {
	var src bytes.Buffer
	src.Write(frame(1, "out1\n"))
	src.Write(frame(2, "err1\n"))
	src.Write(frame(1, ""))
	src.Write(frame(1, "out2\n"))
	var dst bytes.Buffer
	if err := Demux(&dst, &src); err != nil || dst.String() != "out1\nerr1\nout2\n" {
		t.Fatalf("%q %v", dst.String(), err)
	}
}

func TestDemuxErrors(t *testing.T) {
	trunc := frame(1, "hello")
	cases := map[string][]byte{
		"truncated header":  trunc[:4],
		"truncated payload": trunc[:10],
		"bad type":          frame(7, "x"),
	}
	big := make([]byte, 8)
	big[0] = 1
	binary.BigEndian.PutUint32(big[4:], maxFrame+1)
	cases["oversize"] = big
	for name, in := range cases {
		if err := Demux(io.Discard, bytes.NewReader(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if err := Demux(io.Discard, bytes.NewReader(nil)); err != nil {
		t.Errorf("empty stream: %v", err)
	}
}

// fakeDocker serves a minimal Engine API on a unix socket.
type fakeDocker struct {
	tty       bool
	logsBody  []byte
	holdOpen  bool
	reqCancel chan struct{} // closed when a logs request's context is cancelled
	logsPath  chan string

	api          string        // API-Version reported by _ping (default "1.45")
	pingFail     atomic.Int32  // number of pings to fail with 500 first
	pings        atomic.Int32  // pings served
	prefixes     chan string   // the /v<ver> prefix of each non-ping request
	inspectDelay time.Duration // delay before answering inspect
	logsDelay    time.Duration // delay before answering logs (before headers)
	badRequests  atomic.Int32  // number of versioned requests to answer 400 first
	stopDelay    time.Duration // delay before answering stop
	stopQuery    chan string   // raw query of stop requests
}

// versioned serves /_ping and strips the /v<ver> prefix before next.
func (fd *fakeDocker) versioned(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			fd.pings.Add(1)
			if fd.pingFail.Add(-1) >= 0 {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Header().Set("API-Version", fd.api)
			io.WriteString(w, "OK")
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/v")
		ver, path, _ := strings.Cut(rest, "/")
		if !ok {
			http.Error(w, "unversioned", http.StatusBadRequest)
			return
		}
		select {
		case fd.prefixes <- "v" + ver:
		default:
		}
		if fd.badRequests.Add(-1) >= 0 {
			http.Error(w, "unsupported API version", http.StatusBadRequest)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + path
		next.ServeHTTP(w, r2)
	})
}

func startFakeDocker(t *testing.T, fd *fakeDocker) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fd.reqCancel = make(chan struct{})
	fd.logsPath = make(chan string, 4)
	fd.prefixes = make(chan string, 16)
	fd.stopQuery = make(chan string, 4)
	if fd.api == "" {
		fd.api = "1.45"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Version":"25.0.3","ApiVersion":"1.44"}`)
	})
	mux.HandleFunc("POST /containers/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		fd.stopQuery <- r.URL.RawQuery
		time.Sleep(fd.stopDelay)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(fd.inspectDelay)
		if r.PathValue("id") == "missing" {
			http.NotFound(w, r)
			return
		}
		if fd.tty {
			io.WriteString(w, `{"Config":{"Tty":true}}`)
		} else {
			io.WriteString(w, `{"Config":{"Tty":false}}`)
		}
	})
	mux.HandleFunc("GET /containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		fd.logsPath <- r.URL.RequestURI()
		time.Sleep(fd.logsDelay)
		w.Write(fd.logsBody)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if fd.holdOpen {
			<-r.Context().Done()
			close(fd.reqCancel)
		}
	})
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: fd.versioned(mux)}}
	srv.Start()
	t.Cleanup(srv.Close)
	return sock
}

func TestLogsNonTTYDemuxes(t *testing.T) {
	fd := &fakeDocker{logsBody: append(frame(1, "a\n"), frame(2, "b\n")...)}
	c := New(startFakeDocker(t, fd))
	var out bytes.Buffer
	if err := c.Logs(context.Background(), "web-1", 50, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "a\nb\n" {
		t.Fatalf("%q", out.String())
	}
	p := <-fd.logsPath
	for _, want := range []string{"/containers/web-1/logs?", "follow=1", "stdout=1", "stderr=1", "tail=50"} {
		if !strings.Contains(p, want) {
			t.Errorf("request %q missing %q", p, want)
		}
	}
}

// TestLogsFollowFalseEndsAtTail: follow=0 goes to Docker and the call returns
// when Docker ends the stream after the tail.
func TestLogsFollowFalseEndsAtTail(t *testing.T) {
	fd := &fakeDocker{logsBody: frame(1, "tail line\n")}
	c := New(startFakeDocker(t, fd))
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- c.LogsFollow(context.Background(), "web-1", 5, false, &out) }()
	select {
	case err := <-done:
		if err != nil || out.String() != "tail line\n" {
			t.Fatalf("%q %v", out.String(), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LogsFollow(false) did not end at the tail")
	}
	if p := <-fd.logsPath; !strings.Contains(p, "follow=0") {
		t.Errorf("request %q: want follow=0", p)
	}
}

func TestLogsTTYCopiesRaw(t *testing.T) {
	fd := &fakeDocker{tty: true, logsBody: []byte("raw tty output\r\n")}
	c := New(startFakeDocker(t, fd))
	var out bytes.Buffer
	if err := c.Logs(context.Background(), "tty-1", 5, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "raw tty output\r\n" {
		t.Fatalf("%q", out.String())
	}
}

func TestLogsValidationAndErrors(t *testing.T) {
	fd := &fakeDocker{}
	c := New(startFakeDocker(t, fd))
	for _, id := range []string{"", "../x", "a/b", "-x", "a b", "a?x=1"} {
		if err := c.Logs(context.Background(), id, 5, io.Discard); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	if err := c.Logs(context.Background(), "ok", 0, io.Discard); err == nil {
		t.Error("tail 0 accepted")
	}
	if err := c.Logs(context.Background(), "missing", 5, io.Discard); err == nil {
		t.Error("404 not reported")
	}
	select {
	case p := <-fd.logsPath:
		t.Errorf("unexpected logs request %q", p)
	default:
	}
	if err := New("/nonexistent/docker.sock").Logs(context.Background(), "x", 5, io.Discard); err == nil {
		t.Error("dial error not reported")
	}
}

func TestLogsContextCancelCancelsRequest(t *testing.T) {
	fd := &fakeDocker{logsBody: frame(1, "first\n"), holdOpen: true}
	c := New(startFakeDocker(t, fd))
	t.Cleanup(c.Close)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		pr, pw := io.Pipe()
		go func() {
			b := make([]byte, 16)
			n, _ := pr.Read(b)
			got <- string(b[:n])
			io.Copy(io.Discard, pr)
		}()
		err := c.Logs(ctx, "slow", 5, pw)
		pw.Close() // ends the reader goroutine
		done <- err
	}()
	if g := <-got; g != "first\n" {
		t.Fatalf("got %q", g)
	}
	t0 := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Logs did not return after cancel")
	}
	select {
	case <-fd.reqCancel:
		if d := time.Since(t0); d > 2*time.Second {
			t.Errorf("docker request cancelled after %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("docker server never saw the request cancelled")
	}
}

func TestNegotiationUsesMinOfServerAndMax(t *testing.T) {
	for _, tc := range []struct{ server, want string }{
		{"1.44", "v1.44"},
		{"1.47", "v1.47"},
		{"1.99", "v" + MaxAPIVersion},
	} {
		fd := &fakeDocker{api: tc.server}
		c := New(startFakeDocker(t, fd))
		t.Cleanup(c.Close)
		if _, _, err := c.Info(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := <-fd.prefixes; got != tc.want {
			t.Errorf("server %s: prefix %s, want %s", tc.server, got, tc.want)
		}
		if c.NegotiatedAPI() != strings.TrimPrefix(tc.want, "v") {
			t.Errorf("NegotiatedAPI = %q", c.NegotiatedAPI())
		}
		// Cached: more calls do not ping again.
		c.Info(context.Background())
		if n := fd.pings.Load(); n != 1 {
			t.Errorf("pings = %d, want 1", n)
		}
	}
}

func TestOldDaemonRefused(t *testing.T) {
	for _, v := range []string{"1.43", "1.41", "0.9"} {
		fd := &fakeDocker{api: v}
		c := New(startFakeDocker(t, fd))
		t.Cleanup(c.Close)
		err := c.Logs(context.Background(), "web-1", 5, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "Docker Engine 25+") {
			t.Errorf("API %s: err = %v", v, err)
		}
		select {
		case p := <-fd.prefixes:
			t.Errorf("API %s: a versioned request was sent (%s)", v, p)
		default:
		}
	}
	// A garbage version header is refused too.
	fd := &fakeDocker{api: "banana"}
	c := New(startFakeDocker(t, fd))
	t.Cleanup(c.Close)
	if _, _, err := c.Info(context.Background()); err == nil {
		t.Error("bad API-Version accepted")
	}
}

func TestPingRetriedAfterFailure(t *testing.T) {
	fd := &fakeDocker{}
	fd.pingFail.Store(1)
	c := New(startFakeDocker(t, fd))
	t.Cleanup(c.Close)
	if _, _, err := c.Info(context.Background()); err == nil {
		t.Fatal("expected the first (failed) ping to error")
	}
	if c.NegotiatedAPI() != "" {
		t.Fatal("failure was cached")
	}
	eng, api, err := c.Info(context.Background())
	if err != nil || eng != "25.0.3" || api != "1.45" {
		t.Fatalf("retry: %q %q %v", eng, api, err)
	}
	if n := fd.pings.Load(); n != 2 {
		t.Errorf("pings = %d, want 2", n)
	}
}

func TestInspectThatHangsFailsAtItsDeadline(t *testing.T) {
	fd := &fakeDocker{inspectDelay: time.Second}
	c := New(startFakeDocker(t, fd))
	t.Cleanup(c.Close)
	c.short = 150 * time.Millisecond
	t0 := time.Now()
	err := c.Logs(context.Background(), "web-1", 5, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Errorf("took %v", d)
	}
}

func TestStopIsNotCutByTheShortDeadline(t *testing.T) {
	fd := &fakeDocker{stopDelay: 500 * time.Millisecond}
	c := New(startFakeDocker(t, fd))
	t.Cleanup(c.Close)
	c.short = 100 * time.Millisecond // a short call would die at 100 ms
	if err := c.Stop(context.Background(), "web-1", 2); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if q := <-fd.stopQuery; q != "t=2" {
		t.Errorf("query %q", q)
	}
	if err := c.Stop(context.Background(), "../x", 2); err == nil {
		t.Error("bad id accepted")
	}
	if err := c.Stop(context.Background(), "web-1", -1); err == nil {
		t.Error("negative grace accepted")
	}
}

func TestLogFollowOutlivesTheShortDeadline(t *testing.T) {
	fd := &fakeDocker{logsBody: frame(1, "first\n"), holdOpen: true}
	c := New(startFakeDocker(t, fd))
	t.Cleanup(c.Close)
	c.short = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out syncBuf
	go func() { done <- c.Logs(ctx, "slow", 5, &out) }()
	time.Sleep(500 * time.Millisecond) // five short deadlines
	select {
	case err := <-done:
		t.Fatalf("stream ended early: %v", err)
	default:
	}
	if out.String() != "first\n" {
		t.Errorf("out = %q", out.String())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Every exported Client method that talks to the daemon must go through call
// or stream (no direct c.hc use), so no request escapes a deadline policy.
func TestExportedMethodsUseTheHelpers(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "dockerapi.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	helpers := map[string]bool{"call": true, "stream": true, "negotiate": true}
	var checked int
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		usesHelper, usesHC := false, false
		name0 := fn.Name.Name
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "c" {
					// Logs delegates to LogsFollow, which is itself checked here.
					if helpers[sel.Sel.Name] || (name0 == "Logs" && sel.Sel.Name == "LogsFollow") {
						usesHelper = true
					}
					if sel.Sel.Name == "hc" {
						usesHC = true
					}
				}
			}
			return true
		})
		name := fn.Name.Name
		switch {
		case helpers[name]:
		case name == "Close" || name == "NegotiatedAPI":
			// do not issue requests
			if usesHC && name != "Close" {
				t.Errorf("%s uses c.hc", name)
			}
		case fn.Name.IsExported():
			checked++
			if !usesHelper || usesHC {
				t.Errorf("exported method %s must use c.call/c.stream and not c.hc", name)
			}
		}
	}
	if checked < 3 { // Info, Stop, Logs
		t.Errorf("only %d exported methods checked", checked)
	}
	// A transport-wide ResponseHeaderTimeout would cut stop?t=N.
	src, _ := os.ReadFile("dockerapi.go")
	for _, l := range strings.Split(string(src), "\n") {
		if strings.Contains(l, "ResponseHeaderTimeout") && !strings.HasPrefix(strings.TrimSpace(l), "//") {
			t.Errorf("code sets ResponseHeaderTimeout: %s", l)
		}
	}
}

// A daemon that accepts the logs request and never sends headers fails at the
// short deadline instead of holding the caller for the whole (unbounded) stream.
func TestStreamHeadersMustArriveInTime(t *testing.T) {
	fd := &fakeDocker{logsBody: frame(1, "x\n"), logsDelay: time.Second}
	c := New(startFakeDocker(t, fd))
	t.Cleanup(c.Close)
	c.short = 150 * time.Millisecond
	t0 := time.Now()
	err := c.Logs(context.Background(), "web-1", 5, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if d := time.Since(t0); d > 900*time.Millisecond {
		t.Errorf("took %v", d)
	}
}

// A 400, or a connection-level error, drops the cached API version so the next
// call pings again.
func TestNegotiationForgottenOn400AndConnectionError(t *testing.T) {
	fd := &fakeDocker{}
	c := New(startFakeDocker(t, fd))
	t.Cleanup(c.Close)
	if _, _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	fd.badRequests.Store(1)
	if _, _, err := c.Info(context.Background()); err == nil {
		t.Fatal("expected the 400 to surface")
	}
	if c.NegotiatedAPI() != "" {
		t.Error("API version still cached after a 400")
	}
	if _, _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := fd.pings.Load(); n != 2 {
		t.Errorf("pings = %d, want 2 (re-negotiated after the 400)", n)
	}
	// Connection-level error: point the client at a dead socket.
	dead := New("/nonexistent/docker.sock")
	dead.mu.Lock()
	dead.prefix, dead.apiVer = "/v1.45", "1.45"
	dead.mu.Unlock()
	if _, _, err := dead.Info(context.Background()); err == nil {
		t.Fatal("expected a dial error")
	}
	if dead.NegotiatedAPI() != "" {
		t.Error("API version still cached after a connection error")
	}
	// Our own cancellation is not evidence about the daemon.
	c2 := New(startFakeDocker(t, &fakeDocker{inspectDelay: time.Second}))
	t.Cleanup(c2.Close)
	c2.short = 100 * time.Millisecond
	c2.Logs(context.Background(), "web-1", 5, io.Discard)
	if c2.NegotiatedAPI() == "" {
		t.Error("a local deadline cleared the negotiated version")
	}
}

// Waiters on a slow first ping obey their own context; nothing holds the lock.
func TestNegotiateWaitersRespectTheirContext(t *testing.T) {
	block := make(chan struct{})
	dir, _ := os.MkdirTemp("", "sd")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})}}
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })
	c := New(sock)
	t.Cleanup(c.Close)
	go c.Info(context.Background()) // first caller: stuck in the ping
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if _, _, err := c.Info(ctx); err == nil {
		t.Fatal("expected a deadline error")
	}
	if d := time.Since(t0); d > time.Second {
		t.Errorf("second caller waited %v behind the first", d)
	}
}
