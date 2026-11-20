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
)

var agentIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// LocalID is the id of the in-process local agent. Remote agents may not use
// it (or any case variant), so they cannot replace or impersonate the local agent.
const LocalID = "local"

func reservedID(id string) bool { return strings.EqualFold(id, LocalID) }

// Hub tracks connected agents.
type Hub struct {
	ctx       context.Context
	tokenHash [sha256.Size]byte
	hasToken  bool
	log       *slog.Logger

	mu       sync.Mutex
	seq      uint64
	sessions map[string]*Session
}

// New creates a hub. ctx is the server's lifetime context: cancelling it closes
// every session, including hijacked WebSocket connections that http.Server
// shutdown cannot close. An empty token disables remote connections.
func New(ctx context.Context, token string, log *slog.Logger) *Hub {
	h := &Hub{ctx: ctx, log: log, sessions: map[string]*Session{}}
	if token != "" {
		h.tokenHash, h.hasToken = sha256.Sum256([]byte(token)), true
	}
	return h
}

// Session is one connected agent.
type Session struct {
	ID    string
	Seq   uint64
	Since time.Time
	ys    *yamux.Session
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
	got := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(got[:], h.tokenHash[:]) == 1
}

// ServeHTTP handles GET /agent/connect. The handler blocks for the whole
// session; its context derives from the hub's lifetime context.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.hasToken {
		http.Error(w, "agent connections are disabled", http.StatusServiceUnavailable)
		return
	}
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.URL.Query().Get("id")
	if !agentIDRe.MatchString(id) || reservedID(id) {
		http.Error(w, "bad agent id", http.StatusBadRequest)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already replied
	}
	defer c.CloseNow()
	sctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	nc := websocket.NetConn(sctx, c, websocket.MessageBinary)
	if err := h.Attach(sctx, id, nc, agent.DefaultKeepAlive); err != nil {
		h.log.Warn("agent session ended", "agent", id, "err", err)
	}
}

// Attach runs a yamux client session over conn and blocks until it ends or ctx
// is cancelled. A new session with the same id replaces the old one.
func (h *Hub) Attach(ctx context.Context, id string, conn net.Conn, keepAlive time.Duration) error {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.EnableKeepAlive = keepAlive > 0
	if keepAlive > 0 {
		cfg.KeepAliveInterval = keepAlive
	}
	ys, err := yamux.Client(conn, cfg)
	if err != nil {
		return fmt.Errorf("yamux client: %w", err)
	}
	h.mu.Lock()
	h.seq++
	s := &Session{ID: id, Seq: h.seq, Since: time.Now(), ys: ys}
	old := h.sessions[id]
	h.sessions[id] = s
	h.mu.Unlock()
	if old != nil {
		old.ys.Close()
	}
	h.log.Info("agent connected", "agent", id)
	defer func() {
		ys.Close()
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
	st, err := s.ys.OpenStream()
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(op)
	_ = st.SetWriteDeadline(time.Now().Add(agent.WriteTimeout))
	if _, err := st.Write(append(b, '\n')); err != nil {
		st.Close()
		return nil, err
	}
	rc := &stream{Stream: st}
	rc.stop = context.AfterFunc(ctx, func() { st.Close() })
	return rc, nil
}

type stream struct {
	*yamux.Stream
	stop func() bool
}

func (s *stream) Close() error { s.stop(); return s.Stream.Close() }

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
