package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
	"unicode/utf8"

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
	cfg.EnableKeepAlive = keepAlive > 0
	if keepAlive > 0 {
		cfg.KeepAliveInterval = keepAlive
	}
	return cfg
}

// LogSource streams container logs into w until ctx is cancelled or the
// stream ends.
type LogSource interface {
	Logs(ctx context.Context, container string, tail int, w io.Writer) error
}

// FollowLogSource is a LogSource that can end the stream at the tail. An agent
// asked for follow=false needs one; without it the request is refused.
type FollowLogSource interface {
	LogsFollow(ctx context.Context, container string, tail int, follow bool, w io.Writer) error
}

// Mode says which ops a serving agent builds its handler table from.
type Mode int

const (
	// ModeRemote (the zero value, so forgetting it fails safe) serves only
	// RemoteOK ops: `serverdock agent`.
	ModeRemote Mode = iota
	// ModeLocal also serves local-only ops: the in-process local agent.
	ModeLocal
)

// Handler runs one v1 op. It reports progress or events through out and
// returns the result value (marshalled into the result line) or an error; an
// *OpError keeps its code, anything else becomes code "docker" with a generic
// message (internal errors are not sent to the hub).
type Handler func(ctx context.Context, args json.RawMessage, out *Out) (any, error)

// Config is what an agent serves. Logs is required.
type Config struct {
	Logs LogSource
	// Version is reported by `hello` (cleaned, at most MaxAgentVersion bytes).
	Version string
	Mode    Mode

	// specs and impls replace the op table and the v1 handlers; tests only.
	specs []OpSpec
	impls map[string]Handler
}

// handlerTable builds the v1 handlers this mode serves. A handler whose spec is
// not RemoteOK is left out of a remote table, so the agent process itself
// refuses it (two-sided LocalOnly; the hub refuses it first).
func (c Config) handlerTable() map[string]opEntry {
	specs := c.specs
	if specs == nil {
		specs = opSpecs
	}
	impls := c.impls
	if impls == nil {
		impls = map[string]Handler{OpHello: c.hello}
	}
	table := map[string]opEntry{}
	for _, sp := range specs {
		h, ok := impls[sp.Name]
		if !ok || sp.Name == OpLogs {
			continue
		}
		if !sp.RemoteOK && c.Mode != ModeLocal {
			continue
		}
		table[sp.Name] = opEntry{spec: sp, h: h}
	}
	return table
}

// opEntry is what this agent process serves for one op: the single source of
// truth for both request parsing and dispatch.
type opEntry struct {
	spec OpSpec
	h    Handler
}

func (t opTable) lookup(name string) (OpSpec, bool) {
	e, ok := t[name]
	return e.spec, ok
}

type opTable map[string]opEntry

func (c Config) hello(_ context.Context, args json.RawMessage, _ *Out) (any, error) {
	var a HelloArgs
	if err := DecodeArgs(args, &a); err != nil {
		return nil, &OpError{Code: CodeInvalid, Msg: err.Error()}
	}
	if a.HubMaxV < 1 || a.HubMaxV > 1000 {
		return nil, &OpError{Code: CodeInvalid, Msg: "hub_max_v out of range"}
	}
	// JSON escaping can grow the version string (< becomes \u003c): shorten it
	// until the whole result fits MaxHelloResult.
	r := HelloResult{V: min(a.HubMaxV, ProtocolVersion), Agent: CleanText(c.Version, MaxAgentVersion)}
	for {
		b, err := json.Marshal(r)
		if err != nil || len(b) <= MaxHelloResult || r.Agent == "" {
			return r, err
		}
		r.Agent = cutRune(r.Agent, len(r.Agent)/2)
	}
}

// Serve runs the agent side of a connection: a yamux server that accepts
// streams from the control plane. keepAlive <= 0 disables keepalives (used for
// the in-memory local pipe). It returns when the session ends.
func Serve(ctx context.Context, conn net.Conn, cfg Config, keepAlive time.Duration, log *slog.Logger) error {
	sess, err := yamux.Server(GuardPings(conn), YamuxConfig(keepAlive))
	if err != nil {
		return fmt.Errorf("yamux server: %w", err)
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	table := opTable(cfg.handlerTable())
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
			handleStream(ctx, st, cfg, table, log)
		}()
	}
}

