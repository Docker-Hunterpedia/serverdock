package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
)

// ConnectPath is the WebSocket endpoint the hub serves and agents dial.
const ConnectPath = "/agent/connect"

// ClientConfig configures the dial-out client of a remote agent.
type ClientConfig struct {
	// URL is wss://host:port/agent/connect, or ws:// for loopback literals only.
	URL string
	// CAFingerprint is "sha256:<hex>" of the hub CA; required for wss.
	CAFingerprint string
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
	Now        func() time.Time
	Sleep      func(ctx context.Context, d time.Duration) error
	httpClient *http.Client

	// OnConnect, if set, is called after each successful handshake.
	OnConnect func()
}

// ValidateURL checks the dial URL: wss:// with any host, or ws:// only for a
// loopback IP literal (127.0.0.0/8, ::1). "localhost" is not accepted: it is a
// name, resolved by whoever controls /etc/hosts or DNS.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return fmt.Errorf("agent url must be ws:// or wss:// with a host")
	}
	if u.Scheme == "ws" && !loopbackHost(u.Hostname()) {
		return fmt.Errorf("ws:// is only allowed for loopback addresses; use wss:// with --ca-fingerprint")
	}
	return nil
}

func loopbackHost(h string) bool {
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Validate checks the URL and, for wss, the CA fingerprint.
func (c *ClientConfig) Validate() error {
	if err := ValidateURL(c.URL); err != nil {
		return err
	}
	if u, _ := url.Parse(c.URL); u.Scheme == "wss" {
		if c.CAFingerprint == "" {
			return errors.New("wss:// requires --ca-fingerprint sha256:<hex> (printed by the server at startup)")
		}
		if _, err := pki.ParseFingerprint(c.CAFingerprint); err != nil {
			return fmt.Errorf("--ca-fingerprint: %w", err)
		}
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
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.defaults()
	hc, err := httpClient(&cfg)
	if err != nil {
		return err
	}
	defer hc.CloseIdleConnections()
	cfg.httpClient = hc
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
	c, resp, err := websocket.Dial(dctx, u.String(), &websocket.DialOptions{
		HTTPClient: cfg.httpClient,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.Token}},
	})
	dcancel()
	if err != nil {
		return time.Time{}, redact(explainDialError(err, resp), cfg.Token)
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

// httpClient returns the client used for the WebSocket handshake. For wss it
// pins the hub CA; the system roots are never consulted.
func httpClient(cfg *ClientConfig) (*http.Client, error) {
	u, _ := url.Parse(cfg.URL)
	tr := &http.Transport{DisableKeepAlives: true}
	if u.Scheme == "wss" {
		tc, err := pki.ClientTLSConfig(u.Hostname(), cfg.CAFingerprint, cfg.Now)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = tc
	}
	return &http.Client{Transport: tr}, nil
}

// explainDialError turns the two failures an operator can fix into one clear
// line each; anything else is returned unchanged.
func explainDialError(err error, resp *http.Response) error {
	switch {
	case resp != nil && resp.StatusCode == http.StatusUnauthorized:
		return errors.New("the hub rejected the agent token (401): check SD_AGENT_TOKEN")
	case resp != nil && resp.StatusCode == http.StatusTooManyRequests:
		return errors.New("the hub is rate limiting this address (429): too many failed attempts; check SD_AGENT_TOKEN")
	case resp != nil && resp.StatusCode == http.StatusServiceUnavailable:
		return errors.New("the hub refused the connection (503): it is at its agent session limit or has remote agents disabled")
	case errors.Is(err, pki.ErrVerify):
		return fmt.Errorf("hub certificate not trusted: check --ca-fingerprint and that --server matches one of the hub's --agent-san names (%w)", err)
	}
	return err
}

func redact(err error, token string) error {
	if token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[redacted]"))
}
