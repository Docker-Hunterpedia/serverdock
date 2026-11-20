package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// ClientConfig configures the dial-out client of a remote agent.
type ClientConfig struct {
	// URL is ws://host/agent/connect (wss:// once TLS exists).
	URL string
	// Token is the shared spike token. It is never logged.
	Token string
	// ID names this agent; sent as ?id=.
	ID   string
	Logs LogSource
	Log  *slog.Logger

	Backoff     Backoff
	DialTimeout time.Duration // handshake only
	KeepAlive   time.Duration
	// ResetAfter: backoff resets when a connection lasted at least this long.
	ResetAfter time.Duration

	// Now and Sleep are injectable for tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// OnConnect, if set, is called after each successful handshake.
	OnConnect func()
}

// ValidateURL checks the dial URL scheme.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return fmt.Errorf("agent url must be ws:// or wss:// with a host")
	}
	return nil
}

func (c *ClientConfig) defaults() {
	if c.Backoff.Min == 0 {
		c.Backoff = DefaultBackoff()
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 15 * time.Second
	}
	if c.KeepAlive == 0 {
		c.KeepAlive = DefaultKeepAlive
	}
	if c.ResetAfter == 0 {
		c.ResetAfter = 60 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = sleepCtx
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RunClient connects, serves streams, and reconnects with backoff until ctx is
// cancelled. It returns nil on cancellation.
func RunClient(ctx context.Context, cfg ClientConfig) error {
	if err := ValidateURL(cfg.URL); err != nil {
		return err
	}
	cfg.defaults()
	for ctx.Err() == nil {
		connectedAt, err := runOnce(ctx, &cfg)
		if ctx.Err() != nil {
			return nil
		}
		if !connectedAt.IsZero() && cfg.Now().Sub(connectedAt) >= cfg.ResetAfter {
			cfg.Backoff.Reset()
		}
		d := cfg.Backoff.Next()
		cfg.Log.Warn("agent disconnected; will retry", "after", d.String(), "err", errString(err))
		if err := cfg.Sleep(ctx, d); err != nil {
			return nil
		}
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return "session ended"
	}
	return err.Error()
}

// runOnce makes one connection. connectedAt is zero if the handshake failed.
func runOnce(ctx context.Context, cfg *ClientConfig) (connectedAt time.Time, err error) {
	// The dial context only bounds the handshake.
	dctx, dcancel := context.WithTimeout(ctx, cfg.DialTimeout)
	u, _ := url.Parse(cfg.URL)
	q := u.Query()
	q.Set("id", cfg.ID)
	u.RawQuery = q.Encode()
	c, _, err := websocket.Dial(dctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.Token}},
	})
	dcancel()
	if err != nil {
		return time.Time{}, redact(err, cfg.Token)
	}
	defer c.CloseNow()
	connectedAt = cfg.Now()
	if cfg.OnConnect != nil {
		cfg.OnConnect()
	}
	// The session context lives until the connection ends; NetConn is bound to it.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	nc := websocket.NetConn(sctx, c, websocket.MessageBinary)
	return connectedAt, ServeSession(sctx, nc, cfg.Logs, cfg.KeepAlive, cfg.Log)
}

func redact(err error, token string) error {
	if token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[redacted]"))
}
