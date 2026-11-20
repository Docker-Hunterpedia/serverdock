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
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.EnableKeepAlive = keepAlive > 0
	if keepAlive > 0 {
		cfg.KeepAliveInterval = keepAlive
	}
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		return fmt.Errorf("yamux server: %w", err)
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, yamux.ErrSessionShutdown) {
				return nil
			}
			return err
		}
		go handleStream(ctx, st, logs, log)
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
