// Package dockerapi is a minimal Docker Engine API client over the unix
// socket. Phase 0 needs logs only. Paths are fixed, container IDs are
// validated, and there is no shell.
package dockerapi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// DefaultSocket is the standard Docker socket path.
const DefaultSocket = "/var/run/docker.sock"

// maxFrame bounds one multiplexed log frame.
const maxFrame = 4 << 20

const (
	// MinAPIVersion is the oldest Engine API we accept: Docker Engine 25
	// (maintainer decision Q7, plan 0002). Older daemons are refused.
	MinAPIVersion = "1.44"
	// MaxAPIVersion is the newest API we speak. The client uses
	// min(daemon API, MaxAPIVersion).
	MaxAPIVersion = "1.52"
	// DefaultCallTimeout bounds one short call (inspect, ping, version).
	DefaultCallTimeout = 30 * time.Second
	// stopMargin is added to stop?t=N so the call outlives the daemon's grace.
	stopMargin  = 15 * time.Second
	dialTimeout = 5 * time.Second
	maxBody     = 4 << 20
)

// Client talks to the Docker Engine over a unix socket. Every request goes
// through call (bounded by a per-call deadline) or stream (bounded only by its
// context); there is no transport-wide ResponseHeaderTimeout, which would cut
// legitimate long calls such as stop?t=N.
type Client struct {
	hc *http.Client

	short time.Duration // deadline for short calls; DefaultCallTimeout, shortened in tests

	mu     sync.Mutex
	prefix string // "/v1.xx" once negotiated; success is cached, failure is not
	apiVer string
}

// New returns a client for the socket at path. The http.Client has no Timeout:
// log streams are long-lived and are cancelled only through their context.
func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: dialTimeout}
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    1,
		IdleConnTimeout: 0,
	}
	return &Client{hc: &http.Client{Transport: tr}, short: DefaultCallTimeout}
}

// Close releases the client's idle connections. In-flight calls are not
// interrupted (cancel their contexts); the client stays usable afterwards.
func (c *Client) Close() { c.hc.CloseIdleConnections() }

// parseAPI parses "major.minor".
func parseAPI(v string) (major, minor int, err error) {
	a, b, ok := strings.Cut(v, ".")
	if !ok {
		return 0, 0, fmt.Errorf("bad API version %q", v)
	}
	if major, err = strconv.Atoi(a); err == nil {
		minor, err = strconv.Atoi(b)
	}
	if err != nil || major < 0 || minor < 0 {
		return 0, 0, fmt.Errorf("bad API version %q", v)
	}
	return major, minor, nil
}

func apiLess(a, b string) bool {
	am, an, _ := parseAPI(a)
	bm, bn, _ := parseAPI(b)
	return am < bm || (am == bm && an < bn)
}

// negotiate returns the "/v<version>" path prefix, pinging the daemon on first
// use. c.mu is never held across the ping, so a caller waits only on its own
// context (two first calls may both ping; that is cheap and harmless). A failed
// or refused negotiation is not cached, so a daemon that comes up or is
// upgraded later is picked up on the next call.
func (c *Client) negotiate(ctx context.Context) (string, error) {
	c.mu.Lock()
	prefix := c.prefix
	c.mu.Unlock()
	if prefix != "" {
		return prefix, nil
	}
	pctx, cancel := context.WithTimeout(ctx, c.short)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("dockerapi: ping: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("dockerapi: ping: status %d", resp.StatusCode)
	}
	server := resp.Header.Get("API-Version")
	if _, _, err := parseAPI(server); err != nil {
		return "", fmt.Errorf("dockerapi: ping: %w", err)
	}
	if apiLess(server, MinAPIVersion) {
		return "", fmt.Errorf("dockerapi: Docker API %s is too old; ServerDock needs Docker Engine 25+ (API %s)", server, MinAPIVersion)
	}
	use := server
	if apiLess(MaxAPIVersion, server) {
		use = MaxAPIVersion
	}
	c.mu.Lock()
	c.apiVer, c.prefix = use, "/v"+use
	prefix = c.prefix
	c.mu.Unlock()
	return prefix, nil
}

// forget drops the cached API version so the next call pings again. It runs on
// connection-level errors (the daemon may have restarted or been upgraded) and
// on a 400 (the daemon no longer accepts the negotiated version).
func (c *Client) forget() {
	c.mu.Lock()
	c.prefix, c.apiVer = "", ""
	c.mu.Unlock()
}

// NegotiatedAPI returns the API version in use, or "" before the first
// successful call.
func (c *Client) NegotiatedAPI() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apiVer
}

// call performs one request bounded by timeout (covering headers and the whole
// body, which is capped at 4 MiB) and returns the status and body.
func (c *Client) call(ctx context.Context, timeout time.Duration, method, path string) (int, []byte, error) {
	prefix, err := c.negotiate(ctx)
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+prefix+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.forget() // not our own deadline or cancel: the daemon may have changed
		}
		return 0, nil, fmt.Errorf("dockerapi: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		c.forget()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, fmt.Errorf("dockerapi: %w", err)
	}
	return resp.StatusCode, body, nil
}

