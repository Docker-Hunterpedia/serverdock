package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/limits"
	"github.com/Docker-Hunterpedia/serverdock/internal/memstat"
)

// AuthKind says which credential a route reads (docs/plans/0003, "Credential
// resolution"). Only that credential is read; everything else on the request is
// ignored for authentication.
type AuthKind int

const (
	// Public: no credential. A session or pending cookie is not used to authenticate.
	Public AuthKind = iota
	// SetupOpen: no credential; the setup token travels in the POST body, and the
	// route answers 404 once an admin exists.
	SetupOpen
	// Pending2FA: only the __Host-sd_pending cookie.
	Pending2FA
	// Session: only the __Host-sd_session cookie (an Authorization header is a 400).
	Session
	// SessionOrToken: Authorization if present (cookies then ignored entirely),
	// otherwise the session cookie.
	SessionOrToken
	// DebugSocket: no credential (filesystem permissions); mounted only on the
	// debug listener, never on the control plane.
	DebugSocket
)

func (k AuthKind) String() string {
	return [...]string{"Public", "SetupOpen", "Pending2FA", "Session", "SessionOrToken", "DebugSocket"}[k]
}

// Route is one entry of the route table. Every route of the control plane and of
// the debug socket is declared here, and the muxes are built only from this
// table (a test fails on any other mux registration in this package or in the
// command).
type Route struct {
	Method, Pattern string
	Auth            AuthKind
	// Upgrade marks a WebSocket route: it requires a session and an exact Origin.
	Upgrade bool
	Handler http.Handler
}

// Sessions resolves session cookies (auth.Store implements it).
type Sessions interface {
	LookupSession(ctx context.Context, secret string) (*auth.Session, error)
	PendingUser(ctx context.Context, secret string) (int64, error)
}

// Config is what New needs.
type Config struct {
	// SPA is the embedded single-page app.
	SPA fs.FS
	// Sessions resolves cookies; nil means every cookie is "no credential".
	Sessions Sessions
	// Tokens verifies API tokens (M2 step 8); nil means every bearer token is refused.
	Tokens TokenVerifier
	// BadTokens limits failed bearer-token lookups per client IP (5 a minute, burst
	// 10, like the hub's agent token); nil means that default.
	BadTokens *limits.Keyed
	// Auth holds the setup and sign-in handlers (M2 step 6b); nil means stubs.
	Auth *AuthHandlers
	// PanelHosts are the extra names the Host allowlist accepts (--panel-host).
	PanelHosts []string
	// DebugExtra registers further debug routes (the hub's /debug/agents...).
	DebugExtra []func(*http.ServeMux)
	// ExtraRoutes are added to the table (tests: a SessionOrToken route and a
	// WebSocket route, until the real ones exist).
	ExtraRoutes []Route
	Log         *slog.Logger
}

// AuthHandlers are the pre-session endpoints.
type AuthHandlers struct {
	SetupStatus, SetupEnroll, SetupComplete, Login, TwoFA, SessionInfo, Reauth, Logout http.Handler
}

// Server is the control-plane handler plus the debug handler.
type Server struct {
	cfg    Config
	routes []Route
	hosts  map[string]bool
	log    *slog.Logger
	// badTokens is the failed-token limiter.
	badTokens *limits.Keyed
}

// New builds the route table and the servers' muxes from it.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, hosts: map[string]bool{}, log: cfg.Log, badTokens: cfg.BadTokens}
	if s.badTokens == nil {
		s.badTokens = limits.NewKeyed(5, 10, 0, nil)
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	for _, h := range cfg.PanelHosts {
		s.hosts[strings.ToLower(strings.TrimSuffix(h, "."))] = true
	}
	s.routes = s.table()
	return s
}

func stub(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"stub":true}` + "\n"))
}

// table is THE route table.
func (s *Server) table() []Route {
	a := s.cfg.Auth
	if a == nil {
		a = &AuthHandlers{}
	}
	or := func(h http.Handler) http.Handler {
		if h == nil {
			return http.HandlerFunc(stub)
		}
		return h
	}
	debugMux := http.NewServeMux()
	for _, f := range s.cfg.DebugExtra {
		f(debugMux)
	}
	rs := []Route{
		{Method: "GET", Pattern: "/healthz", Auth: Public, Handler: http.HandlerFunc(healthz)},
		{Method: "GET", Pattern: "/", Auth: Public, Handler: spaHandler(s.cfg.SPA)},
		// Remote agents connect to the hub's own TLS listener, never here: without
		// this the SPA fallback would answer for /agent/connect.
		{Method: "GET", Pattern: "/agent/", Auth: Public, Handler: http.NotFoundHandler()},
		{Method: "GET", Pattern: "/api/v1/setup/status", Auth: Public, Handler: or(a.SetupStatus)},
		{Method: "POST", Pattern: "/api/v1/setup/enroll", Auth: SetupOpen, Handler: or(a.SetupEnroll)},
		{Method: "POST", Pattern: "/api/v1/setup/complete", Auth: SetupOpen, Handler: or(a.SetupComplete)},
		{Method: "POST", Pattern: "/api/v1/auth/login", Auth: Public, Handler: or(a.Login)},
		{Method: "POST", Pattern: "/api/v1/auth/2fa", Auth: Pending2FA, Handler: or(a.TwoFA)},
		{Method: "GET", Pattern: "/api/v1/auth/session", Auth: Session, Handler: or(a.SessionInfo)},
		{Method: "POST", Pattern: "/api/v1/auth/reauth", Auth: Session, Handler: or(a.Reauth)},
		{Method: "POST", Pattern: "/api/v1/auth/logout", Auth: Session, Handler: or(a.Logout)},
		// Debug socket only.
		{Method: "GET", Pattern: "/debug/mem", Auth: DebugSocket, Handler: http.HandlerFunc(debugMem)},
		{Method: "GET", Pattern: "/debug/agents", Auth: DebugSocket, Handler: debugMux},
		{Method: "GET", Pattern: "/debug/agents/{id}/logs/{container}", Auth: DebugSocket, Handler: debugMux},
	}
	return append(rs, s.cfg.ExtraRoutes...)
}

func debugMem(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(memstat.Read())
}

// Routes returns the table (tests, and the OpenAPI document later).
func (s *Server) Routes() []Route { return append([]Route(nil), s.routes...) }

// Handler serves the control plane: every route except the DebugSocket ones.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes {
		if rt.Auth == DebugSocket {
			continue
		}
		mux.Handle(rt.Method+" "+rt.Pattern, s.wrap(rt))
	}
	return s.outer(mux)
}

// DebugHandler serves only the DebugSocket routes, on the debug unix socket.
func (s *Server) DebugHandler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes {
		if rt.Auth == DebugSocket {
			mux.Handle(rt.Method+" "+rt.Pattern, rt.Handler)
		}
	}
	return headers(mux)
}
