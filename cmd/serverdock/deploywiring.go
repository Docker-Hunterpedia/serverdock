package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"regexp"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/deploy"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// loadInstallID reads meta.install_id, which migration 0004 creates (16 random
// bytes in lowercase hex). The agent sets serverdock.instance to it and names the
// app network after it, so two installs on one Docker host never touch each
// other's containers.
func loadInstallID(ctx context.Context, st *store.Store) (string, error) {
	var id string
	err := st.R.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'install_id'`).Scan(&id)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("meta.install_id is missing: the database migration did not run")
	}
	if err != nil {
		return "", err
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		return "", fmt.Errorf("meta.install_id is not 32 lowercase hex characters")
	}
	return id, nil
}

// localOps builds the handlers of the local-only agent ops for the in-process
// local agent. The remote agent process never gets them: its handler table holds
// RemoteOK ops only, whatever Config.Ops says. CheckCreate is policy.CheckCreateAgent:
// default-deny on the exact bytes sent to Docker.
//
// applier is the embedded proxy's route applier for proxy_apply; nil when the
// proxy is off (the op then says so). reserved are ServerDock's own listen ports
// besides --listen and --https-listen, which an app route may not use.
//
// appSubnet reads the recorded meta.app_subnet on each proxy_apply that carries
// an upstream (no cache, no timer; it is recorded lazily after start).
func localOps(docker *dockerapi.Client, instance string, allowLoopbackRegistry bool, applier *proxy.Applier, reserved []int, appSubnet func(context.Context) (string, error), log *slog.Logger) (map[string]agent.Handler, error) {
	d := agentops.Deps{Docker: docker, Instance: instance, CheckCreate: policy.CheckCreateAgent, AllowLoopbackRegistry: allowLoopbackRegistry, ReservedPorts: reserved, AppSubnet: appSubnet, Log: log}
	if applier != nil { // never a typed nil in the interface
		d.Proxy = applier
	}
	return agentops.Handlers(d)
}

// recordAppSubnet picks (first start) or reads (later starts) the app subnet and
// returns the AppNet that checks it later. It can block startup for up to 10 s
// when Docker is down. A failure is logged and not fatal (without Docker nothing
// can be deployed anyway, and the choice is retried the next time something asks
// for it), except an explicit --app-subnet that overlaps the host or a Docker
// network: that refuses to start and records nothing.
func recordAppSubnet(ctx context.Context, st *store.Store, docker *dockerapi.Client, instance, flag string, log *slog.Logger) (*deploy.AppNet, error) {
	an, err := deploy.NewAppNet(deploy.AppNetOptions{Store: st, Instance: instance, Flag: flag, Docker: docker, Log: log})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := an.Subnet(ctx); err != nil {
		var oe *deploy.OverlapError
		if errors.As(err, &oe) && oe.FirstStart {
			return nil, err // an explicit --app-subnet that clashes: refuse to start, save nothing
		}
		log.Warn("app subnet not recorded yet", "err", err)
	}
	return an, nil
}

// localCaller runs the local agent's ops over its hub session. The session is
// registered a moment after the server starts, so a call that finds none waits a
// short, bounded time (500 ms, in 10 ms steps, only while there is no session)
// before it fails; nothing polls otherwise.
type localCaller struct{ h *hub.Hub }

var errLocalAgentDown = deploy.ErrAgentDown

func (l localCaller) Call(ctx context.Context, op string, args any, onLine func(agent.Line)) (json.RawMessage, error) {
	for i := 0; ; i++ {
		if s, ok := l.h.Get(hub.LocalID); ok {
			return s.Call(ctx, op, args, onLine)
		}
		if i >= 50 {
			return nil, errLocalAgentDown
		}
		t := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// localLogs opens a container's log without following it, through the local
// agent's hub session (the agent's `logs` op with follow:false).
type localLogs struct{ h *hub.Hub }

func (l localLogs) LogsOnce(ctx context.Context, container string, tail int) (io.ReadCloser, error) {
	s, ok := l.h.Get(hub.LocalID)
	if !ok {
		return nil, errLocalAgentDown
	}
	return s.LogsOnce(ctx, container, tail)
}

// panelAddrs returns the function that lists the addresses the panel is reachable
// on, for the exposure check: the one it is bound to, or, when it listens on every
// interface (the default), every address of this host including loopback (an SSH
// tunnel reaches the panel there). It runs only when the exposure cache refreshes.
func panelAddrs(httpsListen string) func() []netip.Addr {
	return func() []netip.Addr {
		if host, _, err := net.SplitHostPort(httpsListen); err == nil && host != "" {
			if a, err := netip.ParseAddr(host); err == nil && !a.IsUnspecified() {
				return []netip.Addr{a.Unmap()}
			}
		}
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			return nil
		}
		var out []netip.Addr
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(ipn.IP); ok {
					out = append(out, ip.Unmap())
				}
			}
		}
		return out
	}
}
