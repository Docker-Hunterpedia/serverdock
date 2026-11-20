package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
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
