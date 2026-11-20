// Package proxy embeds Caddy, owned by the agent runtime (ADR 0004). Only the
// http app and the reverse_proxy handler are linked in (not caddy/modules/standard),
// the admin API is disabled, and configuration is applied in-process with
// caddy.Load.
package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
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
	// Panel, if set (server mode only), serves the panel over HTTPS and turns the
	// Listen address into a redirect-only HTTP server. The remote agent's proxy
	// never sets it, so its config stays byte-identical.
	Panel *Panel
}

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
	cfg := m{
		// Admin API off; persist=false so caddy.Load never writes autosave.json
		// outside DataDir.
		"admin":   m{"disabled": true, "config": m{"persist": false}},
		"storage": m{"module": "file_system", "root": filepath.Join(c.DataDir, "caddy")},
		"logging": m{"logs": m{"default": m{"level": "WARN"}}},
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
				"tls_connection_policies": []m{{
					"protocol_min": "tls1.2",
					"default_sni":  SNI, // handshakes without SNI (IP literals)
					"fallback_sni": SNI, // SNI that matches no loaded certificate
				}},
			}),
		}
		cfg["apps"].(m)["tls"] = m{
			"certificates":          m{"load_files": []m{{"certificate": c.Panel.CertFile, "key": c.Panel.CertFile, "format": "pem"}}},
			"disable_ocsp_stapling": true,
		}
	}
	return json.Marshal(cfg)
}

// Load builds and applies the config in-process. It can be called again with
// a changed Config to reload live.
func Load(c Config) error {
	b, err := BuildConfig(c)
	if err != nil {
		return err
	}
	return caddy.Load(b, true)
}

// Stop shuts the embedded Caddy down.
func Stop() error { return caddy.Stop() }
