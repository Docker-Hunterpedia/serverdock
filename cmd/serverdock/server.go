package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/api"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
	"github.com/Docker-Hunterpedia/serverdock/web"
)

const (
	serverMemLimit = 64 << 20
	shutdownGrace  = 10 * time.Second
)

type serverConfig struct {
	Data        string
	Listen      string
	Internal    string
	Proxy       bool
	DebugListen string
}

func parseServerFlags(args []string, stderr io.Writer) (serverConfig, error) {
	var c serverConfig
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.Data, "data", "/var/lib/serverdock", "data directory")
	fs.StringVar(&c.Listen, "listen", ":80", "public listen address (served by the embedded proxy)")
	fs.StringVar(&c.Internal, "internal", "127.0.0.1:7070", "control-plane listen address (loopback)")
	fs.BoolVar(&c.Proxy, "proxy", true, "run the embedded Caddy proxy in the local agent")
	fs.StringVar(&c.DebugListen, "debug-listen", "", "serve /debug/mem on this address (off by default)")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return c, nil
}

// applyMemLimit sets a soft memory limit unless GOMEMLIMIT is set.
func applyMemLimit(limit int64) {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(limit)
	}
}

func serverMain(args []string, stderr io.Writer) int {
	cfg, err := parseServerFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "serverdock server:", err)
		return 2
	}
	applyMemLimit(serverMemLimit)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log := slog.New(slog.NewTextHandler(stderr, nil))
	if err := runServer(ctx, cfg, log, nil); err != nil {
		log.Error("server stopped", "err", err)
		return 1
	}
	return 0
}

// serverAddrs are the bound addresses, reported through the ready callback so
// tests can use random free ports.
type serverAddrs struct {
	Internal string
	Debug    string
	Listen   string // public proxy address; empty when --proxy=false
}

// runServer runs the control plane until ctx is cancelled.
func runServer(ctx context.Context, cfg serverConfig, log *slog.Logger, ready func(serverAddrs)) error {
	if err := os.MkdirAll(cfg.Data, 0o750); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	st, err := store.Open(ctx, cfg.Data)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	srv := api.New(web.FS())
	ln, err := net.Listen("tcp", cfg.Internal)
	if err != nil {
		return fmt.Errorf("listen internal: %w", err)
	}
	addrs := serverAddrs{Internal: ln.Addr().String()}
	errc := make(chan error, 3)
	running := 1
	go func() { errc <- api.Serve(ctx, ln, srv.Handler(), shutdownGrace) }()

	if cfg.DebugListen != "" {
		dl, err := net.Listen("tcp", cfg.DebugListen)
		if err != nil {
			cancel()
			<-errc
			return fmt.Errorf("listen debug: %w", err)
		}
		addrs.Debug = dl.Addr().String()
		log.Warn("debug listener enabled", "addr", addrs.Debug)
		running++
		go func() { errc <- api.Serve(ctx, dl, api.DebugHandler(), shutdownGrace) }()
	}
	log.Info("control plane listening", "internal", addrs.Internal)

	// In-process local agent; it owns the embedded Caddy when --proxy is set.
	if cfg.Proxy {
		addrs.Listen = cfg.Listen
	}
	running++
	go func() {
		errc <- agent.RunLocal(ctx, agent.LocalOptions{
			DataDir: cfg.Data, Listen: cfg.Listen, Upstream: addrs.Internal, Proxy: cfg.Proxy,
		}, log, func() {
			if ready != nil {
				ready(addrs)
			}
		})
	}()

	var first error
	for i := 0; i < running; i++ {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
		cancel()
	}
	return first
}
