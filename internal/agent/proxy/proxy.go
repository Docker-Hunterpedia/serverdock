// Package proxy embeds Caddy, owned by the agent runtime (ADR 0004). Only the
// http app and the reverse_proxy handler are linked in (not caddy/modules/standard),
// the admin API is disabled, and configuration is applied in-process with
// caddy.Load.
package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	_ "github.com/caddyserver/caddy/v2/modules/filestorage"
)

// GracePeriod bounds how long Caddy waits for in-flight requests on shutdown
// and reload; it matches the control plane's shutdown grace.
const GracePeriod = 10 * time.Second

// Config describes the proxy: one HTTP listener, everything proxied to the
// control plane. Remote agents do not come through here: the hub serves
// /agent/connect on its own TLS listener, which a Caddy reload cannot touch.
type Config struct {
	// DataDir is the ServerDock data directory; Caddy storage lives in DataDir/caddy.
	DataDir string
	// Listen is the public listen address, for example ":80".
	Listen string
	// Upstream is where requests go: a host:port, or "unix//<absolute path>"
	// (UnixUpstream) for the server's control-plane socket.
	Upstream string
	// LogLevel is Caddy's log level: WARN when empty or not one of DEBUG, INFO,
	// WARN, ERROR. Only the secret-sentinel test sets it (through an environment
	// variable the server reads for no other purpose), to see what Caddy logs.
	LogLevel string
	// Panel, if set (server mode only), serves the panel over HTTPS and turns the
	// Listen address into a redirect-only HTTP server. The remote agent's proxy
	// never sets it, so its config stays byte-identical.
	Panel *Panel
	// Apps are the exposed apps' routes, all in one Caddy server with one listen
	// entry per public port (server mode only: they need Panel). Only the set of
	// ports reaches the config; what each port answers is the Applier's table
	// (app.go). The remote agent never sets it, so its config and the panel
	// servers stay byte-identical.
	Apps []AppRoute
}

// MaxApps bounds Apps (the service cap of plan 0004).
const MaxApps = 100

// Bodies of the static pages an app route can serve.
const (
	BodyNotRunning  = "This app is not running (ServerDock)\n"
	BodyMaintenance = "Deploying, back in a moment\n"
)

// AppRoute is one exposed app: plain HTTP on :Port, either proxied to an
// upstream or answered with a static page. Exactly one of Upstream and Static
// is set.
type AppRoute struct {
	Port int
	// Upstream is "ip:port": an IPv4 literal, never a name or a socket.
	Upstream string
	// Static is the page served when there is no upstream (the serverdock_app
	// handler writes it; it never reaches the reverse_proxy).
	Static *Static
}

// Static is a fixed response.
type Static struct {
	Status     int    // 400..599
	Body       string // no placeholders ("{")
	RetryAfter int    // seconds, 0 = no header
}

// NotRunning is the page for an app with no running container.
func NotRunning() *Static { return &Static{Status: 503, Body: BodyNotRunning} }

// Maintenance is the page served during a stop-first gap.
func Maintenance() *Static { return &Static{Status: 503, Body: BodyMaintenance, RetryAfter: 10} }

// Panel is the HTTPS panel part of the config (ADR 0010).
type Panel struct {
	// Listen is the HTTPS listen address, for example ":443".
	Listen string
	// CertFile is DATA/pki/panel.pem, used as both certificate and key.
	CertFile string
	// Hosts are the --panel-host names. The :80 redirect answers only for an IP
	// literal, "localhost" and these names (the control plane's Host allowlist);
	// any other Host gets 421 instead of a redirect to an attacker-chosen name.
	Hosts []string
}

// SNI is the server name both the default (no SNI) and the fallback (unknown
// SNI) handshakes resolve to. The one panel leaf has it in its SANs, so it is
// served for every handshake on the panel port, IP literals included.
const SNI = "localhost"

// redirectPort is ":<port>" for a port other than 443, else "".
func (p Panel) redirectPort() string {
	_, port, _ := net.SplitHostPort(p.Listen)
	if port == "443" {
		return ""
	}
	return ":" + port
}

// UnixUpstream returns the Upstream value for a unix socket at the absolute path.
func UnixUpstream(path string) string { return "unix/" + path }

func (c Config) validate() error {
	if c.DataDir == "" {
		return fmt.Errorf("proxy: DataDir is required")
	}
	if _, port, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("proxy: Listen %q: %w", c.Listen, err)
	} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("proxy: Listen %q: bad port", c.Listen)
	}
	if c.Panel != nil {
		if _, port, err := net.SplitHostPort(c.Panel.Listen); err != nil {
			return fmt.Errorf("proxy: Panel.Listen %q: %w", c.Panel.Listen, err)
		} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("proxy: Panel.Listen %q: bad port", c.Panel.Listen)
		}
		if !strings.HasPrefix(c.Panel.CertFile, "/") {
			return fmt.Errorf("proxy: Panel.CertFile %q must be an absolute path", c.Panel.CertFile)
		}
		if c.Panel.Listen == c.Listen {
			return fmt.Errorf("proxy: the HTTP and HTTPS listen addresses are the same (%q)", c.Listen)
		}
	}
	if err := c.validateApps(); err != nil {
		return err
	}
	if sock, ok := strings.CutPrefix(c.Upstream, "unix/"); ok {
		if !strings.HasPrefix(sock, "/") {
			return fmt.Errorf("proxy: Upstream %q: a unix socket path must be absolute", c.Upstream)
		}
		return nil
	}
	if _, _, err := net.SplitHostPort(c.Upstream); err != nil {
		return fmt.Errorf("proxy: Upstream %q: %w", c.Upstream, err)
	}
	return nil
}