// stream starts a GET whose body lives as long as ctx (log follow; later
// events, pull and build). Only a 200 response is returned. The body is
// unbounded, but the response headers must arrive within the short deadline: a
// daemon that accepts the request and never answers does not hold the caller.
func (c *Client) stream(ctx context.Context, path string) (*http.Response, error) {
	prefix, err := c.negotiate(ctx)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, "http://docker"+prefix+path, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	hdrTimer := time.AfterFunc(c.short, cancel)
	resp, err := c.hc.Do(req)
	timedOut := !hdrTimer.Stop() // true when the timer fired (before or just after headers)
	if err != nil {
		cancel()
		if timedOut && ctx.Err() == nil {
			return nil, fmt.Errorf("dockerapi: no response headers within %v: %w", c.short, context.DeadlineExceeded)
		}
		if ctx.Err() == nil {
			c.forget()
		}
		return nil, fmt.Errorf("dockerapi: %w", err)
	}
	if timedOut && ctx.Err() == nil {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("dockerapi: no response headers within %v: %w", c.short, context.DeadlineExceeded)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusBadRequest {
			c.forget()
		}
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("dockerapi: %s: status %d", path[:min(len(path), 12)], resp.StatusCode)
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelBody releases the stream's context when the body is closed.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// Info reports the engine and the negotiated API version (for the startup log).
func (c *Client) Info(ctx context.Context) (engine, api string, err error) {
	st, body, err := c.call(ctx, c.short, http.MethodGet, "/version")
	if err != nil {
		return "", "", err
	}
	if st != http.StatusOK {
		return "", "", fmt.Errorf("dockerapi: version: status %d", st)
	}
	var v struct{ Version string }
	if err := json.Unmarshal(body, &v); err != nil {
		return "", "", fmt.Errorf("dockerapi: version: %w", err)
	}
	return v.Version, c.NegotiatedAPI(), nil
}

// Stop stops a container, giving it graceSeconds before the daemon kills it.
// The call is bounded by graceSeconds plus a margin, not by the 30 s default.
func (c *Client) Stop(ctx context.Context, id string, graceSeconds int) error {
	if !agent.ValidContainerID(id) {
		return errors.New("dockerapi: invalid container id")
	}
	if graceSeconds < 0 || graceSeconds > 3600 {
		return errors.New("dockerapi: stop grace out of range")
	}
	timeout := time.Duration(graceSeconds)*time.Second + stopMargin
	st, _, err := c.call(ctx, timeout, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop?t="+strconv.Itoa(graceSeconds))
	if err != nil {
		return err
	}
	if st != http.StatusNoContent && st != http.StatusNotModified {
		return fmt.Errorf("dockerapi: stop: status %d", st)
	}
	return nil
}

// Logs streams a container's logs (stdout and stderr, following) into w until
// ctx is cancelled or the container's log stream ends. For non-TTY containers
// the 8-byte-header multiplexed stream is demultiplexed; for TTY containers
// the raw stream is copied.
func (c *Client) Logs(ctx context.Context, id string, tail int, w io.Writer) error {
	if !agent.ValidContainerID(id) {
		return errors.New("dockerapi: invalid container id")
	}
	if tail < agent.MinTail || tail > agent.MaxTail {
		return errors.New("dockerapi: tail out of range")
	}
	esc := url.PathEscape(id)
	var info struct {
		Config struct{ Tty bool }
	}
	st, body, err := c.call(ctx, c.short, http.MethodGet, "/containers/"+esc+"/json")
	if err != nil {
		return err
	}
	if st != http.StatusOK {
		return fmt.Errorf("dockerapi: inspect: status %d", st)
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("dockerapi: inspect: %w", err)
	}
	q := url.Values{"follow": {"1"}, "stdout": {"1"}, "stderr": {"1"}, "tail": {strconv.Itoa(tail)}}
	resp, err := c.stream(ctx, "/containers/"+esc+"/logs?"+q.Encode())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if info.Config.Tty {
		_, err = io.Copy(w, resp.Body)
	} else {
		err = Demux(w, resp.Body)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Demux copies the payloads of a Docker multiplexed stream (8-byte header:
// stream type, 3 zero bytes, big-endian uint32 length) to dst, stdout and
// stderr interleaved.
func Demux(dst io.Writer, src io.Reader) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(src, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil // clean end at a frame boundary
			}
			return err
		}
		if hdr[0] > 2 {
			return fmt.Errorf("dockerapi: bad stream type %d", hdr[0])
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		if n > maxFrame {
			return fmt.Errorf("dockerapi: frame too large (%d)", n)
		}
		if _, err := io.CopyN(dst, src, int64(n)); err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
}
