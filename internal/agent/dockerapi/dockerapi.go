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

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// DefaultSocket is the standard Docker socket path.
const DefaultSocket = "/var/run/docker.sock"

// maxFrame bounds one multiplexed log frame.
const maxFrame = 4 << 20

// Client talks to the Docker Engine over a unix socket.
type Client struct {
	hc *http.Client
}

// New returns a client for the socket at path. The http.Client has no Timeout:
// log streams are long-lived and are cancelled only through their context.
func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    1,
		IdleConnTimeout: 0,
	}
	return &Client{hc: &http.Client{Transport: tr}}
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
	resp, err := c.get(ctx, "/containers/"+esc+"/json")
	if err != nil {
		return err
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&info)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("dockerapi: inspect: %w", err)
	}
	q := url.Values{"follow": {"1"}, "stdout": {"1"}, "stderr": {"1"}, "tail": {strconv.Itoa(tail)}}
	resp, err = c.get(ctx, "/containers/"+esc+"/logs?"+q.Encode())
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

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dockerapi: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("dockerapi: %s: status %d", path[:min(len(path), 12)], resp.StatusCode)
	}
	return resp, nil
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
