// Package api is the control-plane HTTP surface: /healthz, SPA serving and
// the optional debug handlers.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/memstat"
)

// Server is the control-plane handler. Other packages (the hub) register
// additional routes with Handle.
type Server struct {
	mux *http.ServeMux
}

// New builds the handler: /healthz plus the SPA for everything else.
func New(spa fs.FS) *Server {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", healthz)
	s.mux.Handle("GET /", spaHandler(spa))
	// Remote agents connect to the hub's own TLS listener, never here: without
	// this the SPA fallback would answer 200 for /agent/connect.
	s.mux.Handle("GET /agent/", http.NotFoundHandler())
	return s
}

// Handle registers an extra route (Go 1.22+ ServeMux pattern).
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return securityHeaders(s.mux) }

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// spaHandler serves files from spa; paths without a file extension that do
// not exist fall back to index.html (client-side routing). Missing paths that
// look like files return 404.
func spaHandler(spa fs.FS) http.Handler {
	files := http.FileServerFS(spa)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if st, err := fs.Stat(spa, p); err == nil && !st.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		if path.Ext(p) != "" {
			http.NotFound(w, r)
			return
		}
		index, err := fs.ReadFile(spa, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}

// DebugHandler serves /debug/mem plus any extra routes (for example the hub's
// /debug/agents). It is only served on the --debug-socket unix socket (0600 in a
// 0700 directory) and exposes no secrets.
func DebugHandler(extra ...func(*http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	for _, f := range extra {
		f(mux)
	}
	mux.HandleFunc("GET /debug/mem", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(memstat.Read())
	})
	return mux
}

// HTTP server limits (plan 0002 M1 step 6).
const (
	// MaxHeaderBytes caps the request header size.
	MaxHeaderBytes = 64 << 10
	// MaxBodyBytes caps a request body (reads fail past it).
	MaxBodyBytes = 1 << 20
)

// ServeOption adjusts the server Serve builds.
type ServeOption func(*http.Server)

// WithReadHeaderTimeout replaces the 10 s header timeout. On a TLS listener it
// also bounds the TLS handshake (net/http uses it as the handshake deadline).
func WithReadHeaderTimeout(d time.Duration) ServeOption {
	return func(s *http.Server) { s.ReadHeaderTimeout = d }
}

// WithoutKeepAlives answers every request with Connection: close, for
// listeners whose only request is a one-shot upgrade.
func WithoutKeepAlives() ServeOption {
	return func(s *http.Server) { s.SetKeepAlivesEnabled(false) }
}

// Serve serves h on ln until ctx is cancelled, then shuts down gracefully,
// waiting up to grace for in-flight requests. It returns nil on a clean stop.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, grace time.Duration, opts ...ServeOption) error {
	// No global ReadTimeout or WriteTimeout: listeners here carry upgraded
	// WebSocket sessions and long log streams. Slow headers are bounded by
	// ReadHeaderTimeout; idle keep-alives by IdleTimeout.
	srv := &http.Server{
		Handler:           http.MaxBytesHandler(h, MaxBodyBytes),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    MaxHeaderBytes,
	}
	for _, o := range opts {
		o(srv)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	err := srv.Shutdown(sctx)
	if err != nil {
		_ = srv.Close()
	}
	if serr := <-errc; serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		return serr
	}
	return err
}
