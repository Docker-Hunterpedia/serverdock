package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/api"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
	"github.com/Docker-Hunterpedia/serverdock/internal/limits"
	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
	"github.com/Docker-Hunterpedia/serverdock/web"
)

const (
	serverMemLimit = 64 << 20
	shutdownGrace  = 10 * time.Second

	internalMaxConns   = 256
	agentMaxConns      = 128 // idle TLS connections are cheap; the per-IP cap does the real work
	agentHeaderTimeout = 5 * time.Second
	agentMaxConnsPerIP = 4
)

type serverConfig struct {
	Data     string
	Listen   string
	Internal string
	Proxy    bool
	// DebugSocket, if set, serves /debug/* on a unix socket (mode 0600 inside
	// a 0700 directory). There is no TCP debug listener.
	DebugSocket string
	// AgentListen, if set, serves GET /agent/connect over TLS 1.3 on its own
	// listener (ADR 0009). Off by default. AgentSANs are required with it.
	AgentListen string
	AgentSANs   []string
	// AgentMaxSessions caps remote agent sessions; 0 means hub.DefaultMaxSessions.
	AgentMaxSessions int
	// AgentToken is the shared agent token (SD_AGENT_TOKEN), at least 43
	// characters. If empty and AgentListen is set, one is generated into
	// DATA/agent.token. It is never logged.
	AgentToken string
	// DockerSocket is used by the in-process local agent for log streams.
	DockerSocket string
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func parseServerFlags(args []string, stderr io.Writer) (serverConfig, error) {
	var c serverConfig
	var sans stringList
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.Data, "data", "/var/lib/serverdock", "data directory")
	fs.StringVar(&c.Listen, "listen", ":80", "public listen address (served by the embedded proxy)")
	fs.StringVar(&c.Internal, "internal", "127.0.0.1:7070", "control-plane listen address (loopback)")
	fs.BoolVar(&c.Proxy, "proxy", true, "run the embedded Caddy proxy in the local agent")
	fs.StringVar(&c.DebugSocket, "debug-socket", "", "serve /debug/* on this unix socket path (0600, inside a 0700 directory; off by default)")
	fs.StringVar(&c.AgentListen, "agent-listen", "", "accept remote (read-only, experimental) agents over TLS on this address, e.g. :7443 (off by default)")
	fs.Var(&sans, "agent-san", "DNS name or IP address agents dial the hub at; repeatable; required with --agent-listen")
	fs.IntVar(&c.AgentMaxSessions, "agent-max-sessions", hub.DefaultMaxSessions, "maximum concurrent remote agent sessions")
	fs.StringVar(&c.DockerSocket, "docker-socket", dockerapi.DefaultSocket, "Docker socket path for the local agent")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	c.AgentSANs = sans
	if c.AgentMaxSessions < 1 {
		return c, errors.New("--agent-max-sessions must be at least 1")
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
	Debug    string // debug unix socket path; empty when off
	Listen   string // public proxy address; empty when --proxy=false
	Agent    string // agent TLS listener address; empty when off
	// CAFingerprint is the hub CA fingerprint ("sha256:<hex>"); empty when the
	// agent listener is off.
	CAFingerprint string
}

// validate checks the agent flags and the token before anything is started.
func (c serverConfig) validate() error {
	if c.AgentListen == "" && len(c.AgentSANs) > 0 {
		return errors.New("--agent-san requires --agent-listen")
	}
	if c.AgentListen != "" {
		if _, _, err := net.SplitHostPort(c.AgentListen); err != nil {
			return fmt.Errorf("--agent-listen %q: %w", c.AgentListen, err)
		}
		if len(c.AgentSANs) == 0 {
			return errors.New("--agent-san is required with --agent-listen: name the DNS names or IP addresses agents dial the hub at (there is no default, because auto-detected addresses would include Docker bridge IPs)")
		}
	}
	if c.AgentToken != "" {
		if err := hub.ValidateToken(c.AgentToken); err != nil {
			return fmt.Errorf("SD_AGENT_TOKEN: %w", err)
		}
	}
	return nil
}

// runServer runs the control plane until ctx is cancelled. Shutdown order:
// the local agent (and with it Caddy) stops first, then the control plane.
func runServer(ctx context.Context, cfg serverConfig, log *slog.Logger, ready func(serverAddrs)) (retErr error) {
	if err := cfg.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Data, 0o750); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	token := cfg.AgentToken
	var mgr *pki.Manager
	if cfg.AgentListen != "" {
		if token == "" {
			var path string
			var err error
			if token, path, err = hub.LoadOrCreateToken(cfg.Data); err != nil {
				return err
			}
			log.Info("agent token is in a file (the token is not logged)", "path", path)
		}
		var err error
		mgr, err = pki.Open(pki.Options{Dir: filepath.Join(cfg.Data, "pki"), SANs: cfg.AgentSANs, Log: log})
		if err != nil {
			return err
		}
		log.Info("hub CA ready; agents pin this fingerprint with --ca-fingerprint", "fingerprint", mgr.CAFingerprint())
	}
	st, err := store.Open(ctx, cfg.Data)
	if err != nil {
		return err
	}
	defer st.Close()
	auditLog, err := audit.New(st, log)
	if err != nil {
		return err
	}
	if err := auditLog.LogHead(ctx); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	defer func() { _ = auditLog.Flush(context.WithoutCancel(ctx)) }()

	// Open every listener first, so a failure leaves nothing half-started.
	var opened []io.Closer
	started := false
	defer func() {
		if !started {
			for _, c := range opened {
				_ = c.Close()
			}
		}
	}()
	addrs := serverAddrs{}
	rawInternal, err := net.Listen("tcp", cfg.Internal)
	if err != nil {
		return fmt.Errorf("listen internal: %w", err)
	}
	// 256 connections in total; no per-IP cap here, because every request from
	// our own Caddy arrives from the same loopback address.
	ln := limits.NewListener(rawInternal, internalMaxConns, 0, nil)
	opened = append(opened, ln)
	addrs.Internal = rawInternal.Addr().String()
	var agentLn, debugLn net.Listener
	if mgr != nil {
		raw, err := net.Listen("tcp", cfg.AgentListen)
		if err != nil {
			return fmt.Errorf("listen agent: %w", err)
		}
		opened = append(opened, raw)
		// Caps apply to the raw TCP connection, before any TLS work: 64 in total
		// and 4 per client IP (a connection over the cap is closed in Accept).
		agentLn = tls.NewListener(limits.NewListener(raw, agentMaxConns, agentMaxConnsPerIP, nil), mgr.TLSConfig())
		addrs.Agent, addrs.CAFingerprint = raw.Addr().String(), mgr.CAFingerprint()
	}
	if cfg.DebugSocket != "" {
		if debugLn, err = listenDebugSocket(cfg.DebugSocket); err != nil {
			return err
		}
		opened = append(opened, debugLn)
		addrs.Debug = cfg.DebugSocket
		log.Warn("debug socket enabled", "path", cfg.DebugSocket)
	}

	// cpCtx bounds the control plane (HTTP servers); it outlives the agent.
	cpCtx, cpCancel := context.WithCancel(context.Background())
	defer cpCancel()
	agentCtx, agentCancel := context.WithCancel(ctx)
	defer agentCancel()

	srv := api.New(web.FS())
	maxSessions := cfg.AgentMaxSessions
	if maxSessions <= 0 {
		maxSessions = hub.DefaultMaxSessions
	}
	h := hub.New(cpCtx, token, log, hub.WithMaxSessions(maxSessions))
	started = true
	cpErr := make(chan error, 3)
	cpRunning := 1
	go func() { cpErr <- api.Serve(cpCtx, ln, srv.Handler(), shutdownGrace) }()
	if agentLn != nil {
		// Only GET /agent/connect is served here; it is not behind Caddy, so a
		// proxy reload cannot drop agents (ADR 0009).
		mux := http.NewServeMux()
		mux.Handle("GET "+agent.ConnectPath, h)
		cpRunning++
		// The only request is one upgrade: no keep-alives, and 5 s for the TLS
		// handshake plus headers (an idle or slow handshake is dropped quickly).
		go func() {
			cpErr <- api.Serve(cpCtx, agentLn, mux, shutdownGrace,
				api.WithReadHeaderTimeout(agentHeaderTimeout), api.WithoutKeepAlives())
		}()
		log.Info("agent listener (TLS 1.3, experimental, logs only)", "addr", addrs.Agent)
	}
	if debugLn != nil {
		cpRunning++
		go func() { cpErr <- api.Serve(cpCtx, debugLn, api.DebugHandler(h.DebugRoutes), shutdownGrace) }()
	}
	log.Info("control plane listening", "internal", addrs.Internal)

	// In-process local agent; it owns the embedded Caddy when --proxy is set.
	if cfg.Proxy {
		addrs.Listen = cfg.Listen
	}
	// The local agent registers with the hub over an in-memory pipe, so the
	// same session code path runs as for remote agents.
	// Both session goroutines are joined before runServer returns.
	var session sync.WaitGroup
	docker := dockerapi.New(cfg.DockerSocket)
	defer docker.Close()
	logDockerVersion(ctx, docker, log)
	pipeHub, pipeAgent := net.Pipe()
	session.Go(func() { _ = h.Attach(cpCtx, hub.LocalID, pipeHub, 0) })
	session.Go(func() {
		_ = agent.ServeSession(agentCtx, pipeAgent, docker, 0, log)
	})
	agentDone := make(chan error, 1)
	go func() {
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
	session.Wait()
	return first
}

// noLogs is used where no Docker socket is wanted (tests).
type noLogs struct{}

func (noLogs) Logs(context.Context, string, int, io.Writer) error {
	return errors.New("docker logs are not available")
}
