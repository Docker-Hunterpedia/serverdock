package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
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
	if c.Listen != ":80" || c.Internal != "127.0.0.1:7070" || !c.Proxy || c.DebugListen != "" || c.Data != "/var/lib/serverdock" {
		t.Fatalf("defaults: %+v", c)
	}
	c, err = parseServerFlags([]string{"--proxy=false", "--debug-listen", "127.0.0.1:1", "--data", "/x"}, &buf)
	if err != nil || c.Proxy || c.DebugListen != "127.0.0.1:1" || c.Data != "/x" {
		t.Fatalf("%+v %v", c, err)
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

func TestServerDebugListener(t *testing.T) {
	addrs, stop := startServer(t, serverConfig{Data: t.TempDir(), Internal: "127.0.0.1:0", DebugListen: "127.0.0.1:0"})
	resp, err := http.Get("http://" + addrs.Debug + "/debug/mem")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("debug/mem: %v %v", resp, err)
	}
	resp.Body.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
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

func TestCheckDebugAddr(t *testing.T) {
	ok := []string{"", "127.0.0.1:7071", "localhost:7071", "[::1]:7071"}
	for _, a := range ok {
		if err := checkDebugAddr(a, false); err != nil {
			t.Errorf("%q should be allowed: %v", a, err)
		}
	}
	bad := []string{":7071", "0.0.0.0:7071", "10.1.2.3:7071", "example.com:7071", "[::]:7071", "nonsense"}
	for _, a := range bad {
		if err := checkDebugAddr(a, false); err == nil {
			t.Errorf("%q should be refused", a)
		}
		if a != "nonsense" {
			if err := checkDebugAddr(a, true); err != nil {
				t.Errorf("%q with allow-remote: %v", a, err)
			}
		}
	}
}

func TestServerRefusesRemoteDebug(t *testing.T) {
	err := runServer(context.Background(), serverConfig{Data: t.TempDir(), Internal: "127.0.0.1:0", DebugListen: "0.0.0.0:0"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err == nil {
		t.Fatal("expected refusal")
	}
}

func TestServerHubLocalAndRemoteAgents(t *testing.T) {
	addrs, stop := startServer(t, serverConfig{Data: t.TempDir(), Internal: "127.0.0.1:0", DebugListen: "127.0.0.1:0", AgentToken: "tok"})
	list := func() string {
		resp, err := http.Get("http://" + addrs.Debug + "/debug/agents")
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

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = agent.RunClient(ctx, agent.ClientConfig{URL: "ws://" + addrs.Internal + "/agent/connect", Token: "tok", ID: "remote1",
			Logs: noLogs{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}()
	defer func() { cancel(); <-done }()
	waitFor(`"remote1"`)

	// Shutdown must complete promptly even with a hijacked WebSocket session open.
	t0 := time.Now()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("shutdown with a connected agent took %v", d)
	}
}
