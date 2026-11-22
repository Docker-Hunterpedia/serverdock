// Package agent is the agent runtime. In Phase 0 step 4 it provides the
// in-process local agent used by `serverdock server`: it owns the embedded
// Caddy proxy. The dial-out client for remote agents and the in-memory hub
// pipe arrive in step 6.
package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
)

// LocalOptions configures the in-process local agent.
type LocalOptions struct {
	DataDir  string
	Listen   string // public listen address served by Caddy
	Upstream string // control plane: host:port, or proxy.UnixUpstream(socket)
	Proxy    bool   // start the embedded Caddy
	// ProxyLogLevel is passed to the proxy (empty: WARN); see proxy.Config.LogLevel.
	ProxyLogLevel string
	// Panel, if set, makes the proxy serve the HTTPS panel and redirect Listen to it.
	Panel *proxy.Panel
	// Applier, if set, is the proxy's route applier (the server builds it with the
	// same settings so the proxy_apply op reaches it). RunLocal starts it, which
	// loads the panel alone: app routes come later, through proxy_apply, only
	// after the control plane has seen the containers. When nil, RunLocal builds
	// its own.
	Applier *proxy.Applier
}

// RunLocal runs the local agent until ctx is cancelled. started (optional) is
// called once the proxy, if enabled, has been loaded.
func RunLocal(ctx context.Context, opts LocalOptions, log *slog.Logger, started func()) error {
	if opts.Proxy {
		ap := opts.Applier
		if ap == nil {
			ap = proxy.NewApplier(proxy.Config{DataDir: opts.DataDir, Listen: opts.Listen, Upstream: opts.Upstream, Panel: opts.Panel, LogLevel: opts.ProxyLogLevel})
		}
		if err := ap.Start(); err != nil {
			return fmt.Errorf("local agent: start proxy: %w", err)
		}
		log.Info("proxy listening", "listen", opts.Listen, "upstream", opts.Upstream)
		defer func() {
			if err := ap.Stop(); err != nil {
				log.Error("proxy stop", "err", err)
			}
		}()
	}
	if started != nil {
		started()
	}
	<-ctx.Done()
	return nil
}