func (c Config) validateApps() error {
	if len(c.Apps) == 0 {
		return nil
	}
	if c.Panel == nil {
		return fmt.Errorf("proxy: app routes need the panel (server mode)")
	}
	if len(c.Apps) > MaxApps {
		return fmt.Errorf("proxy: %d app routes (at most %d)", len(c.Apps), MaxApps)
	}
	own := map[int]bool{}
	for _, addr := range []string{c.Listen, c.Panel.Listen} {
		_, port, _ := net.SplitHostPort(addr)
		p, _ := strconv.Atoi(port)
		own[p] = true
	}
	seen := map[int]bool{}
	for _, a := range c.Apps {
		switch {
		case a.Port < 1024 || a.Port > 65535:
			return fmt.Errorf("proxy: app port %d is outside 1024..65535", a.Port)
		case own[a.Port]:
			return fmt.Errorf("proxy: app port %d is one of the panel's listen ports", a.Port)
		case seen[a.Port]:
			return fmt.Errorf("proxy: app port %d appears twice", a.Port)
		}
		seen[a.Port] = true
		if (a.Upstream == "") == (a.Static == nil) {
			return fmt.Errorf("proxy: app port %d needs exactly one of an upstream and a static page", a.Port)
		}
		if a.Static != nil {
			s := a.Static
			if s.Status < 400 || s.Status > 599 || s.RetryAfter < 0 || s.RetryAfter > 3600 || len(s.Body) > 1024 || strings.Contains(s.Body, "{") {
				return fmt.Errorf("proxy: app port %d: bad static page", a.Port)
			}
			continue
		}
		host, port, err := net.SplitHostPort(a.Upstream)
		if err != nil {
			return fmt.Errorf("proxy: app port %d: upstream: %w", a.Port, err)
		}
		ip, err := netip.ParseAddr(host)
		if p, perr := strconv.Atoi(port); err != nil || !ip.Is4() || ip.String() != host || perr != nil || p < 1 || p > 65535 || strconv.Itoa(p) != port {
			return fmt.Errorf("proxy: app port %d: the upstream must be an IPv4 address and a port", a.Port)
		}
	}
	return nil
}

// appServer is the one Caddy server of every app route (plan 0004 step 6b):
// plain HTTP on every public port (one listen entry each), HTTP/1.1 only (no
// h2c, no HTTP/3, so no UDP), the panel's public limits. Its route is the
// serverdock_app handler (static pages, and the sd_upstream variable for an
// upstream) followed by the ONE reverse_proxy of all apps, which dials
// {http.vars.sd_upstream}. Nothing else is configured on that reverse_proxy: no
// passive health checks, no max_requests, no retries (lb_try_duration), no
// dynamic_upstreams. The single upstream host entry is shared by every app, so
// a failing app must never be able to mark it down or fill it for the others
// (reverseproxy/hosts.go:93-103: health only counts failures when a health
// policy exists, line 95; hosts.go:106-107: Full only with max_requests;
// reverseproxy.go:1440-1442: tryAgain is false with no try duration and no
// retries). Never follow HTTP_PROXY/HTTPS_PROXY from the environment: an app
// route dials its container directly.
func appServer(apps []AppRoute, limits func(m) m) m {
	listen := make([]string, len(apps))
	for i, a := range apps {
		listen[i] = ":" + strconv.Itoa(a.Port)
	}
	proxy := reverseProxy(upstreamDial, m{"transport": m{"protocol": "http", "network_proxy": m{"from": "none"}}})
	return limits(m{
		"listen":    listen,
		"routes":    []m{{"handle": []m{{"handler": appHandlerName}, proxy}}},
		"protocols": []string{"h1"},
	})
}

type m = map[string]any