// handleStream implements the stream protocol with yamux half-close handling:
// stream.Close() from the server only sends FIN, so a reader goroutine watches
// for EOF/error and cancels the request; failed or timed-out writes cancel too.
func handleStream(ctx context.Context, st *yamux.Stream, cfg Config, table opTable, log *slog.Logger) {
	defer st.Close()
	_ = st.SetReadDeadline(time.Now().Add(opReadTimeout))
	br := bufio.NewReaderSize(st, 4096)
	line, err := ReadLine(br, MaxRequestLine)
	if err != nil {
		if errors.Is(err, ErrLineTooLong) {
			// Too long for either protocol: a v1-style reply is the only
			// well-formed one, and a v0 hub ignores it.
			writeResult(st, nil, &OpError{Code: CodeInvalid, Msg: "request line too long"})
		}
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	startWatch := func() {
		go func() {
			_, _ = io.Copy(io.Discard, br) // returns on peer FIN/RST or local Close
			cancel()
		}()
	}
	if IsV1Request(line) {
		serveV1(cctx, st, line, table, startWatch, cancel, log)
		return
	}
	op, err := ParseOp(line)
	if err != nil {
		writeLine(st, "serverdock-agent: "+err.Error())
		return
	}
	startWatch()
	w := &deadlineWriter{st: st, cancel: cancel}
	var lerr error
	if !op.Following() {
		fl, ok := cfg.Logs.(FollowLogSource)
		if !ok {
			writeLine(st, "serverdock-agent: follow=false is not supported by this log source")
			return
		}
		lerr = fl.LogsFollow(cctx, op.Container, op.Tail, false, w)
	} else {
		lerr = cfg.Logs.Logs(cctx, op.Container, op.Tail, w)
	}
	if lerr != nil && cctx.Err() == nil {
		log.Warn("log stream failed", "container", op.Container, "err", lerr)
		writeLine(st, "serverdock-agent: log stream failed")
	}
}

// serveV1 answers one v1 request: strict parse, handler lookup in this mode's
// table, the op's timeout, and exactly one result line.
func serveV1(ctx context.Context, st *yamux.Stream, line []byte, table opTable, startWatch func(), cancel context.CancelFunc, log *slog.Logger) {
	// The table is what this process serves: a local-only op on a remote agent
	// is not in it, so it is refused exactly like an unknown op.
	req, err := parseV1Request(line, table.lookup)
	if err != nil {
		writeResult(st, nil, &OpError{Code: CodeInvalid, Msg: err.Error()})
		return
	}
	entry := table[req.Op]
	h, spec := entry.h, entry.spec
	startWatch()
	if spec.Timeout > 0 {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(ctx, spec.Timeout)
		defer c()
	}
	out := &Out{st: st, spec: spec, cancel: cancel}
	res, err := h(ctx, req.Args, out)
	if err != nil {
		var oe *OpError
		if !errors.As(err, &oe) {
			log.Warn("op failed", "op", req.Op, "err", err)
			oe = &OpError{Code: CodeDocker, Msg: "operation failed"}
		}
		writeResult(st, nil, oe)
		return
	}
	b, err := json.Marshal(res)
	if err != nil || len(b) > spec.maxResult() {
		log.Warn("op result unusable", "op", req.Op, "size", len(b))
		writeResult(st, nil, &OpError{Code: CodeInvalid, Msg: "result too large"})
		return
	}
	writeResult(st, b, nil)
}

// Out lets a v1 handler send progress and event lines.
type Out struct {
	mu       sync.Mutex // handlers may report progress from several goroutines
	st       *yamux.Stream
	spec     OpSpec
	cancel   context.CancelFunc
	progress int
}

// ErrOutLimit is returned by Out when a line would break the protocol caps.
var ErrOutLimit = errors.New("agent: progress line over the op's limits")

// Progress sends one progress line (at most MaxProgressLine bytes, at most the
// op's MaxProgressLines lines).
func (o *Out) Progress(p any) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.progress >= o.spec.MaxProgressLines {
		return ErrOutLimit
	}
	o.progress++
	return o.send(Line{T: LineProgress}, p, true)
}

// Event sends one event line (events op only).
func (o *Out) Event(e any) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.spec.Name != OpEvents {
		return ErrOutLimit
	}
	return o.send(Line{T: LineEvent}, e, false)
}

func (o *Out) send(l Line, v any, progress bool) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if progress {
		l.P = b
	} else {
		l.E = b
	}
	lb, err := json.Marshal(l)
	if err != nil || len(lb) > MaxProgressLine {
		return ErrOutLimit
	}
	_ = o.st.SetWriteDeadline(time.Now().Add(WriteTimeout))
	if _, err := o.st.Write(append(lb, '\n')); err != nil {
		o.cancel()
		return err
	}
	return nil
}

// writeResult writes the result line: ok with payload r, or the error.
func writeResult(st *yamux.Stream, r json.RawMessage, e *OpError) {
	l := Line{T: LineResult}
	ok := e == nil
	l.OK = &ok
	if ok {
		l.R = r
	} else {
		l.Err = &OpError{Code: e.Code, Msg: CleanText(e.Msg, MaxReasonText)}
		if !ValidErrorCode(l.Err.Code) {
			l.Err.Code = CodeDocker
		}
	}
	b, err := json.Marshal(l)
	// JSON escaping grows text (<, > and & become \u003c, quotes get a
	// backslash): shorten the message until the line fits MaxErrorLine.
	for err == nil && !ok && len(b) > MaxErrorLine && l.Err.Msg != "" {
		l.Err.Msg = cutRune(l.Err.Msg, len(l.Err.Msg)/2)
		b, err = json.Marshal(l)
	}
	if err != nil {
		return
	}
	writeLine(st, string(b))
}

// cutRune returns s cut to at most n bytes on a rune boundary.
func cutRune(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
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
