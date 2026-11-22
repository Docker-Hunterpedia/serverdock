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

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/api"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/datadir"
	"github.com/Docker-Hunterpedia/serverdock/internal/deploy"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
	"github.com/Docker-Hunterpedia/serverdock/internal/limits"
	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
	"github.com/Docker-Hunterpedia/serverdock/web"
)

const (
	serverMemLimit = 64 << 20
	shutdownGrace  = 10 * time.Second

	controlMaxConns    = 256 // one Caddy in front: no per-IP cap, every connection is ours
	agentMaxConns      = 128 // idle TLS connections are cheap; the per-IP cap does the real work
	agentHeaderTimeout = 5 * time.Second
	agentMaxConnsPerIP = 4
)

type serverConfig struct {
	Data   string
	Listen string
	// HTTPSListen is the panel's HTTPS address served by the embedded proxy;
	// Listen then only redirects to it. PanelHosts are extra names the panel
	// certificate carries (and, from M2 step 6a, the Host allowlist accepts).
	HTTPSListen string
	PanelHosts  []string
	// ControlSocket is the control plane's unix socket (mode 0600 inside a 0700
	// directory). Empty means DATA/run/cp.sock. There is no TCP listener.
	ControlSocket string
	Proxy         bool
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
	// DockerSocket is used by the in-process local agent for its Docker ops.
	DockerSocket string
	// AppSubnet is --app-subnet: the app network's subnet (/24 to /20), applied
	// at first start only. Empty means choose one clear of every network.
	AppSubnet string
	// AllowLoopbackRegistry is --allow-loopback-registry: image references may
	// name a registry on localhost, 127.0.0.0/8 or ::1 (homelab registries; the
	// integration tests). Link-local and metadata addresses stay refused.
	AllowLoopbackRegistry bool
}

// policyOptions are the install-wide switches the policy validators take.
func (c serverConfig) policyOptions() policy.Options {
	return policy.Options{AllowLoopbackRegistry: c.AllowLoopbackRegistry}
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
	var hosts stringList
	fs.StringVar(&c.HTTPSListen, "https-listen", ":443", "panel HTTPS listen address (served by the embedded proxy). --listen only redirects to it, using this port as seen inside the process: behind Docker port mapping (for example -p 8443:443) the redirect points at 443")
	fs.Var(&hosts, "panel-host", "extra DNS name or IP address of the panel, added to its certificate; repeatable")
	fs.StringVar(&c.ControlSocket, "control-socket", "", "control-plane unix socket (default DATA/run/cp.sock; 0600 inside a 0700 directory)")
	fs.BoolVar(&c.Proxy, "proxy", true, "run the embedded Caddy proxy in the local agent")
	fs.StringVar(&c.DebugSocket, "debug-socket", "", "serve /debug/* on this unix socket path (0600, inside a 0700 directory; off by default)")
	fs.StringVar(&c.AgentListen, "agent-listen", "", "accept remote (read-only, experimental) agents over TLS on this address, e.g. :7443 (off by default)")
	fs.Var(&sans, "agent-san", "DNS name or IP address agents dial the hub at; repeatable; required with --agent-listen")
	fs.IntVar(&c.AgentMaxSessions, "agent-max-sessions", hub.DefaultMaxSessions, "maximum concurrent remote agent sessions")
	fs.StringVar(&c.DockerSocket, "docker-socket", dockerapi.DefaultSocket, "Docker socket path for the local agent")
	fs.StringVar(&c.AppSubnet, "app-subnet", "", "subnet of the app network, /24 to /20 (default: the first free /24 in 10.250.0.0/16). Used at first start only; it is never changed afterwards")
	fs.BoolVar(&c.AllowLoopbackRegistry, "allow-loopback-registry", false, "allow image references that name a registry on localhost, 127.0.0.0/8 or ::1 (off by default; link-local and cloud metadata addresses are always refused)")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	c.AgentSANs = sans
	c.PanelHosts = hosts
	if err := c.withDefaults(); err != nil {
		return c, err
	}
	if c.AgentMaxSessions < 1 {
		return c, errors.New("--agent-max-sessions must be at least 1")
	}
	if err := c.validateAppSubnet(); err != nil {
		return c, err
	}
	return c, nil
}

// withDefaults fills in the control socket path: DATA/run/cp.sock, made absolute
// (a socket path must be).
func (c *serverConfig) withDefaults() error {
	if c.HTTPSListen == "" {
		c.HTTPSListen = ":443"
	}
	if c.ControlSocket != "" {
		return nil
	}
	abs, err := filepath.Abs(c.Data)
	if err != nil {
		return fmt.Errorf("--data %q: %w", c.Data, err)
	}
	c.ControlSocket = filepath.Join(abs, "run", "cp.sock")
	return nil
}

// applyMemLimit sets a soft memory limit unless GOMEMLIMIT is set.
func applyMemLimit(limit int64) {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(limit)
	}
}

