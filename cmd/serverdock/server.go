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
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/api"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
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
	// DebugAllowRemote permits a non-loopback --debug-listen address.
	DebugAllowRemote bool
	// AgentToken is the shared spike token (SD_AGENT_TOKEN); empty disables
	// remote agents. It is never logged.
	AgentToken string
	// DockerSocket is used by the in-process local agent for log streams.
	DockerSocket string
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
	fs.StringVar(&c.DockerSocket, "docker-socket", dockerapi.DefaultSocket, "Docker socket path for the local agent")
	fs.BoolVar(&c.DebugAllowRemote, "debug-allow-remote", false, "allow --debug-listen on a non-loopback address (unauthenticated; bench networks only)")
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
	cfg.AgentToken = os.Getenv("SD_AGENT_TOKEN")
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

// checkDebugAddr refuses debug listeners that are not loopback unless the
// operator explicitly allows remote access.
func checkDebugAddr(addr string, allowRemote bool) error {
	if addr == "" || allowRemote {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("debug-listen %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("debug-listen %q is not a loopback address; pass --debug-allow-remote to expose it (bench networks only)", addr)
}

// runServer runs the control plane until ctx is cancelled. Shutdown order:
// the local agent (and with it Caddy) stops first, then the control plane.
func runServer(ctx context.Context, cfg serverConfig, log *slog.Logger, ready func(serverAddrs)) error {
	if err := checkDebugAddr(cfg.DebugListen, cfg.DebugAllowRemote); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Data, 0o750); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	st, err := store.Open(ctx, cfg.Data)
	if err != nil {
		return err
	}
	defer st.Close()

	// cpCtx bounds the control plane (HTTP servers); it outlives the agent.
	cpCtx, cpCancel := context.WithCancel(context.Background())
	defer cpCancel()
	agentCtx, agentCancel := context.WithCancel(ctx)
	defer agentCancel()

	srv := api.New(web.FS())
	h := hub.New(cpCtx, cfg.AgentToken, log)
	srv.Handle("GET "+proxy.AgentConnectPath, h)
	ln, err := net.Listen("tcp", cfg.Internal)
	if err != nil {
		return fmt.Errorf("listen internal: %w", err)
	}
	addrs := serverAddrs{Internal: ln.Addr().String()}
	cpErr := make(chan error, 2)
	cpRunning := 1
	go func() { cpErr <- api.Serve(cpCtx, ln, srv.Handler(), shutdownGrace) }()

	if cfg.DebugListen != "" {
		dl, err := net.Listen("tcp", cfg.DebugListen)
		if err != nil {
			cpCancel()
			<-cpErr
			return fmt.Errorf("listen debug: %w", err)
		}
		addrs.Debug = dl.Addr().String()
		if cfg.DebugAllowRemote {
			log.Warn("DEBUG LISTENER ALLOWED ON A NON-LOOPBACK ADDRESS (--debug-allow-remote); it is unauthenticated", "addr", addrs.Debug)
		} else {
			log.Warn("debug listener enabled", "addr", addrs.Debug)
		}
		cpRunning++
		go func() { cpErr <- api.Serve(cpCtx, dl, api.DebugHandler(h.DebugRoutes), shutdownGrace) }()
	}
	log.Info("control plane listening", "internal", addrs.Internal)

	// In-process local agent; it owns the embedded Caddy when --proxy is set.
	if cfg.Proxy {
		addrs.Listen = cfg.Listen
	}
	// The local agent registers with the hub over an in-memory pipe, so the
	// same session code path runs as for remote agents.
	pipeHub, pipeAgent := net.Pipe()
	go func() { _ = h.Attach(cpCtx, "local", pipeHub, 0) }()
	agentDone := make(chan error, 1)
	go func() {
		go func() {
			_ = agent.ServeSession(agentCtx, pipeAgent, localLogs(cfg.DockerSocket), 0, log)
		}()
		agentDone <- agent.RunLocal(agentCtx, agent.LocalOptions{
			DataDir: cfg.Data, Listen: cfg.Listen, Upstream: addrs.Internal, Proxy: cfg.Proxy,
		}, log, func() {
			if ready != nil {
				ready(addrs)
			}
		})
	}()

	var first error
	agentExited := false
	select {
	case <-ctx.Done():
	case err := <-agentDone:
		agentExited, first = true, err
	case err := <-cpErr:
		cpRunning--
		first = err
	}
	agentCancel()
	if !agentExited {
		if err := <-agentDone; err != nil && first == nil {
			first = err
		}
	}
	cpCancel()
	for ; cpRunning > 0; cpRunning-- {
		if err := <-cpErr; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// noLogs is used where no Docker socket is wanted (tests).
type noLogs struct{}

func (noLogs) Logs(context.Context, string, int, io.Writer) error {
	return errors.New("docker logs are not available")
}

func localLogs(socket string) agent.LogSource { return dockerapi.New(socket) }
