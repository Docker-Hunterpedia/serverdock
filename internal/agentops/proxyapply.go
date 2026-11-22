package agentops

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// RouteApplier is what proxy_apply drives: *proxy.Applier. Apply returns the
// ports that could not be bound; an error means the routes were not (all)
// applied and Caddy still serves what it served before.
type RouteApplier interface {
	Apply(ctx context.Context, routes []proxy.AppRoute) (failedPorts []int, err error)
}

// cgnat is 100.64.0.0/10, the other range --app-subnet may use.
var cgnat = sync.OnceValue(func() netip.Prefix { return netip.MustParsePrefix("100.64.0.0/10") })

// appRoutes validates a proxy_apply request and converts it. The agent does not
// trust the list: a route may only point at a private IPv4 address (the app
// network is RFC 1918 or 100.64.0.0/10), never at loopback or link-local, so no
// app server can reach the control plane's own listeners. The public ports are
// checked again here (1024..65535, never 80, 443 or ServerDock's own), and the
// proxy refuses duplicates and the panel's ports.
func (h *handlers) appRoutes(a ApplyArgs) ([]proxy.AppRoute, error) {
	if len(a.Routes) > MaxRoutes {
		return nil, opErr(agent.CodeInvalid, "at most %d routes", MaxRoutes)
	}
	out := make([]proxy.AppRoute, 0, len(a.Routes))
	seen := map[int]bool{}
	for _, r := range a.Routes {
		if err := policy.ValidatePublicPort(r.Port, h.ReservedPorts...); err != nil {
			return nil, opErr(agent.CodeInvalid, "route port %d: %v", r.Port, err)
		}
		if seen[r.Port] {
			return nil, opErr(agent.CodeInvalid, "route port %d appears twice", r.Port)
		}
		seen[r.Port] = true
		if (r.Upstream == "") == (r.Page == "") {
			return nil, opErr(agent.CodeInvalid, "route port %d needs exactly one of an upstream and a page", r.Port)
		}
		switch r.Page {
		case "":
			host, port, err := net.SplitHostPort(r.Upstream)
			ip, ipErr := netip.ParseAddr(host)
			p, pErr := strconv.Atoi(port)
			if err != nil || ipErr != nil || pErr != nil || !ip.Is4() || ip.String() != host || p < 1 || p > 65535 || strconv.Itoa(p) != port {
				return nil, opErr(agent.CodeInvalid, "route port %d: the upstream must be an IPv4 address and a port", r.Port)
			}
			if !(ip.IsPrivate() || cgnat().Contains(ip)) {
				return nil, opErr(agent.CodeInvalid, "route port %d: the upstream is not on a private network", r.Port)
			}
			out = append(out, proxy.AppRoute{Port: r.Port, Upstream: r.Upstream})
		case PageNotRunning:
			out = append(out, proxy.AppRoute{Port: r.Port, Static: proxy.NotRunning()})
		case PageMaintenance:
			out = append(out, proxy.AppRoute{Port: r.Port, Static: proxy.Maintenance()})
		default:
			return nil, opErr(agent.CodeInvalid, "route port %d: unknown page", r.Port)
		}
	}
	return out, nil
}

func (h *handlers) proxyApply(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	var a ApplyArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	routes, err := h.appRoutes(a)
	if err != nil {
		return nil, err
	}
	if h.Proxy == nil {
		return nil, opErr(agent.CodeConflict, "the proxy is not running on this server")
	}
	failed, err := h.Proxy.Apply(ctx, routes)
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		return nil, opErr(agent.CodeTimeout, "timed out")
	case errors.Is(err, context.Canceled):
		return nil, opErr(agent.CodeCancelled, "cancelled")
	case errors.Is(err, proxy.ErrNotStarted):
		return nil, opErr(agent.CodeConflict, "the proxy is not running on this server")
	default:
		// Caddy's message can name paths and sockets: keep it in the agent's log.
		h.Log.Warn("proxy apply failed", "err", err)
		return nil, opErr(agent.CodeConflict, "the proxy could not apply the routes; the previous routes keep running")
	}
	sort.Ints(failed)
	if failed == nil {
		failed = []int{}
	}
	return ApplyResult{Applied: true, FailedPorts: failed}, nil
}
