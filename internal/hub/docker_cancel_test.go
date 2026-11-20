package hub

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
)

// End to end: hub stream.Close() (FIN only) -> agent reader goroutine -> context
// cancel -> the HTTP request to the (fake) Docker socket is cancelled, within 2 s.
func TestServerCloseCancelsDockerRequest(t *testing.T) {
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
	cancelled := make(chan time.Time, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Config":{"Tty":true}}`)
	})
	mux.HandleFunc("GET /containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "docker says hi\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		cancelled <- time.Now()
	})
	ds := &httptest.Server{Listener: ln, Config: &http.Server{Handler: mux}}
	ds.Start()
	defer ds.Close()

	e := newEnv(t)
	startAgent(t, e.wsURL(), "dk", dockerapi.New(sock), nil)
	s := waitSession(t, e.hub, "dk", 0)
	rc, err := s.Logs(context.Background(), "web-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := rc.Read(buf); err != nil || string(buf[:n]) != "docker says hi\n" {
		t.Fatalf("%q %v", buf[:n], err)
	}
	t0 := time.Now()
	rc.Close()
	select {
	case at := <-cancelled:
		if d := at.Sub(t0); d > 2*time.Second {
			t.Fatalf("docker request cancelled after %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("docker request not cancelled within 2s of the server closing the stream")
	}
}
