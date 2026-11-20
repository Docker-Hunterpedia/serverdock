package dockerapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
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
		w.Write(fd.logsBody)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if fd.holdOpen {
			<-r.Context().Done()
			close(fd.reqCancel)
		}
	})
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: mux}}
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