func serverMain(args []string, stderr io.Writer) int {
	restrictUmask() // before any file is created: new files are 0600, new directories 0700
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
	Control string // control-plane unix socket path
	Debug   string // debug unix socket path; empty when off
	Listen  string // public HTTP (redirect-only) address; empty when --proxy=false
	HTTPS   string // panel HTTPS address; empty when --proxy=false
	Agent   string // agent TLS listener address; empty when off
	// CAFingerprint is the hub CA fingerprint ("sha256:<hex>"); empty when the
	// agent listener is off.
	CAFingerprint string

	// hub is the live hub, for tests that call ops on the local session.
	hub *hub.Hub
}

func (c serverConfig) validateAppSubnet() error {
	if c.AppSubnet == "" {
		return nil
	}
	if _, err := deploy.ParseAppSubnet(c.AppSubnet); err != nil {
		return fmt.Errorf("--app-subnet %q: %w", c.AppSubnet, err)
	}
	return nil
}

// validate checks the agent flags and the token before anything is started.
func (c serverConfig) validate() error {
	if err := c.validateAppSubnet(); err != nil {
		return err
	}
	if c.HTTPSListen != "" {
		if _, _, err := net.SplitHostPort(c.HTTPSListen); err != nil {
			return fmt.Errorf("--https-listen %q: %w", c.HTTPSListen, err)
		}
		if c.Proxy && c.HTTPSListen == c.Listen {
			return errors.New("--https-listen and --listen must be different addresses")
		}
	}
	if c.ControlSocket != "" && c.DebugSocket != "" && filepath.Clean(c.ControlSocket) == filepath.Clean(c.DebugSocket) {
		return errors.New("--control-socket and --debug-socket must be different paths")
	}
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
	if err := cfg.withDefaults(); err != nil {
		return err
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	// Startup order (plan 0003): Prepare, then the data-directory lock inside
	// store.Open, then keys, certificates and the agent token, then every
	// listener. Nothing writes under DATA before the lock, so a second server on
	// the same DATA fails here without touching the first one's files.
	if err := datadir.Prepare(cfg.Data); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.Data)
	if err != nil {
		return err
	}
	defer st.Close()
	installID, err := loadInstallID(ctx, st)
	if err != nil {
		return err
	}
	// The master key seals TOTP seeds; creating it (or refusing to start without
	// it) belongs to this point of the startup order.
	box, keyCreated, err := secretbox.LoadOrCreate(ctx, cfg.Data, secretbox.SQLMeta{R: st.R, W: st.W}, func() (bool, error) {
		return secretbox.CiphertextsExist(ctx, st.R, secretbox.Columns())
	})
	if err != nil {
		return err
	}
	if keyCreated {
		log.Info("master key created", "path", secretbox.KeyPath(cfg.Data))
	}
	if lvl := os.Getenv("SERVERDOCK_TEST_PROXY_LOG_LEVEL"); lvl != "" && lvl != "WARN" && lvl != "ERROR" {
		// Test-only knob: a verbose proxy log is not for production use.
		log.Warn("SERVERDOCK_TEST_PROXY_LOG_LEVEL is set: the embedded proxy logs at a verbose level, which is for tests only", "level", lvl)
	}
	// The panel leaf is only needed when the proxy serves the panel.
	var panel *pki.Panel
	if cfg.Proxy {
		if panel, err = pki.EnsurePanelLeaf(pki.PanelOptions{Dir: filepath.Join(cfg.Data, "pki"), Hosts: cfg.PanelHosts, Log: log}); err != nil {
			return err
		}
		// Logged at every start; browsers show the same value in the certificate
		// details. It is never shown on an unauthenticated page.
		log.Info("panel certificate", "sha256", panel.Fingerprint, "expires", panel.NotAfter.UTC().Format(time.DateOnly), "file", panel.CertFile)
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
	// The setup token (server, no admin yet only): written at every start while
	// no admin exists. Only its path is logged, never the token.
	hasher := auth.NewHasher() // one per process: every password check shares its semaphore
	authStore := auth.NewStore(st, box, hasher, auth.Options{Log: log})
	setup, err := authStore.PrepareSetup(ctx)
	if err != nil {
		return err
	}
	if setup != nil {
		fp := ""
		if panel != nil {
			fp = panel.Fingerprint
		}
		_, port, _ := net.SplitHostPort(cfg.HTTPSListen)
		log.Info(auth.StartupNotice(*setup, fp, port))
	}
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
	rawControl, err := listenPrivateSocket(cfg.ControlSocket)
	if err != nil {
		return fmt.Errorf("--control-socket: %w", err)
	}
	// 256 connections in total; no per-IP cap here, because every connection
	// comes from our own Caddy.
	ln := limits.NewListener(rawControl, controlMaxConns, 0, nil)
	opened = append(opened, ln)
	addrs.Control = cfg.ControlSocket
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
		if debugLn, err = listenPrivateSocket(cfg.DebugSocket); err != nil {
			return fmt.Errorf("--debug-socket: %w", err)
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

	maxSessions := cfg.AgentMaxSessions
	if maxSessions <= 0 {
		maxSessions = hub.DefaultMaxSessions
	}
	h := hub.New(cpCtx, token, log, hub.WithMaxSessions(maxSessions), hub.WithLocalVersion(version))
	lim := api.NewLimits(nil)
	docker := dockerapi.New(cfg.DockerSocket)
	defer docker.Close()
	logDockerVersion(ctx, docker, log)
	ops, err := localOps(docker, installID, log)
	if err != nil {
		return err
	}
	if _, err := recordAppSubnet(ctx, st, docker, installID, cfg.AppSubnet, log); err != nil {
		return err
	}
	// The action registry behind the REST handler: built-ins and the session-only
	// auth actions, never auth.admin_reset. The one-time system invoker is
	// deliberately not taken here, and the api package never receives it.
	deps := actions.Deps{Version: version, Started: time.Now(), Audit: auditLog, Docker: docker, DB: st.R}
	if panel != nil {
		deps.PanelCert = func() actions.PanelCert {
			return actions.PanelCert{Fingerprint: panel.Fingerprint, Created: panel.NotBefore, NotAfter: panel.NotAfter,
				RenewSoon: time.Until(panel.NotAfter) < pki.PanelRenewBefore}
		}
	}
	reg := newServerRegistry(auditLog, log, deps, authStore, auth.ActionDeps{Hasher: hasher, PasswordGate: lim.AccountPassword})
	srv := api.New(api.Config{
		SPA: web.FS(), Sessions: authStore, Actions: reg, Tokens: api.NewTokenVerifier(authStore), PanelHosts: cfg.PanelHosts,
		Auth:       api.NewAuthHandlers(api.AuthDeps{Auth: authStore, Hasher: hasher, Audit: auditLog, Limits: lim, Log: log}),
		DebugExtra: []func(*http.ServeMux){h.DebugRoutes}, Log: log,
	})
	started = true
	cpErr := make(chan error, 3)
	cpRunning := 1
	// Only this listener is marked as "from our proxy": Caddy dials it, and its
	// X-Forwarded-For (exactly one value) is the client address.
	go func() { cpErr <- api.Serve(cpCtx, ln, srv.Handler(), shutdownGrace, api.WithTrustedProxy(log)) }()
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
		go func() { cpErr <- api.Serve(cpCtx, debugLn, srv.DebugHandler(), shutdownGrace) }()
	}
	log.Info("control plane listening", "socket", addrs.Control)

	// In-process local agent; it owns the embedded Caddy when --proxy is set.
	if cfg.Proxy {
		addrs.Listen, addrs.HTTPS = cfg.Listen, cfg.HTTPSListen
	}
	addrs.hub = h
	// The local agent registers with the hub over an in-memory pipe, so the
	// same session code path runs as for remote agents.
	// Both session goroutines are joined before runServer returns.
	var session sync.WaitGroup
	pipeHub, pipeAgent := net.Pipe()
	session.Go(func() { _ = h.Attach(cpCtx, hub.LocalID, pipeHub, 0) })
	session.Go(func() {
		_ = agent.Serve(agentCtx, pipeAgent, agent.Config{Logs: docker, Version: version, Mode: agent.ModeLocal, Ops: ops}, 0, log)
	})
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- agent.RunLocal(agentCtx, agent.LocalOptions{
			Panel: panelProxy(panel, cfg),
			// Read by the secret-sentinel test only (not a flag, not in --help).
			ProxyLogLevel: os.Getenv("SERVERDOCK_TEST_PROXY_LOG_LEVEL"),
			DataDir:       cfg.Data, Listen: cfg.Listen, Upstream: proxy.UnixUpstream(cfg.ControlSocket), Proxy: cfg.Proxy,
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

// panelProxy is the proxy's HTTPS panel part, nil when there is no panel leaf.
func panelProxy(p *pki.Panel, cfg serverConfig) *proxy.Panel {
	if p == nil {
		return nil
	}
	return &proxy.Panel{Listen: cfg.HTTPSListen, CertFile: p.CertFile, Hosts: cfg.PanelHosts}
}

// newServerRegistry is the server's action registry: the built-ins only. It never
// gets auth.admin_reset: that action is console-only and exists solely in the
// registry `serverdock admin reset` builds (TestServerRegistryHasNoAdminReset).
func newServerRegistry(sink actions.Sink, log *slog.Logger, deps actions.Deps, as *auth.Store, ad auth.ActionDeps) *actions.Registry {
	reg := actions.New(sink, log)
	actions.RegisterBuiltins(reg, deps)
	auth.RegisterAuthActions(reg, as, ad)
	return reg
}
