package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

func startServer(t *testing.T, cfg serverConfig) (serverAddrs, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan serverAddrs, 1)
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- runServer(ctx, cfg, log, func(a serverAddrs) { ready <- a }) }()
	select {
	case a := <-ready:
		return a, func() error {
			cancel()
			select {
			case err := <-done:
				return err
			case <-time.After(10 * time.Second):
				t.Fatal("server did not stop")
				return nil
			}
		}
	case err := <-done:
		cancel()
		t.Fatalf("server exited early: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("server not ready")
	}
	return serverAddrs{}, nil
}

func TestParseServerFlags(t *testing.T) {
	var buf bytes.Buffer
	c, err := parseServerFlags(nil, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":80" || c.Internal != "127.0.0.1:7070" || !c.Proxy || c.DebugSocket != "" || c.AgentListen != "" || len(c.AgentSANs) != 0 || c.Data != "/var/lib/serverdock" {
		t.Fatalf("defaults: %+v", c)
	}
	c, err = parseServerFlags([]string{"--proxy=false", "--debug-socket", "/run/sd/d.sock", "--data", "/x",
		"--agent-listen", ":7443", "--agent-san", "hub.example", "--agent-san", "203.0.113.7"}, &buf)
	if err != nil || c.Proxy || c.DebugSocket != "/run/sd/d.sock" || c.Data != "/x" || c.AgentListen != ":7443" ||
		len(c.AgentSANs) != 2 || c.AgentSANs[1] != "203.0.113.7" {
		t.Fatalf("%+v %v", c, err)
	}
	// The TCP debug listener and its remote override are gone.
	for _, gone := range []string{"--debug-listen", "--debug-allow-remote"} {
		if _, err := parseServerFlags([]string{gone, "127.0.0.1:1"}, &buf); err == nil {
			t.Errorf("%s still exists", gone)
		}
	}
	if _, err := parseServerFlags([]string{"extra"}, &buf); err == nil {
		t.Error("expected error for positional args")
	}
}

func TestServerDebugOffByDefault(t *testing.T) {
	addrs, stop := startServer(t, serverConfig{Data: t.TempDir(), Internal: "127.0.0.1:0"})
	if addrs.Debug != "" {
		t.Fatal("debug listener started without flag")
	}
	resp, err := http.Get("http://" + addrs.Internal + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp, err)
	}
	resp.Body.Close()
	resp, err = http.Get("http://" + addrs.Internal + "/debug/mem")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 && resp.Header.Get("Content-Type") == "application/json" {
		t.Error("/debug/mem exposed on the control-plane listener")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// sockDir returns a short temp dir (unix socket paths are limited to ~104 bytes).
func sockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// unixClient talks HTTP over the debug socket.
func unixClient(sock string) *http.Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}, DisableKeepAlives: true}
	return &http.Client{Transport: tr}
}

func TestServerDebugSocket(t *testing.T) {
	sock := filepath.Join(sockDir(t), "dbg", "d.sock")
	addrs, stop := startServer(t, serverConfig{Data: t.TempDir(), Internal: "127.0.0.1:0", DebugSocket: sock})
	if addrs.Debug != sock {
		t.Fatalf("Debug = %q", addrs.Debug)
	}
	st, err := os.Stat(sock)
	if err != nil || st.Mode().Perm() != 0o600 || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket %v %v", st, err)
	}
	if ds, err := os.Stat(filepath.Dir(sock)); err != nil || ds.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir %v %v", ds, err)
	}
	resp, err := unixClient(sock).Get("http://debug/debug/mem")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("debug/mem: %v %v", resp, err)
	}
	resp.Body.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); err == nil {
		t.Error("socket file left behind after shutdown")
	}
}

func TestDebugSocketDirRules(t *testing.T) {
	base := sockDir(t)
	// A group/world accessible directory is refused.
	open := filepath.Join(base, "open")
	os.Mkdir(open, 0o755)
	os.Chmod(open, 0o755)
	if _, err := listenDebugSocket(filepath.Join(open, "d.sock")); err == nil {
		t.Error("0755 directory accepted")
	}
	// A regular file at the socket path is not replaced.
	priv := filepath.Join(base, "priv")
	os.Mkdir(priv, 0o700)
	os.WriteFile(filepath.Join(priv, "d.sock"), []byte("x"), 0o600)
	if _, err := listenDebugSocket(filepath.Join(priv, "d.sock")); err == nil {
		t.Error("a regular file was replaced")
	}
	// A stale socket from a crash is replaced.
	os.Remove(filepath.Join(priv, "d.sock"))
	stale, _ := net.Listen("unix", filepath.Join(priv, "d.sock"))
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	ln, err := listenDebugSocket(filepath.Join(priv, "d.sock"))
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	ln.Close()
	if _, err := listenDebugSocket("relative.sock"); err == nil {
		t.Error("relative path accepted")
	}
}

func TestServerWithProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	ln.Close()
	addrs, stop := startServer(t, serverConfig{Data: t.TempDir(), Internal: "127.0.0.1:0", Listen: listen, Proxy: true})
	if addrs.Listen != listen {
		t.Fatalf("Listen = %q", addrs.Listen)
	}
	for _, p := range []string{"/", "/healthz"} {
		resp, err := http.Get("http://" + listen + p)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET %s via proxy: %v %v", p, resp, err)
		}
		resp.Body.Close()
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get("http://" + listen + "/"); err == nil {
		t.Error("proxy still listening after shutdown")
	}
}

var testToken = strings.Repeat("t0k3n-", 8) // 48 characters, base64url-safe

func TestServerHubLocalAndRemoteAgentsOverTLS(t *testing.T) {
	sock := filepath.Join(sockDir(t), "d.sock")
	data := t.TempDir()
	addrs, stop := startServer(t, serverConfig{Data: data, Internal: "127.0.0.1:0", DebugSocket: sock,
		AgentListen: "127.0.0.1:0", AgentSANs: []string{"127.0.0.1"}, AgentToken: testToken})
	if addrs.Agent == "" || !strings.HasPrefix(addrs.CAFingerprint, "sha256:") {
		t.Fatalf("agent listener not reported: %+v", addrs)
	}
	dc := unixClient(sock)
	list := func() string {
		resp, err := dc.Get("http://debug/debug/agents")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	waitFor := func(sub string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if strings.Contains(list(), sub) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("agents list never contained %s: %s", sub, list())
	}
	waitFor(`"local"`) // in-process agent registered over the in-memory pipe

	// Plaintext HTTP, a wrong CA pin, and a wrong token never create a session.
	// (Go answers a plaintext request on a TLS port with a bare 400.)
	if resp, err := http.Get("http://" + addrs.Agent + "/agent/connect"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("plaintext HTTP on the TLS listener got %d, want 400", resp.StatusCode)
		}
	}
	badFP := "sha256:" + strings.Repeat("00", 32)
	ctxBad, cancelBad := context.WithTimeout(context.Background(), 2*time.Second)
	_ = agent.RunClient(ctxBad, agent.ClientConfig{URL: "wss://" + addrs.Agent + "/agent/connect", CAFingerprint: badFP, Token: testToken, ID: "wrongpin",
		Logs: noLogs{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	cancelBad()
	if strings.Contains(list(), "wrongpin") {
		t.Fatal("agent with the wrong CA pin connected")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = agent.RunClient(ctx, agent.ClientConfig{URL: "wss://" + addrs.Agent + "/agent/connect", CAFingerprint: addrs.CAFingerprint,
			Token: testToken, ID: "remote1", Logs: noLogs{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}()
	defer func() { cancel(); <-done }()
	waitFor(`"remote1"`)

	// The token and the CA key are not exposed anywhere the debug listing reaches.
	if strings.Contains(list(), testToken) {
		t.Error("token in the debug listing")
	}

	// Shutdown must complete promptly even with a hijacked WebSocket session open.
	t0 := time.Now()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("shutdown with a connected agent took %v", d)
	}
}

// /agent/connect is not served on the control-plane listener (and so not by
// Caddy on :80): it answers 404, not the SPA fallback.
func TestAgentConnectIs404OnControlPlaneAndProxy(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	listen := ln.Addr().String()
	ln.Close()
	addrs, stop := startServer(t, serverConfig{Data: t.TempDir(), Internal: "127.0.0.1:0", Listen: listen, Proxy: true,
		AgentListen: "127.0.0.1:0", AgentSANs: []string{"127.0.0.1"}, AgentToken: testToken})
	for _, base := range []string{"http://" + addrs.Internal, "http://" + listen} {
		for _, p := range []string{"/agent/connect", "/agent/", "/agent/anything"} {
			resp, err := http.Get(base + p)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s%s = %d, want 404", base, p, resp.StatusCode)
			}
		}
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// The 5th concurrent TCP connection from one IP to the agent listener is closed
// by the server in Accept (before any TLS work); the other four stay open.
func TestAgentListenerPerIPConnectionCap(t *testing.T) {
	addrs, stop := startServer(t, serverConfig{Data: t.TempDir(), Internal: "127.0.0.1:0",
		AgentListen: "127.0.0.1:0", AgentSANs: []string{"127.0.0.1"}, AgentToken: testToken})
	var conns []net.Conn
	for i := 0; i < agentMaxConnsPerIP+1; i++ {
		c, err := net.Dial("tcp", addrs.Agent)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	closed := 0
	for _, c := range conns {
		c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				closed++
			}
		}
	}
	if closed != 1 {
		t.Errorf("connections closed by the server = %d, want exactly 1", closed)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentMaxSessionsFlag(t *testing.T) {
	var buf bytes.Buffer
	c, err := parseServerFlags(nil, &buf)
	if err != nil || c.AgentMaxSessions != 16 {
		t.Fatalf("default: %+v %v", c, err)
	}
	if c, err = parseServerFlags([]string{"--agent-max-sessions", "3"}, &buf); err != nil || c.AgentMaxSessions != 3 {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{"0", "-1"} {
		if _, err := parseServerFlags([]string{"--agent-max-sessions", bad}, &buf); err == nil {
			t.Errorf("--agent-max-sessions %s accepted", bad)
		}
	}
}

func TestAgentListenNeedsSANAndToken(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// No SAN: refuses to start, and nothing is bound.
	err := runServer(context.Background(), serverConfig{Data: t.TempDir(), Internal: "127.0.0.1:0", AgentListen: "127.0.0.1:0"}, quiet, nil)
	if err == nil || !strings.Contains(err.Error(), "--agent-san") {
		t.Fatalf("agent-listen without SAN: %v", err)
	}
	// SAN without a listener is a mistake, not silently ignored.
	err = runServer(context.Background(), serverConfig{Data: t.TempDir(), Internal: "127.0.0.1:0", AgentSANs: []string{"x.example"}}, quiet, nil)
	if err == nil || !strings.Contains(err.Error(), "--agent-listen") {
		t.Fatalf("agent-san without listen: %v", err)
	}
	// Bad listen address / bad SAN.
	for _, c := range []serverConfig{
		{AgentListen: "nope", AgentSANs: []string{"x.example"}},
		{AgentListen: "127.0.0.1:0", AgentSANs: []string{"bad name"}},
	} {
		c.Data, c.Internal = t.TempDir(), "127.0.0.1:0"
		if err := runServer(context.Background(), c, quiet, nil); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	// A short token is refused, with or without the listener, and never echoed.
	for _, c := range []serverConfig{
		{AgentToken: "short-secret-value"},
		{AgentToken: "short-secret-value", AgentListen: "127.0.0.1:0", AgentSANs: []string{"127.0.0.1"}},
	} {
		c.Data, c.Internal = t.TempDir(), "127.0.0.1:0"
		err := runServer(context.Background(), c, quiet, nil)
		if err == nil || !strings.Contains(err.Error(), "too short") || strings.Contains(err.Error(), "short-secret-value") {
			t.Errorf("short token: %v", err)
		}
	}
}

func TestGeneratedAgentTokenFile(t *testing.T) {
	data := t.TempDir()
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan serverAddrs, 1)
	done := make(chan error, 1)
	go func() {
		done <- runServer(ctx, serverConfig{Data: data, Internal: "127.0.0.1:0", AgentListen: "127.0.0.1:0", AgentSANs: []string{"127.0.0.1"}},
			slog.New(slog.NewTextHandler(&logs, nil)), func(a serverAddrs) { ready <- a })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, "agent.token")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", st, err)
	}
	tok, _ := os.ReadFile(path)
	if len(strings.TrimSpace(string(tok))) != 43 {
		t.Errorf("generated token length %d", len(strings.TrimSpace(string(tok))))
	}
	if !strings.Contains(logs.String(), path) {
		t.Error("the token file path is not logged")
	}
	if strings.Contains(logs.String(), strings.TrimSpace(string(tok))) {
		t.Fatal("the generated token was logged")
	}
	for _, name := range []string{"pki/ca.key", "pki/hub.pem"} {
		if st, err := os.Stat(filepath.Join(data, name)); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", name, st, err)
		}
	}
}
