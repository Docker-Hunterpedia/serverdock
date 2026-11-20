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

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	_ "github.com/caddyserver/caddy/v2/modules/filestorage"
)

// AgentConnectPath is the WebSocket endpoint remote agents dial.
const AgentConnectPath = "/agent/connect"

// Config describes the Phase 0 proxy: one HTTP listener, everything proxied
// to the control plane.
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
		"admin":   m{"disabled": true},
		"storage": m{"module": "file_system", "root": filepath.Join(c.DataDir, "caddy")},
		"logging": m{"logs": m{"default": m{"level": "WARN"}}},
		"apps": m{"http": m{"servers": m{"main": m{
			"listen":          []string{c.Listen},
			"automatic_https": m{"disable": true},
			"routes": []m{
				{
					"match":  []m{{"path": []string{AgentConnectPath}}},
					"handle": []m{reverseProxy(c.Upstream, m{"flush_interval": -1})},
				},
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
