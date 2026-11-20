// Package hub is the server side of agent connections: a WebSocket endpoint
// whose connection carries a yamux session in which the server opens streams.
package hub

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/limits"
)

var agentIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// LocalID is the id of the in-process local agent. Remote agents may not use
// it (or any case variant), so they cannot replace or impersonate the local agent.
const LocalID = "local"

func reservedID(id string) bool { return strings.EqualFold(id, LocalID) }

// Limits (plan 0002 M1 step 6).
const (
	// DefaultMaxSessions is the default cap on remote agent sessions.
	DefaultMaxSessions = 16
	// MaxServerStreams bounds the streams the hub has open to the local
	// in-process session.
	MaxServerStreams = 32
	// MaxRemoteStreams bounds the streams the hub has open to one remote agent
	// session: 16 sessions x 8 x 256 KiB (yamux's minimum window) = 32 MiB worst case.
	MaxRemoteStreams = 8
	// AuthFailuresPerMinute and AuthFailureBurst shape the per-IP limiter on
	// failed token checks.
	AuthFailuresPerMinute = 5
	AuthFailureBurst      = 10
)

// ErrTooManyStreams is returned when a session already has MaxServerStreams open.
var ErrTooManyStreams = errors.New("hub: too many open streams to this agent")

// Hub tracks connected agents.
type Hub struct {
	ctx       context.Context
	tokenHash [sha256.Size]byte
	hasToken  bool
	log       *slog.Logger

	maxSessions int
	// compare checks a presented token; replaced by tests to count calls.
	compare  func(tok string) bool
	fails    *limits.Keyed // failed token checks per client IP
	failLogs *limits.Keyed // one auth-failure log line per IP per minute

	mu       sync.Mutex
	seq      uint64
	sessions map[string]*Session
	slots    map[string]int // remote session slots in use, per agent id
}

func streamCap(id string) int {
	if id == LocalID {
		return MaxServerStreams
	}
	return MaxRemoteStreams
}

// Option configures New.
type Option func(*Hub)

// WithMaxSessions sets the remote session cap (default DefaultMaxSessions).
func WithMaxSessions(n int) Option { return func(h *Hub) { h.maxSessions = n } }