func reverseProxy(upstream string, extra m) m {
	h := m{"handler": "reverse_proxy", "upstreams": []m{{"dial": upstream}}}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// BuildConfig returns the Caddy JSON config for c.
func BuildConfig(c Config) ([]byte, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	limits := func(server m) m {
		// Public-facing limits: slow headers, idle keep-alives and header size.
		server["automatic_https"] = m{"disable": true}
		server["read_header_timeout"] = "10s"
		server["idle_timeout"] = "2m"
		server["max_header_bytes"] = 64 << 10
		return server
	}
	httpApp := m{"grace_period": GracePeriod.String()}
	logDefault := m{"level": logLevel(c.LogLevel)}
	if c.Panel != nil {
		// Server mode only: never log the CSRF header (see redact.go).
		registerRedactEncoder()
		logDefault["encoder"] = m{"format": redactEncoderName}
	}
	cfg := m{
		// Admin API off; persist=false so caddy.Load never writes autosave.json
		// outside DataDir.
		"admin":   m{"disabled": true, "config": m{"persist": false}},
		"storage": m{"module": "file_system", "root": filepath.Join(c.DataDir, "caddy")},
		"logging": m{"logs": m{"default": logDefault}},
		"apps":    m{"http": httpApp},
	}
	if c.Panel == nil {
		httpApp["servers"] = m{"main": limits(m{
			"listen": []string{c.Listen},
			"routes": []m{{"handle": []m{reverseProxy(c.Upstream, nil)}}},
		})}
	} else {
		// :80 only redirects; nothing on it reaches the control plane. The port in
		// Location is the one of --https-listen as seen inside this process.
		// {http.request.host} drops the port. For an IPv6 literal it has no brackets
		// when the Host had a port ("[2001:db8::1]:80") and keeps them when it had
		// none ("[2001:db8::1]"), so only the first form needs them put back.
		redirect := func(hostExpr string) m {
			return m{"handler": "static_response", "status_code": 308,
				"headers": m{"Location": []string{"https://" + hostExpr + c.Panel.redirectPort() + "{http.request.uri}"}}}
		}
		host := "{http.request.host}"
		match := func(pattern string) []m {
			return []m{{"vars_regexp": m{host: m{"pattern": pattern}}}}
		}
		redirects := []m{
			// IPv6 literal that had a port: no brackets in the placeholder.
			{"match": match("^[0-9a-fA-F:.]*:[0-9a-fA-F:.]*$"), "handle": []m{redirect("[" + host + "]")}, "terminal": true},
			// IPv4 literal, bracketed IPv6 literal without a port, localhost.
			{"match": match(`(?i)^(localhost|\[[0-9a-f:.]+\]|([0-9]{1,3}\.){3}[0-9]{1,3})$`), "handle": []m{redirect(host)}, "terminal": true},
		}
		if len(c.Panel.Hosts) > 0 {
			redirects = append(redirects, m{"match": []m{{"host": c.Panel.Hosts}}, "handle": []m{redirect(host)}, "terminal": true})
		}
		// Anything else: 421, not a redirect to whatever name the client sent.
		redirects = append(redirects, m{"handle": []m{m{"handler": "static_response", "status_code": 421, "body": "misdirected request\n"}}})
		httpApp["servers"] = m{
			"main": limits(m{
				"listen": []string{c.Listen},
				"routes": redirects,
			}),
			"panel": limits(m{
				"listen": []string{c.Panel.Listen},
				"routes": []m{{"handle": []m{reverseProxy(c.Upstream, nil)}}},
				// HTTP/1.1 and HTTP/2 only: no HTTP/3, so Caddy opens no UDP listener
				// and sends no Alt-Svc header (nothing for a browser to switch to).
				"protocols": []string{"h1", "h2"},
				"tls_connection_policies": []m{{
					"protocol_min": "tls1.2",
					"default_sni":  SNI, // handshakes without SNI (IP literals)
					"fallback_sni": SNI, // SNI that matches no loaded certificate
				}},
			}),
		}
		servers := httpApp["servers"].(m)
		apps := append([]AppRoute(nil), c.Apps...)
		sort.Slice(apps, func(i, j int) bool { return apps[i].Port < apps[j].Port })
		if len(apps) > 0 {
			registerAppHandler()
			servers["apps"] = appServer(apps, limits)
		}
		cfg["apps"].(m)["tls"] = m{
			"certificates":          m{"load_files": []m{{"certificate": c.Panel.CertFile, "key": c.Panel.CertFile, "format": "pem"}}},
			"disable_ocsp_stapling": true,
			// We manage no certificates (the panel leaf is loaded from a file), so the
			// storage clean-up pass has nothing to do and its timer need not exist.
			"disable_storage_clean": true,
		}
	}
	return json.Marshal(cfg)
}

// loadAttempts counts calls of Load. It exists only so tests can count load
// attempts independently of Caddy's log writer (which is pooled per process and
// so unreliable to count from); one atomic add per load, nothing else.
var loadAttempts atomic.Int64

// LoadAttempts is the number of times Load was called in this process (test use).
func LoadAttempts() int64 { return loadAttempts.Load() }

// Load builds and applies the config in-process. It can be called again with
// a changed Config to reload live.
func Load(c Config) error {
	loadAttempts.Add(1)
	b, err := BuildConfig(c)
	if err != nil {
		return err
	}
	return caddy.Load(b, true)
}

// Stop shuts the embedded Caddy down.
func Stop() error { return caddy.Stop() }

func logLevel(l string) string {
	switch l {
	case "DEBUG", "INFO", "WARN", "ERROR":
		return l
	}
	return "WARN"
}
