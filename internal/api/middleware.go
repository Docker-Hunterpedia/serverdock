package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/netip"
	"strings"

	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/limits"
)

// Cookie and header names.
const (
	SessionCookie = "__Host-sd_session"
	PendingCookie = "__Host-sd_pending"
	CSRFHeader    = "X-SD-CSRF"
)

// TokenInfo is what a verified API token grants (M2 step 8).
type TokenInfo struct {
	ID     int64
	UserID int64
	Scope  string
}

// TokenVerifier checks a bearer token. It returns ErrTokenExpired for an expired
// token and any other error for an unknown or revoked one.
type TokenVerifier interface {
	VerifyToken(ctx context.Context, token string) (TokenInfo, error)
}

// ErrTokenExpired is the TokenVerifier error that answers 401 token_expired.
var ErrTokenExpired = errors.New("api: token expired")

type storeTokens struct{ s *auth.Store }

// NewTokenVerifier checks bearer tokens against the database.
func NewTokenVerifier(s *auth.Store) TokenVerifier { return storeTokens{s} }

func (v storeTokens) VerifyToken(ctx context.Context, token string) (TokenInfo, error) {
	c, err := v.s.VerifyToken(ctx, token)
	if errors.Is(err, auth.ErrAPITokenExpired) {
		return TokenInfo{}, ErrTokenExpired
	}
	if err != nil {
		return TokenInfo{}, err
	}
	return TokenInfo{ID: c.ID, UserID: c.UserID, Scope: c.Scope}, nil
}

// PrincipalKind says how a request authenticated.
type PrincipalKind int

const (
	// Anonymous: no credential was used (Public, SetupOpen, DebugSocket routes).
	Anonymous PrincipalKind = iota
	// PendingPrincipal: a valid pending-2FA cookie on the 2FA route; NOT a session.
	PendingPrincipal
	// SessionPrincipal: a valid session cookie.
	SessionPrincipal
	// TokenPrincipal: a valid bearer token.
	TokenPrincipal
)

// Principal is who a request is, as resolved by the middleware from the route's
// AuthKind. Handlers read it with PrincipalFrom.
type Principal struct {
	Kind          PrincipalKind
	Session       *auth.Session // SessionPrincipal
	SessionSecret string        // SessionPrincipal: the raw cookie value (for CSRF and logout)
	PendingSecret string        // PendingPrincipal
	PendingUserID int64         // PendingPrincipal
	Token         TokenInfo     // TokenPrincipal
	// OldSessionSecret is a session cookie presented on a route that does not
	// authenticate with it (login, 2FA, setup): the handler revokes it when it
	// mints a new session, or clears it if it is stale.
	OldSessionSecret string
}

type principalKey struct{}

// PrincipalFrom returns the request's principal (Anonymous when none).
func PrincipalFrom(ctx context.Context) *Principal {
	if p, _ := ctx.Value(principalKey{}).(*Principal); p != nil {
		return p
	}
	return &Principal{}
}

// ---- errors, cookies, headers ----------------------------------------------

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type errBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// WriteError answers with the standard error envelope.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteErrorFields(w, status, code, msg, nil)
}