// New creates a hub. ctx is the server's lifetime context: cancelling it closes
// every session, including hijacked WebSocket connections that http.Server
// shutdown cannot close. An empty token disables remote connections.
func New(ctx context.Context, token string, log *slog.Logger, opts ...Option) *Hub {
	h := &Hub{
		ctx: ctx, log: log, sessions: map[string]*Session{}, slots: map[string]int{},
		maxSessions: DefaultMaxSessions,
		fails:       limits.NewKeyed(AuthFailuresPerMinute, AuthFailureBurst, limits.MaxKeys, nil),
		failLogs:    limits.NewKeyed(1, 1, limits.MaxKeys, nil),
	}
	if token != "" {
		h.tokenHash, h.hasToken = sha256.Sum256([]byte(token)), true
	}
	h.compare = func(tok string) bool {
		got := sha256.Sum256([]byte(tok))
		return subtle.ConstantTimeCompare(got[:], h.tokenHash[:]) == 1
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Session is one connected agent.
type Session struct {
	ID    string
	Seq   uint64
	Since time.Time
	ys    *yamux.Session
	sem   chan struct{} // stream slots: MaxServerStreams (local) or MaxRemoteStreams
	// remote is the peer's address for log lines ("in-process" for the local agent).
	remote string
	// specFor looks up an op's spec; agent.SpecFor, replaced only by tests.
	specFor func(string) (agent.OpSpec, bool)
}

// Info describes a session for the debug listing.
type Info struct {
	ID    string    `json:"id"`
	Seq   uint64    `json:"seq"`
	Since time.Time `json:"since"`
}

func (h *Hub) authorized(r *http.Request) bool {
	if !h.hasToken {
		return false
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return h.compare(tok)
}

// reserve claims a session slot for id. Reconnecting under an id that already
// holds a slot does not need a new one, so an agent re-dialing before the hub
// notices its dead session is not refused at the cap.
func (h *Hub) reserve(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.slots[id] == 0 && len(h.slots) >= h.maxSessions {
		return false
	}
	h.slots[id]++
	return true
}

func (h *Hub) release(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.slots[id]--; h.slots[id] <= 0 {
		delete(h.slots, id)
	}
}

// ServeHTTP handles GET /agent/connect. The handler blocks for the whole
// session; its context derives from the hub's lifetime context.
//
// Order of checks (each refuses before the next, more expensive, step):
//  1. the per-IP auth-failure limiter (429): a limited IP never reaches the compare;
//  2. the bearer-token compare, constant time (401, counted as a failure);
//  3. the session cap (503);
//  4. websocket.Accept, the only step that upgrades the connection.
//
// (Concurrent handshakes per client IP are bounded one layer down, by the
// listener's per-IP connection cap, so there is no separate handshake counter.)
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.hasToken {
		http.Error(w, "agent connections are disabled", http.StatusServiceUnavailable)
		return
	}
	ip := limits.ClientIP(r, false) // direct TLS listener: never trust X-Forwarded-For
	if blocked, retry := h.fails.Blocked(ip); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry/time.Second)+1))
		http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
		return
	}
	if !h.authorized(r) {
		h.fails.Take(ip)
		if h.failLogs.Take(ip) {
			h.log.Warn("agent authentication failed", "client", ip)
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.URL.Query().Get("id")
	if !agentIDRe.MatchString(id) || reservedID(id) {
		http.Error(w, "bad agent id", http.StatusBadRequest)
		return
	}
	if !h.reserve(id) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "too many agent sessions", http.StatusServiceUnavailable)
		return
	}
	defer h.release(id)
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already replied
	}
	defer c.CloseNow()
	sctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	nc := websocket.NetConn(sctx, c, websocket.MessageBinary)
	if err := h.attach(sctx, id, ip, nc, agent.DefaultKeepAlive); err != nil {
		h.log.Warn("agent session ended", "agent", id, "err", err)
	}
}

// Attach runs a yamux client session over conn and blocks until it ends or ctx
// is cancelled. A new session with the same id replaces the old one.
func (h *Hub) Attach(ctx context.Context, id string, conn net.Conn, keepAlive time.Duration) error {
	return h.attach(ctx, id, "in-process", conn, keepAlive)
}

// attach is Attach plus the peer's limiting key (an IP, never a token), used
// only in log lines.
func (h *Hub) attach(ctx context.Context, id, remote string, conn net.Conn, keepAlive time.Duration) error {
	ys, err := yamux.Client(conn, agent.YamuxConfig(keepAlive))
	if err != nil {
		return fmt.Errorf("yamux client: %w", err)
	}
	h.mu.Lock()
	h.seq++
	s := &Session{ID: id, Seq: h.seq, Since: time.Now(), ys: ys, sem: make(chan struct{}, streamCap(id)), remote: remote, specFor: agent.SpecFor}
	old := h.sessions[id]
	h.sessions[id] = s
	h.mu.Unlock()
	if old != nil {
		h.log.Warn("agent session replaced by a new connection", "agent", id, "old_remote", old.remote, "new_remote", remote)
		old.ys.Close()
	}
	h.log.Info("agent connected", "agent", id, "remote", remote)
	// Agents never open streams to the hub, so the first one is a protocol
	// violation: end the session (and log once). Merely closing the stream is
	// not enough: yamux keeps an accepted-then-closed stream and its buffered
	// data until the close timeout, which an agent could use to pin memory.
	var drain sync.WaitGroup
	drain.Go(func() {
		if _, err := ys.AcceptStream(); err == nil {
			h.log.Warn("agent opened a stream; ending its session (protocol violation)", "agent", id, "remote", remote)
			ys.Close()
		}
	})
	defer func() {
		ys.Close()
		drain.Wait()
		h.mu.Lock()
		if h.sessions[id] == s {
			delete(h.sessions, id)
		}
		h.mu.Unlock()
		h.log.Info("agent disconnected", "agent", id)
	}()
	select {
	case <-ys.CloseChan():
	case <-ctx.Done():
	}
	return nil
}

