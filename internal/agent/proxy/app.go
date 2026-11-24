package proxy

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Plan 0004 step 6b, ADR 0004 addendum A: every app port is a listener of ONE
// Caddy server whose route is this handler followed by one reverse_proxy that
// dials {http.vars.sd_upstream}. What a request gets is decided here, from an
// immutable table keyed by the local port it arrived on, so a deploy switch is
// one atomic pointer store and no Caddy reload.

// appHandlerName is the module name; its ID is http.handlers.serverdock_app.
const (
	appHandlerName = "serverdock_app"
	// upstreamVar is the request variable the reverse_proxy dials. Only server
	// side code can set an http.vars entry: the map is created per request in
	// caddyhttp.Server.ServeHTTP (server.go:1412, VarsCtxKey) and no client input
	// is ever copied into it.
	upstreamVar = "sd_upstream"
	// upstreamDial is the placeholder form of the variable, replaced per request
	// (reverseproxy/hosts.go:117, fillDialInfo).
	upstreamDial = "{http.vars." + upstreamVar + "}"
)

// appEntry is one port's answer: a static page (upstream == "") or an upstream.
type appEntry struct {
	status     int
	body       string
	retryAfter string // "" = no header
	upstream   string // "ip:port"
}

// appTable maps a public port to its entry. A table is never modified after it
// is stored.
type appTable map[int]appEntry

// table is the only package-level state besides the module registration: the
// table the handler reads. The zero value (nil) means "no app routes".
//
// Exactly one Applier may write it per process, and it does: Caddy itself is a
// process singleton (caddy.Load replaces the one running config), so a second
// Applier would replace the first one's config as well. Only Applier.Start,
// Stop and the commit after a successful load store it. Server mode only; the
// remote agent never builds an Applier with a panel, so it never stores it and
// its config (golden test) carries no app server.
var table atomic.Pointer[appTable]

func newTable(routes []AppRoute) *appTable {
	t := make(appTable, len(routes))
	for _, r := range routes {
		if r.Static != nil {
			e := appEntry{status: r.Static.Status, body: r.Static.Body}
			if r.Static.RetryAfter > 0 {
				e.retryAfter = strconv.Itoa(r.Static.RetryAfter)
			}
			t[r.Port] = e
		} else {
			t[r.Port] = appEntry{upstream: r.Upstream}
		}
	}
	return &t
}

var registerApp sync.Once

// registerAppHandler registers the module on first use, not at package load
// (the remote agent never needs it); the same path as the redact encoder.
func registerAppHandler() { registerApp.Do(func() { caddy.RegisterModule(Handler{}) }) }

// Handler is the http.handlers.serverdock_app module.
type Handler struct{}

// CaddyModule implements caddy.Module.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers." + appHandlerName, New: func() caddy.Module { return new(Handler) }}
}

// ServeHTTP answers a static entry itself and never calls next for it. For an
// upstream entry it sets sd_upstream and calls next (the reverse_proxy). It
// never calls next without setting the variable: an unset one expands to ""
// and Caddy would fail the dial by accident, which this code does not rely on.
func (Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	e := lookup(table.Load(), localPort(r)) // the table is loaded once per request
	if e.upstream == "" {
		h := w.Header()
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		if e.retryAfter != "" {
			h.Set("Retry-After", e.retryAfter)
		}
		w.WriteHeader(e.status)
		_, _ = w.Write([]byte(e.body))
		return nil
	}
	caddyhttp.SetVar(r.Context(), upstreamVar, e.upstream)
	return next.ServeHTTP(w, r)
}

// lookup returns the entry for port; an unknown port or no table is "not running".
func lookup(t *appTable, port int) appEntry {
	if t != nil {
		if e, ok := (*t)[port]; ok {
			return e
		}
	}
	s := NotRunning()
	return appEntry{status: s.Status, body: s.Body}
}

// localPort is the port the connection arrived on (http.LocalAddrContextKey is
// set by net/http for every connection). 0 when it cannot be read.
func localPort(r *http.Request) int {
	switch a := r.Context().Value(http.LocalAddrContextKey).(type) {
	case *net.TCPAddr:
		return a.Port
	case net.Addr:
		if _, p, err := net.SplitHostPort(a.String()); err == nil {
			n, _ := strconv.Atoi(p)
			return n
		}
	}
	return 0
}

var _ caddyhttp.MiddlewareHandler = Handler{}