// WriteErrorFields is WriteError with extra top-level fields next to "error"
// (for example tries_left).
func WriteErrorFields(w http.ResponseWriter, status int, code, msg string, extra map[string]any) {
	var b errBody
	b.Error.Code, b.Error.Message, b.Error.RequestID = code, msg, newRequestID()
	out := map[string]any{"error": b.Error}
	for k, v := range extra {
		out[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

// WriteJSON answers with status and v as JSON.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// SetCookie sets a __Host- cookie: Secure, HttpOnly, SameSite=Strict, Path=/.
func SetCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// ClearCookie expires a cookie (Max-Age=0).
func ClearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// CSRFToken is base64url(HMAC-SHA256(key = the raw session secret, msg =
// "sd-csrf-v1")): recomputed from the cookie on every request, never stored (the
// database holds only SHA-256 of the secret, which is not the key).
func CSRFToken(sessionSecret string) string {
	m := hmac.New(sha256.New, []byte(sessionSecret))
	m.Write([]byte("sd-csrf-v1"))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// headers sets the security headers on every response; /api/ also gets no-store.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// ---- Host allowlist ----------------------------------------------------------

// hostAllowed: the Host header, port stripped, must be an IP literal (DNS
// rebinding needs an attacker-controlled name, so literals are safe), exactly
// "localhost" (the SSH-tunnel path), or a --panel-host name.
func (s *Server) hostAllowed(host string) bool {
	h := host
	if strings.HasPrefix(h, "[") { // [v6] or [v6]:port
		end := strings.IndexByte(h, ']')
		if end < 0 || (len(h) > end+1 && h[end+1] != ':') {
			return false
		}
		_, err := netip.ParseAddr(h[1:end])
		return err == nil && !strings.Contains(h[1:end], "%")
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		if strings.Contains(h[:i], ":") { // an unbracketed v6 is not a valid Host
			return false
		}
		h = h[:i]
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return !strings.Contains(h, "%")
	}
	h = strings.ToLower(h)
	return h == "localhost" || s.hosts[h]
}

// outer is the first middleware of the control plane: headers, then the Host
// allowlist (421 for a mismatch).
func (s *Server) outer(next http.Handler) http.Handler {
	return headers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			WriteError(w, http.StatusMisdirectedRequest, "misdirected", "this name is not one this server answers to; use its IP address, localhost or a configured --panel-host name")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// ---- per-route chain ------------------------------------------------------------

func unsafeMethod(m string) bool { return m != http.MethodGet && m != http.MethodHead }

// wrap applies, in order: Origin, content type, credential resolution by the
// route's AuthKind, the Origin and CSRF rules for sessions, then the handler.
func (s *Server) wrap(rt Route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unsafe := unsafeMethod(r.Method)
		// Origin, when present, must be this very host over https, on every unsafe
		// request: pre-session ones (setup, login, 2FA) included.
		if vals := r.Header.Values("Origin"); unsafe && len(vals) > 0 {
			if len(vals) != 1 || vals[0] != "https://"+r.Host {
				WriteError(w, http.StatusForbidden, "origin", "the request's Origin is not this server")
				return
			}
		}
		// Every POST under /api/ is JSON: a cross-site page cannot send that
		// without a CORS preflight, and we never answer preflights with CORS headers.
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/") && !isJSON(r.Header.Get("Content-Type")) {
			WriteError(w, http.StatusBadRequest, "invalid_input", "the content type must be application/json")
			return
		}
		p, ok := s.resolve(w, r, rt)
		if !ok {
			return
		}
		if rt.Upgrade && p.Kind != SessionPrincipal {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "a session is required")
			return
		}
		if p.Kind == SessionPrincipal && (unsafe || rt.Upgrade) {
			// A cookie-authenticated unsafe request (and a WebSocket upgrade) must
			// carry Origin; CSRF is never skipped because some other header is present.
			if r.Header.Get("Origin") == "" {
				WriteError(w, http.StatusForbidden, "origin", "this request needs an Origin header")
				return
			}
			if unsafe && subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(CSRFToken(p.SessionSecret))) != 1 {
				WriteError(w, http.StatusForbidden, "csrf", "missing or wrong "+CSRFHeader+" header")
				return
			}
		}
		if rt.Upgrade && r.Header.Get("Origin") != "https://"+r.Host {
			WriteError(w, http.StatusForbidden, "origin", "the request's Origin is not this server")
			return
		}
		rt.Handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func isJSON(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

// resolve reads the credential the route's kind allows, and only that one. It
// writes the response and returns false when the request cannot go on.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, rt Route) (*Principal, bool) {
	p := &Principal{}
	switch rt.Auth {
	case Public, SetupOpen, DebugSocket:
		// No credential. A presented session cookie is only remembered so a handler
		// that mints a new session can revoke it; it authenticates nothing.
		p.OldSessionSecret = cookieValue(r, SessionCookie)
		return p, true

	case Pending2FA:
		p.OldSessionSecret = cookieValue(r, SessionCookie) // ignored for authentication
		secret := cookieValue(r, PendingCookie)
		if secret == "" || s.cfg.Sessions == nil {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "sign in with your password first")
			return nil, false
		}
		uid, err := s.cfg.Sessions.PendingUser(r.Context(), secret)
		if err != nil {
			ClearCookie(w, PendingCookie)
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "this step expired; sign in again")
			return nil, false
		}
		p.Kind, p.PendingSecret, p.PendingUserID = PendingPrincipal, secret, uid
		return p, true

	case Session:
		if r.Header.Get("Authorization") != "" {
			WriteError(w, http.StatusBadRequest, "invalid_input", "Authorization is not accepted on this route; it uses the session cookie")
			return nil, false
		}
		return s.sessionPrincipal(w, r, p)

	case SessionOrToken:
		if authz := r.Header.Get("Authorization"); authz != "" {
			// Cookies are then ignored entirely; no CSRF is needed, because no
			// ambient credential is used.
			return s.tokenPrincipal(w, r, p, authz)
		}
		return s.sessionPrincipal(w, r, p)
	}
	WriteError(w, http.StatusInternalServerError, "internal", "unknown route kind")
	return nil, false
}

func (s *Server) sessionPrincipal(w http.ResponseWriter, r *http.Request, p *Principal) (*Principal, bool) {
	secret := cookieValue(r, SessionCookie)
	if secret == "" || s.cfg.Sessions == nil {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "sign in first")
		return nil, false
	}
	sess, err := s.cfg.Sessions.LookupSession(r.Context(), secret)
	if err != nil {
		var se *auth.SessionError
		switch {
		case errors.As(err, &se):
			// Unknown, expired or revoked: no credential, and the cookie is cleared.
			ClearCookie(w, SessionCookie)
			WriteError(w, http.StatusUnauthorized, se.Reason, "your session is no longer valid; sign in again")
		case errors.Is(err, auth.ErrNoSession):
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "sign in first")
		default:
			s.log.Error("session lookup failed", "err", err.Error())
			WriteError(w, http.StatusInternalServerError, "internal", "could not check the session")
		}
		return nil, false
	}
	p.Kind, p.Session, p.SessionSecret = SessionPrincipal, sess, secret
	return p, true
}

func (s *Server) tokenPrincipal(w http.ResponseWriter, r *http.Request, p *Principal, authz string) (*Principal, bool) {
	tok, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || !strings.HasPrefix(tok, "sd_") || s.cfg.Tokens == nil {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "a valid API token is required")
		return nil, false
	}
	// The bad-token bucket is checked BEFORE the lookup: an attacker guessing
	// tokens is stopped without touching the database.
	ip := limits.ClientIP(r)
	if blocked, wait := s.badTokens.Blocked(ip); blocked {
		tooMany(w, wait)
		return nil, false
	}
	info, err := s.cfg.Tokens.VerifyToken(r.Context(), tok)
	if err != nil {
		s.badTokens.Take(ip) // every failure (unknown, revoked, expired) counts
		if errors.Is(err, ErrTokenExpired) {
			WriteError(w, http.StatusUnauthorized, "token_expired", "this API token has expired")
		} else {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "a valid API token is required")
		}
		return nil, false
	}
	p.Kind, p.Token = TokenPrincipal, info
	return p, true
}
