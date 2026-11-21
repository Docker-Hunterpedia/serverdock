// Package api is the control-plane HTTP surface: one route table
// (routes.go), the middleware every route passes (middleware.go: Host
// allowlist, Origin, content type, credential resolution by route kind, CSRF,
// security headers), the SPA (spa.go) and the listener helpers in this file.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/limits"
)

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
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

// WithTrustedProxy marks every connection the server accepts as coming from our
// own reverse proxy, so limits.ClientIP trusts its X-Forwarded-For. Use it ONLY
// on a listener that nothing but that proxy can reach (the control-plane unix
// socket in a 0700 directory). The mark is set per connection through
// http.Server.ConnContext, never derived from the peer address.
//
// Our proxy always sets exactly one X-Forwarded-For value holding one IP (Caddy
// replaces a client's value when no trusted_proxies are configured). A request
// without exactly that is answered 400 here, before any handler, and logged once
// a minute: it means the proxy is misconfigured or something else is talking to
// the socket.
func WithTrustedProxy(log *slog.Logger) ServeOption {
	return func(s *http.Server) {
		s.ConnContext = func(ctx context.Context, _ net.Conn) context.Context { return limits.WithProxyMark(ctx) }
		next := s.Handler
		var lastLog atomic.Int64 // unix nanoseconds of the last log line
		s.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := limits.ParseForwarded(r.Header); err != nil {
				now := time.Now().UnixNano()
				if last := lastLog.Load(); now-last >= int64(time.Minute) && lastLog.CompareAndSwap(last, now) && log != nil {
					log.Warn("control plane: request without exactly one X-Forwarded-For value; is the proxy misconfigured?", "path", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_forwarded_for","message":"the proxy must send exactly one X-Forwarded-For value"}}` + "\n"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
