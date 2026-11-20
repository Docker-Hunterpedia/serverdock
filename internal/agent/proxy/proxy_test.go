package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/coder/websocket"
)

// Caddy keeps global state, so tests here must not run in parallel.

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func hostPort(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }

func body(t *testing.T, url string) string {
	t.Helper()
	var last error
	for i := 0; i < 50; i++ {
		resp, err := http.Get(url)
		if err == nil {
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return string(b)
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("GET %s: %v", url, last)
	return ""
}

func TestBuildConfig(t *testing.T) {
	b, err := BuildConfig(Config{DataDir: "/data", Listen: ":80", Upstream: "127.0.0.1:7070"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["admin"].(map[string]any)["disabled"] != true {
		t.Error("admin not disabled")
	}
	st := cfg["storage"].(map[string]any)
	if st["module"] != "file_system" || st["root"] != "/data/caddy" {
		t.Errorf("storage: %v", st)
	}
	cfgAdmin := cfg["admin"].(map[string]any)
	if cfgAdmin["config"].(map[string]any)["persist"] != false {
		t.Error("admin.config.persist must be false")
	}
	s := string(b)
	for _, want := range []string{`"grace_period":"10s"`, `"automatic_https":{"disable":true}`, `"dial":"127.0.0.1:7070"`} {
		if !strings.Contains(s, want) {
			t.Errorf("config missing %s", want)
		}
	}
	// The agent route and its stream_close_delay are gone: the hub serves
	// /agent/connect on its own TLS listener (ADR 0009).
	for _, gone := range []string{"/agent/connect", "stream_close_delay", "flush_interval"} {
		if strings.Contains(s, gone) {
			t.Errorf("config still contains %q", gone)
		}
	}
}

func TestLoadDoesNotAutosave(t *testing.T) {
	before, beforeErr := os.Stat(caddy.ConfigAutosavePath)
	addr := freeAddr(t)
	up := httptest.NewServer(http.NotFoundHandler())
	defer up.Close()
	if err := Load(Config{DataDir: t.TempDir(), Listen: addr, Upstream: hostPort(up)}); err != nil {
		t.Fatal(err)
	}
	defer Stop()
	after, afterErr := os.Stat(caddy.ConfigAutosavePath)
	if (beforeErr == nil) != (afterErr == nil) || (beforeErr == nil && !before.ModTime().Equal(after.ModTime())) {
		t.Fatalf("autosave file touched at %s (before %v, after %v)", caddy.ConfigAutosavePath, beforeErr, afterErr)
	}
}

func TestBuildConfigValidation(t *testing.T) {
	bad := []Config{
		{Listen: ":80", Upstream: "127.0.0.1:1"},
		{DataDir: "/d", Listen: "80", Upstream: "127.0.0.1:1"},
		{DataDir: "/d", Listen: ":0", Upstream: "127.0.0.1:1"},
		{DataDir: "/d", Listen: ":80", Upstream: "nohost"},
	}
	for _, c := range bad {
		if _, err := BuildConfig(c); err == nil {
			t.Errorf("expected error for %+v", c)
		}
	}
}

func TestProxyServesSPAWebSocketAndReload(t *testing.T) {
	// Upstream 1: SPA + WebSocket echo at /agent/connect.
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/connect", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		typ, msg, err := c.Read(ctx)
		if err != nil {
			return
		}
		_ = c.Write(ctx, typ, append([]byte("echo:"), msg...))
		c.Close(websocket.StatusNormalClosure, "")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "SPA-ONE ", r.URL.Path)
	})
	up1 := httptest.NewServer(mux)
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "SPA-TWO ", r.URL.Path)
	}))
	defer up2.Close()

	dir := t.TempDir()
	addr := freeAddr(t)
	cfg := Config{DataDir: dir, Listen: addr, Upstream: hostPort(up1)}
	if err := Load(cfg); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	t.Cleanup(func() { Stop() })

	// SPA through the proxy.
	if got := body(t, "http://"+addr+"/some/route"); got != "SPA-ONE /some/route" {
		t.Fatalf("SPA via proxy: %q", got)
	}

	// A WebSocket upgrade passes through the proxy (future panel sockets).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+addr+"/agent/connect", nil)
	if err != nil {
		t.Fatalf("websocket dial through proxy: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.Read(ctx)
	if err != nil || string(msg) != "echo:hi" {
		t.Fatalf("websocket echo: %q %v", msg, err)
	}
	c.CloseNow()

	// SECOND caddy.Load in-process with admin disabled: change the upstream.
	cfg.Upstream = hostPort(up2)
	if err := Load(cfg); err != nil {
		t.Fatalf("second Load (reload with admin disabled): %v", err)
	}
	if got := body(t, "http://"+addr+"/after"); got != "SPA-TWO /after" {
		t.Fatalf("after reload: %q", got)
	}

	// A reload may also move the listener.
	addr2 := freeAddr(t)
	cfg.Listen = addr2
	if err := Load(cfg); err != nil {
		t.Fatalf("third Load (new listener): %v", err)
	}
	if got := body(t, "http://"+addr2+"/moved"); got != "SPA-TWO /moved" {
		t.Fatalf("after listener move: %q", got)
	}
}
