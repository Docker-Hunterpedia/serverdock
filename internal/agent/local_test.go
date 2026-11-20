package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestRunLocalWithoutProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- RunLocal(ctx, LocalOptions{DataDir: t.TempDir()}, log, func() { close(started) }) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("not started")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunLocalBadProxyConfig(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := RunLocal(context.Background(), LocalOptions{DataDir: t.TempDir(), Listen: "bad", Upstream: "127.0.0.1:1", Proxy: true}, log, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}
