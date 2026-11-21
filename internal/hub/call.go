package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// ErrAgentProtocol marks an op that failed because the agent broke the v1
// protocol (an over-long or malformed line, a second or missing result, too
// many lines or bytes). The stream is closed and the op fails.
var ErrAgentProtocol = errors.New("agent_protocol")

func protoErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrAgentProtocol, fmt.Sprintf(format, a...))
}

// postResultWait is how long the hub waits for the stream to end after the
// result line; a byte that arrives first is a violation.
const postResultWait = 2 * time.Second

// Call runs a v1 op: one request line, then progress and event lines to onLine
// (may be nil; each line's payload is raw, untrusted JSON), then the result.
// It returns the result payload, or an *agent.OpError for a failed result, or
// an error wrapping ErrAgentProtocol if the agent broke the protocol.
//
// The op must be in the op table and, on a remote session, RemoteOK (checked by
// open, independent of the negotiated version). A v1 op other than `hello`
// needs a session that negotiated v1.
func (s *Session) Call(ctx context.Context, op string, args any, onLine func(agent.Line)) (json.RawMessage, error) {
	return s.call(ctx, op, args, onLine, false)
}

func (s *Session) call(ctx context.Context, op string, args any, onLine func(agent.Line), quiet bool) (json.RawMessage, error) {
	spec, ok := s.spec(op)
	if !ok {
		return nil, ErrUnknownOp
	}
	if op == agent.OpLogs {
		return nil, errors.New("hub: logs is a v0 stream op (use Logs)")
	}
	if op != agent.OpHello && s.Version() < 1 {
		// open would let a RemoteOK v1 op through; the version gate is separate.
		if !spec.RemoteOK && s.ID != LocalID {
			return nil, ErrLocalOnly
		}
		return nil, ErrNeedsV1
	}
	req, err := agent.MarshalRequest(op, args)
	if err != nil {
		return nil, err
	}
	st, err := s.openStream(op, op != agent.OpHello)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	// Closing a yamux stream locally does not wake a blocked Read, so the
	// watcher also expires the read deadline.
	st.setStop(context.AfterFunc(ctx, func() {
		_ = st.SetReadDeadline(time.Unix(1, 0))
		st.shut()
	}))
	_ = st.SetWriteDeadline(time.Now().Add(agent.WriteTimeout))
	if _, err := st.Write(req); err != nil {
		return nil, ctxOr(ctx, err)
	}
	raw, err := s.readResult(st, spec, onLine)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrAgentProtocol) && !quiet && s.protoLogs.Take(op) {
			s.log.Warn("agent broke the op protocol; op failed", "agent", s.ID, "op", op, "err", err)
		}
		return nil, err
	}
	return raw, nil
}

func ctxOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (s *Session) readResult(st *stream, spec agent.OpSpec, onLine func(agent.Line)) (json.RawMessage, error) {
	br := bufio.NewReaderSize(st, 4096)
	limit := spec.MaxLineBytes()
	var total, progress int
	for {
		line, err := agent.ReadLine(br, limit)
		switch {
		case errors.Is(err, agent.ErrLineTooLong):
			return nil, protoErr("line over %d bytes", limit)
		case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
			return nil, protoErr("stream ended without a result")
		case err != nil:
			return nil, err
		}
		l, err := agent.ParseAgentLine(line, spec)
		if err != nil {
			return nil, protoErr("%v", err)
		}
		if l.T != agent.LineEvent { // events are unbounded by design (one op, bounded by its context)
			if total += len(line) + 1; total > agent.MaxOpBytes {
				return nil, protoErr("more than %d bytes", agent.MaxOpBytes)
			}
		}
		switch l.T {
		case agent.LineProgress:
			if progress++; progress > spec.MaxProgressLines {
				return nil, protoErr("more than %d progress lines", spec.MaxProgressLines)
			}
			if onLine != nil {
				onLine(l)
			}
		case agent.LineEvent:
			if onLine != nil {
				onLine(l)
			}
		case agent.LineResult:
			// Nothing may follow the result: the stream must end.
			_ = st.SetReadDeadline(time.Now().Add(postResultWait))
			if _, err := br.ReadByte(); err == nil {
				return nil, protoErr("data after the result")
			}
			if !*l.OK {
				return nil, l.Err
			}
			return l.R, nil
		}
	}
}
