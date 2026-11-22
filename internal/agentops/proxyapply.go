package agentops

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"sort"
	"strconv"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// RouteApplier is what proxy_apply drives: *proxy.Applier. Apply returns the
// ports that could not be bound; an error means the routes were not (all)
// applied and Caddy still serves what it served before.
type RouteApplier interface {
	Apply(ctx context.Context, routes []proxy.AppRoute) (failedPorts []int, err error)
}

// appRoutes validates a proxy_apply request and converts it. The agent does not
// trust the list. The public ports are checked again here (1024..65535, never
// 80, 443 or ServerDock's own), duplicates are refused, and an upstream must be
// an IPv4 literal and a port. That an upstream is a host address of our app
// network's static range (never loopback, the gateway, the network or broadcast
// address, the dynamic range, or another network) is checked by proxyApply
// against the network itself, when any route has an upstream.
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
	// Only a list with an upstream needs the network (a list of static pages works
	// before it exists): every upstream must be a host address of our static range.
	var net_ *dockerapi.NetworkInfo
	for _, r := range a.Routes {
		if r.Upstream == "" {
			continue
		}
		if net_ == nil {
			n, err := h.ourNetwork(ctx, agent.OpProxyApply)
			if err != nil {
				return nil, err
			}
			net_ = &n
		}
		host, _, _ := net.SplitHostPort(r.Upstream) // shape checked by appRoutes
		if err := StaticAddr(*net_, netip.MustParseAddr(host)); err != nil {
			return nil, opErr(agent.CodePolicy, "route port %d: the upstream is not a host address of the app network's static range", r.Port)
		}
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