// Get returns the current session for id.
func (h *Hub) Get(id string) (*Session, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	return s, ok
}

// List returns connected agents sorted by id.
func (h *Hub) List() []Info {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Info, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, Info{ID: s.ID, Seq: s.Seq, Since: s.Since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Logs opens a log stream. Closing the returned stream half-closes (FIN) it;
// the agent cancels its Docker request when it sees that. Cancelling ctx closes it.
func (s *Session) Logs(ctx context.Context, container string, tail int) (io.ReadCloser, error) {
	op := agent.Op{Op: agent.OpLogs, Container: container, Tail: tail}
	if err := op.Validate(); err != nil {
		return nil, err
	}
	st, err := s.open(agent.OpLogs)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(op)
	_ = st.SetWriteDeadline(time.Now().Add(agent.WriteTimeout))
	if _, err := st.Write(append(b, '\n')); err != nil {
		st.Close()
		return nil, err
	}
	st.setStop(context.AfterFunc(ctx, st.shut))
	return st, nil
}

// ErrLocalOnly is returned when an op that is not RemoteOK is requested on a remote session.
var ErrLocalOnly = errors.New("hub: this operation is only available on the local server (remote agents are read-only)")

// ErrUnknownOp is returned for an op name that is not in the agent's op table.
var ErrUnknownOp = errors.New("hub: unknown operation")

// open starts a stream for the named op. Every op goes through here, so the rule
// "remote agents are read-only until Phase 2" is enforced in one place: an op
// that is unknown is refused, and one that is not RemoteOK is refused unless the
// session is the in-process local agent (LocalID is reserved, and only
// runServer attaches it, over the in-memory pipe).
//
// A slot is released when the hub closes its end of the stream. yamux then
// tears the stream down within StreamCloseTimeout (10 s) even if the agent never
// closes its side, so streams that are closed-but-lingering are bounded by the
// hub's own open rate, which an agent cannot influence: opens happen only for an
// operator's request (the debug route today, the panel later).
func (s *Session) open(name string) (*stream, error) {
	spec, ok := s.specFor(name)
	if !ok {
		return nil, ErrUnknownOp
	}
	if !spec.RemoteOK && s.ID != LocalID {
		return nil, ErrLocalOnly
	}
	select {
	case s.sem <- struct{}{}:
	default:
		return nil, ErrTooManyStreams
	}
	ys, err := s.ys.OpenStream()
	if err != nil {
		<-s.sem
		return nil, err
	}
	return &stream{Stream: ys, release: func() { <-s.sem }}, nil
}

type stream struct {
	*yamux.Stream
	release func()
	once    sync.Once

	mu   sync.Mutex
	stop func() bool // cancels the context watcher; set once, after open
}

func (s *stream) setStop(f func() bool) {
	s.mu.Lock()
	s.stop = f
	s.mu.Unlock()
}

// shut closes the stream and frees its slot. It never touches s.stop, so the
// context watcher can run it even while setStop is still being called.
func (s *stream) shut() {
	_ = s.Stream.Close()
	s.once.Do(s.release)
}

func (s *stream) Close() error {
	s.mu.Lock()
	stop := s.stop
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	s.shut()
	return nil
}

// DebugRoutes registers /debug/agents and /debug/agents/{id}/logs/{container}.
// Mount these only on the debug listener.
func (h *Hub) DebugRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/agents", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.List())
	})
	mux.HandleFunc("GET /debug/agents/{id}/logs/{container}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.Get(r.PathValue("id"))
		if !ok {
			http.Error(w, "no such agent", http.StatusNotFound)
			return
		}
		tail := 100
		if v := r.URL.Query().Get("tail"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				http.Error(w, "bad tail", http.StatusBadRequest)
				return
			}
			tail = n
		}
		rc, err := s.Logs(r.Context(), r.PathValue("container"), tail)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fl, _ := w.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, err := rc.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				if fl != nil {
					fl.Flush()
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					h.log.Debug("log proxy ended", "err", err)
				}
				return
			}
		}
	})
}
