package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

const (
	// WriteTimeout bounds every write to a stream.
	WriteTimeout  = 10 * time.Second
	opReadTimeout = 10 * time.Second
	// DefaultKeepAlive is the yamux keepalive interval.
	DefaultKeepAlive = 30 * time.Second
)

// MaxAgentStreams bounds the log streams one agent serves at once; a stream
// opened beyond it is closed immediately.
const MaxAgentStreams = 32

// YamuxConfig is the session config both ends use: a small accept backlog and
// bounded open/close timeouts so a stuck peer cannot pin streams for minutes.
//
// MaxStreamWindowSize stays at yamux's default (256 KiB): yamux rejects any
// smaller value because every stream starts with a 256 KiB window, so the plan's
// 64 KiB bound is not expressible with this library. The memory a peer can pin
// is bounded instead by the stream caps (docs/security.md).
func YamuxConfig(keepAlive time.Duration) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.AcceptBacklog = 8
	cfg.StreamOpenTimeout = 10 * time.Second
	cfg.StreamCloseTimeout = 10 * time.Second
	// Each answered ping holds a goroutine for up to this long (see GuardPings).
	cfg.ConnectionWriteTimeout = 5 * time.Second
	cfg.EnableKeepAlive = keepAlive > 0
	if keepAlive > 0 {
		cfg.KeepAliveInterval = keepAlive
	}
	return cfg
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// LogSource streams container logs into w until ctx is cancelled or the
// stream ends.
type LogSource interface {
	Logs(ctx context.Context, container string, tail int, w io.Writer) error
}

// ServeSession runs the agent side of a connection: a yamux server that
// accepts streams from the control plane. keepAlive <= 0 disables keepalives
// (used for the in-memory local pipe). It returns when the session ends.
func ServeSession(ctx context.Context, conn net.Conn, logs LogSource, keepAlive time.Duration, log *slog.Logger) error {
	sess, err := yamux.Server(GuardPings(conn), YamuxConfig(keepAlive))
	if err != nil {
		return fmt.Errorf("yamux server: %w", err)
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	sem := make(chan struct{}, MaxAgentStreams)
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, yamux.ErrSessionShutdown) {
				return nil
			}
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			// Over the per-session stream cap. The only peer is the hub, which
			// reads: say why in one short frame (1 s write bound, so a stuck
			// peer cannot stall the accept loop), then close.
			_ = st.SetWriteDeadline(time.Now().Add(time.Second))
			_, _ = st.Write([]byte("serverdock-agent: too many open streams\n"))
			st.Close()
			continue
		}
		go func() {
			defer func() { <-sem }()
			handleStream(ctx, st, logs, log)
		}()
	}
}

// handleStream implements the stream protocol with yamux half-close handling:
// stream.Close() from the server only sends FIN, so a reader goroutine watches
// for EOF/error and cancels the request; failed or timed-out writes cancel too.
func handleStream(ctx context.Context, st *yamux.Stream, logs LogSource, log *slog.Logger) {
	defer st.Close()
	_ = st.SetReadDeadline(time.Now().Add(opReadTimeout))
	br := bufio.NewReaderSize(st, MaxOpLine)
	line, err := br.ReadSlice('\n')
	if err != nil {
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	op, err := ParseOp(line)
	if err != nil {
		writeLine(st, "serverdock-agent: "+err.Error())
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, br) // returns on peer FIN/RST or local Close
		cancel()
	}()
	w := &deadlineWriter{st: st, cancel: cancel}
	if err := logs.Logs(cctx, op.Container, op.Tail, w); err != nil && cctx.Err() == nil {
		log.Warn("log stream failed", "container", op.Container, "err", err)
		writeLine(st, "serverdock-agent: log stream failed")
	}
}

func writeLine(st *yamux.Stream, s string) {
	_ = st.SetWriteDeadline(time.Now().Add(WriteTimeout))
	_, _ = st.Write([]byte(s + "\n"))
}

type deadlineWriter struct {
	st     *yamux.Stream
	cancel context.CancelFunc
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	_ = d.st.SetWriteDeadline(time.Now().Add(WriteTimeout))
	n, err := d.st.Write(p)
	if err != nil {
		d.cancel()
	}
	return n, err
}
