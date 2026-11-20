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
	// Upstream is the control plane's host:port.
	Upstream string
}

func (c Config) validate() error {
	if c.DataDir == "" {
		return fmt.Errorf("proxy: DataDir is required")
	}
	if _, port, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("proxy: Listen %q: %w", c.Listen, err)
	} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("proxy: Listen %q: bad port", c.Listen)
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
	cfg := m{
		// Admin API off; persist=false so caddy.Load never writes autosave.json
		// outside DataDir.
		"admin":   m{"disabled": true, "config": m{"persist": false}},
		"storage": m{"module": "file_system", "root": filepath.Join(c.DataDir, "caddy")},
		"logging": m{"logs": m{"default": m{"level": "WARN"}}},
		"apps": m{"http": m{"grace_period": GracePeriod.String(), "servers": m{"main": m{
			"listen":          []string{c.Listen},
			"automatic_https": m{"disable": true},
			// Public-facing limits: slow headers, idle keep-alives and header size.
			"read_header_timeout": "10s",
			"idle_timeout":        "2m",
			"max_header_bytes":    64 << 10,
			"routes": []m{
				{"handle": []m{reverseProxy(c.Upstream, nil)}},
			},
		}}}},
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
