package api

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/limits"
	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// The route matrix: every route of the table x every credential case, with the
// expected status written out here from the plan (docs/plans/0003, "Credential
// resolution" and "Host allowlist, Origin and security headers"), not derived
// from the implementation. Later steps and milestones extend it: a route that is
// in the table but not in `known` fails TestEveryRouteIsInTheMatrix.

type mclock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *mclock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *mclock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	t        *testing.T
	srv      *Server
	h        http.Handler
	as       *auth.Store
	st       *store.Store
	clk      *mclock
	userID   int64
	seed     []byte
	session  string // a valid session cookie value
	expired  string
	revoked  string
	pending  string
	hasAdmin bool
	tokens   map[string]string // case name -> bearer token
}

const testPassword = "correct horse battery staple"

func newFixture(t *testing.T, withAdmin bool) *fixture {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secretbox.New(make([]byte, 32))
	f := &fixture{t: t, st: st, clk: &mclock{t: time.Unix(1_800_000_000, 0)}, hasAdmin: withAdmin}
	f.as = auth.NewStore(st, box, auth.NewHasher(), auth.Options{Now: f.clk.Now})
	ctx := context.Background()
	if _, err := f.as.PrepareSetup(ctx); err != nil {
		t.Fatal(err)
	}
	if withAdmin {
		b, _ := os.ReadFile(filepath.Join(dir, auth.SetupTokenFile))
		tok := strings.TrimSpace(string(b))
		er, err := f.as.SetupEnroll(ctx, tok, "admin")
		if err != nil {
			t.Fatal(err)
		}
		f.seed = decodeSecret(t, er.Secret)
		res, err := f.as.SetupComplete(ctx, auth.SetupComplete{Token: tok, EnrollmentID: er.EnrollmentID, Username: "admin", Password: testPassword,
			Code: auth.TOTPCode(f.seed, auth.TOTPCounter(f.clk.Now())), IPKey: "203.0.113.5", UserAgent: "test", HostKind: "ip"})
		if err != nil {
			t.Fatal(err)
		}
		f.userID, f.session = res.UserID, res.SessionSecret
		f.expired = f.login()
		f.st.W.Exec(`UPDATE sessions SET idle_expires_at = 1 WHERE id_hash = ?`, sha(f.expired))
		f.revoked = f.login()
		f.as.RevokeSession(ctx, f.revoked)
		var err2 error
		if f.pending, err2 = f.as.CreatePending(ctx, f.userID, "203.0.113.5"); err2 != nil {
			t.Fatal(err2)
		}
		f.tokens = map[string]string{}
		mk := func(name, scope string) {
			secret, _, err := f.as.CreateToken(ctx, f.userID, name, scope, 30)
			if err != nil {
				t.Fatal(err)
			}
			f.tokens[name] = secret
		}
		mk("token read", "read")
		mk("token write", "write")
		mk("token admin", "admin")
		mk("token expired", "read")
		f.st.W.Exec(`UPDATE api_tokens SET expires_at = ? WHERE name = 'token expired'`, f.clk.Now().Unix()-3600)
		mk("token revoked", "admin")
		f.st.W.Exec(`UPDATE api_tokens SET revoked_at = ? WHERE name = 'token revoked'`, f.clk.Now().Unix()-3600)
	}
	okh := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, 200, map[string]bool{"ok": true}) })
	f.srv = New(Config{
		SPA: fstest.MapFS{"index.html": {Data: []byte(`<html><body>spa</body></html>`)}}, Sessions: f.as,
		Tokens: NewTokenVerifier(f.as), BadTokens: limits.NewKeyed(100000, 100000, 0, nil),
		PanelHosts: []string{"panel.example.com"},
		ExtraRoutes: []Route{
			{Method: "GET", Pattern: "/test/session-or-token", Auth: SessionOrToken, Handler: okh},
			{Method: "POST", Pattern: "/test/session-or-token", Auth: SessionOrToken, Handler: okh},
			{Method: "GET", Pattern: "/test/ws", Auth: Session, Upgrade: true, Handler: okh},
		},
	})
	f.h = f.srv.Handler()
	return f
}

func sha(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// login signs the admin in again (a new session), advancing the clock to a fresh
// TOTP step first.
func (f *fixture) login() string {
	f.t.Helper()
	f.clk.Add(31 * time.Second)
	pending, err := f.as.CreatePending(context.Background(), f.userID, "203.0.113.5")
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := f.as.CompleteLogin(context.Background(), auth.LoginInput{PendingSecret: pending,
		Code: auth.TOTPCode(f.seed, auth.TOTPCounter(f.clk.Now())), IPKey: "203.0.113.5", UserAgent: "test"})
	if err != nil {
		f.t.Fatal(err)
	}
	return res.SessionSecret
}

type mcase struct {
	name   string
	mutate func(f *fixture, r *http.Request)
}

func cookie(r *http.Request, name, value string) { r.AddCookie(&http.Cookie{Name: name, Value: value}) }

func (f *fixture) cases() []mcase {
	csrf := func(r *http.Request) { r.Header.Set(CSRFHeader, CSRFToken(f.session)) }
	origin := func(r *http.Request) { r.Header.Set("Origin", "https://"+r.Host) }
	sess := func(r *http.Request) { cookie(r, SessionCookie, f.session) }
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer sd_"+strings.Repeat("A", 43)) } // well-formed, unknown
	tok := func(name string) func(f *fixture, r *http.Request) {
		return func(f *fixture, r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.tokens[name]) }
	}
	return []mcase{
		{"none", func(f *fixture, r *http.Request) {}},
		{"pending", func(f *fixture, r *http.Request) { cookie(r, PendingCookie, f.pending) }},
		{"session", func(f *fixture, r *http.Request) { sess(r) }},
		{"session+origin", func(f *fixture, r *http.Request) { sess(r); origin(r) }},
		{"session+csrf", func(f *fixture, r *http.Request) { sess(r); csrf(r) }},
		{"session+csrf+origin", func(f *fixture, r *http.Request) { sess(r); csrf(r); origin(r) }},
		{"pending+session+csrf+origin", func(f *fixture, r *http.Request) {
			sess(r)
			cookie(r, PendingCookie, f.pending)
			csrf(r)
			origin(r)
		}},
		{"unknown session cookie", func(f *fixture, r *http.Request) { cookie(r, SessionCookie, "no-such-session") }},
		{"expired session cookie", func(f *fixture, r *http.Request) {
			// An expired session is deleted by the first lookup: make a fresh one each time.
			e := f.login()
			f.st.W.Exec(`UPDATE sessions SET idle_expires_at = 1 WHERE id_hash = ?`, sha(e))
			cookie(r, SessionCookie, e)
		}},
		{"revoked session cookie", func(f *fixture, r *http.Request) { cookie(r, SessionCookie, f.revoked) }},
		{"stale pending cookie", func(f *fixture, r *http.Request) { cookie(r, PendingCookie, "no-such-pending") }},
		{"token read", tok("token read")},
		{"token write", tok("token write")},
		{"token admin", tok("token admin")},
		{"token expired", tok("token expired")},
		{"token revoked", tok("token revoked")},
		{"token unknown", func(f *fixture, r *http.Request) { bearer(r) }},
		{"token in a cookie", func(f *fixture, r *http.Request) {
			cookie(r, SessionCookie, f.tokens["token admin"])
			cookie(r, "token", f.tokens["token admin"])
		}},
		{"bearer garbage", func(f *fixture, r *http.Request) { r.Header.Set("Authorization", "Basic Zm9vOmJhcg==") }},
		{"cookie+bearer", func(f *fixture, r *http.Request) { sess(r); csrf(r); origin(r); bearer(r) }},
		{"token in query", func(f *fixture, r *http.Request) {
			q := r.URL.Query()
			q.Set("token", "sd_"+strings.Repeat("A", 43))
			q.Set("access_token", f.session)
			q.Set("session", f.session)
			r.URL.RawQuery = q.Encode()
		}},
		{"wrong host", func(f *fixture, r *http.Request) { r.Host = "evil.test"; sess(r); csrf(r) }},
		{"look-alike host", func(f *fixture, r *http.Request) { r.Host = "localhost.evil.test"; sess(r); csrf(r) }},
		{"cross-site origin", func(f *fixture, r *http.Request) { sess(r); csrf(r); r.Header.Set("Origin", "https://evil.test") }},
		{"origin null", func(f *fixture, r *http.Request) { sess(r); csrf(r); r.Header.Set("Origin", "null") }},
		{"wrong content type", func(f *fixture, r *http.Request) {
			sess(r)
			csrf(r)
			origin(r)
			r.Header.Set("Content-Type", "text/plain")
		}},
	}
}

// expect is the status the plan prescribes, with the error code ("" = success).
func expect(rt Route, c string) (status int, code string) {
	unsafe := rt.Method == "POST"
	has := func(names ...string) bool {
		for _, n := range names {
			if c == n {
				return true
			}
		}
		return false
	}
	hasAuthz := has("token read", "token write", "token admin", "token expired", "token revoked", "token unknown", "bearer garbage", "cookie+bearer")
	hasValidSession := has("session", "session+origin", "session+csrf", "session+csrf+origin", "pending+session+csrf+origin")
	hasPending := has("pending", "pending+session+csrf+origin")

	if has("wrong host", "look-alike host") {
		return 421, "misdirected"
	}
	if unsafe && has("cross-site origin", "origin null") {
		return 403, "origin"
	}
	if unsafe && strings.HasPrefix(rt.Pattern, "/api/") && c == "wrong content type" {
		return 400, "invalid_input"
	}
	stale := func() (int, string) {
		switch c {
		case "unknown session cookie", "revoked session cookie", "token in a cookie": // a token in a cookie is just an unknown session id
			return 401, "revoked"
		case "expired session cookie":
			return 401, "idle_expired"
		}
		return 401, "unauthenticated"
	}
	sessionRule := func() (int, string) {
		if !hasValidSession && !has("cross-site origin", "origin null", "wrong content type") {
			return stale()
		}
		// A valid session. Unsafe requests (and upgrades) need Origin; unsafe ones also CSRF.
		hasOrigin := has("session+origin", "session+csrf+origin", "pending+session+csrf+origin", "cross-site origin", "origin null", "wrong content type")
		hasCSRF := has("session+csrf", "session+csrf+origin", "pending+session+csrf+origin", "cross-site origin", "origin null", "wrong content type")
		if rt.Upgrade {
			if !hasOrigin {
				return 403, "origin"
			}
			if has("cross-site origin", "origin null") { // a GET: Origin is checked only by the upgrade rule
				return 403, "origin"
			}
			return 200, ""
		}
		if unsafe {
			if !hasOrigin {
				return 403, "origin"
			}
			if !hasCSRF {
				return 403, "csrf"
			}
		}
		return 200, ""
	}
	switch rt.Auth {
	case Public, SetupOpen, DebugSocket:
		return 200, ""
	case Pending2FA:
		if hasPending {
			return 200, ""
		}
		return 401, "unauthenticated"
	case Session:
		if r := hasAuthz; r {
			return 400, "invalid_input"
		}
		return sessionRule()
	case SessionOrToken:
		if hasAuthz {
			switch c {
			case "token read", "token write", "token admin":
				return 200, "" // a valid token: no cookie, CSRF or Origin involved
			case "token expired":
				return 401, "token_expired"
			}
			return 401, "unauthenticated" // revoked, unknown, malformed, or a bad bearer next to a good cookie (cookies are then ignored)
		}
		return sessionRule()
	}
	panic("unreachable")
}

// known is every route this matrix covers. A route in the table that is missing
// here fails TestEveryRouteIsInTheMatrix: extend this file with the new route.
func known() map[string]AuthKind {
	return map[string]AuthKind{
		"GET /healthz":                            Public,
		"GET /":                                   Public,
		"GET /agent/":                             Public,
		"GET /api/v1/setup/status":                Public,
		"POST /api/v1/setup/enroll":               SetupOpen,
		"POST /api/v1/setup/complete":             SetupOpen,
		"POST /api/v1/auth/login":                 Public,
		"POST /api/v1/auth/2fa":                   Pending2FA,
		"GET /api/v1/auth/session":                Session,
		"POST /api/v1/auth/reauth":                Session,
		"POST /api/v1/auth/logout":                Session,
		"GET /debug/mem":                          DebugSocket,
		"GET /debug/agents":                       DebugSocket,
		"GET /debug/agents/{id}/logs/{container}": DebugSocket,
		"GET /test/session-or-token":              SessionOrToken,
		"POST /test/session-or-token":             SessionOrToken,
		"GET /test/ws":                            Session,
	}
}

func TestEveryRouteIsInTheMatrix(t *testing.T) {
	f := newFixture(t, false)
	k := known()
	seen := map[string]bool{}
	for _, rt := range f.srv.Routes() {
		key := rt.Method + " " + rt.Pattern
		seen[key] = true
		want, ok := k[key]
		if !ok {
			t.Errorf("route %q is in the table but not in the route matrix", key)
		} else if want != rt.Auth {
			t.Errorf("route %q has AuthKind %v, the matrix expects %v", key, rt.Auth, want)
		}
		if rt.Handler == nil {
			t.Errorf("route %q has no handler", key)
		}
	}
	for key := range k {
		if !seen[key] {
			t.Errorf("the matrix lists %q but the table has no such route", key)
		}
	}
}

func TestRouteMatrix(t *testing.T) {
	f := newFixture(t, true)
	for _, rt := range f.srv.Routes() {
		if rt.Auth == DebugSocket {
			continue
		}
		path := strings.NewReplacer("{name}", "x").Replace(rt.Pattern)
		if path == "/" || strings.HasSuffix(path, "/") && path != "/" {
			// "/" and "/agent/" are prefix patterns: ask for a concrete path under them.
			path = strings.TrimSuffix(path, "/") + "/p"
			if rt.Pattern == "/" {
				path = "/"
			}
		}
		for _, c := range f.cases() {
			t.Run(rt.Method+" "+rt.Pattern+" / "+c.name, func(t *testing.T) {
				var body *strings.Reader
				if rt.Method == "POST" {
					body = strings.NewReader(`{}`)
				} else {
					body = strings.NewReader("")
				}
				r := httptest.NewRequest(rt.Method, path, body)
				r.Host = "127.0.0.1:8443"
				if rt.Method == "POST" {
					r.Header.Set("Content-Type", "application/json")
				}
				c.mutate(f, r)
				rec := httptest.NewRecorder()
				f.h.ServeHTTP(rec, r)
				wantStatus, wantCode := expect(rt, c.name)
				// Prefix routes with no real content: the SPA root is 200, /agent/... is 404.
				if rt.Pattern == "/agent/" && wantStatus == 200 {
					wantStatus = 404
				}
				if rec.Code != wantStatus {
					t.Fatalf("status %d (%s), want %d (%s)", rec.Code, rec.Body.String(), wantStatus, wantCode)
				}
				if wantCode != "" && !strings.Contains(rec.Body.String(), `"code":"`+wantCode+`"`) {
					t.Fatalf("body %s, want code %s", rec.Body.String(), wantCode)
				}
				// A stale session cookie is cleared where it counts as "no credential" on
				// a session route, and nowhere else.
				cleared := false
				for _, sc := range rec.Result().Cookies() {
					if sc.Name == SessionCookie && sc.MaxAge < 0 {
						cleared = true
					}
				}
				staleCookie := strings.HasSuffix(c.name, "session cookie") && c.name != "pending+session+csrf+origin"
				onSessionRoute := rt.Auth == Session || rt.Auth == SessionOrToken
				if wantCode == "revoked" || wantCode == "idle_expired" {
					if !cleared {
						t.Errorf("a stale session cookie was not cleared")
					}
				} else if cleared {
					t.Errorf("a session cookie was cleared on a request where it was not stale (%s)", c.name)
				}
				_ = staleCookie
				_ = onSessionRoute
				// Every response carries the security headers.
				for h, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "same-origin",
					"Cross-Origin-Opener-Policy": "same-origin", "Cross-Origin-Resource-Policy": "same-origin"} {
					if rec.Header().Get(h) != v {
						t.Errorf("%s = %q", h, rec.Header().Get(h))
					}
				}
				if strings.HasPrefix(path, "/api/") && rec.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("an /api response without no-store")
				}
			})
		}
	}
}

// A session cookie that is stale is NOT cleared on a Public route (it is simply
// not used), and a valid one is never touched.
func TestStaleCookieOnPublicRouteIsIgnored(t *testing.T) {
	f := newFixture(t, true)
	for _, c := range []string{f.expired, f.revoked, "junk", f.session} {
		r := httptest.NewRequest("GET", "/healthz", nil)
		r.Host = "127.0.0.1"
		cookie(r, SessionCookie, c)
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		if rec.Code != 200 || len(rec.Result().Cookies()) != 0 {
			t.Errorf("%q: %d, cookies %v", c, rec.Code, rec.Result().Cookies())
		}
	}
}

func TestHostAllowlist(t *testing.T) {
	f := newFixture(t, false)
	ok := []string{"127.0.0.1", "127.0.0.1:8443", "203.0.113.9", "[::1]", "[::1]:443", "[2001:db8::1]:8443", "localhost", "LOCALHOST:8443", "localhost:1",
		"panel.example.com", "Panel.Example.Com:8443"}
	bad := []string{"", "evil.test", "localhost.evil.test", "evil.test:443", "panel.example.com.evil.test", "xpanel.example.com", "example.com",
		"::1", "2001:db8::1", "[::1", "[::1]x", "[fe80::1%eth0]", "1.2.3", "localhost.", "0x7f.1", "999.1.1.1:80", "a b"}
	for _, h := range ok {
		if !f.srv.hostAllowed(h) {
			t.Errorf("Host %q refused", h)
		}
	}
	for _, h := range bad {
		if f.srv.hostAllowed(h) {
			t.Errorf("Host %q accepted", h)
		}
	}
	// Through the handler: 421 with the standard envelope, and nothing is served.
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Host = "localhost.evil.test"
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != 421 || !strings.Contains(rec.Body.String(), "misdirected") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// Every DebugSocket route is 404 on the control plane, and every other route is
// 404 on the debug socket's handler.
func TestDebugRoutesAreOnlyOnTheDebugSocket(t *testing.T) {
	f := newFixture(t, false)
	debug := f.srv.DebugHandler()
	for _, rt := range f.srv.Routes() {
		path := strings.NewReplacer("{id}", "a", "{container}", "b", "{name}", "x").Replace(rt.Pattern)
		if strings.HasSuffix(path, "/") && path != "/" {
			path += "p"
		}
		for _, side := range []struct {
			name string
			h    http.Handler
			want bool // served here?
		}{{"control", f.h, rt.Auth != DebugSocket}, {"debug", debug, rt.Auth == DebugSocket}} {
			r := httptest.NewRequest(rt.Method, path, strings.NewReader(`{}`))
			r.Host = "127.0.0.1"
			r.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			side.h.ServeHTTP(rec, r)
			served := rec.Code != 404 && rec.Code != 405
			if rt.Pattern == "/agent/" {
				served = false // always 404
				side.want = false
			}
			// The SPA root answers 200 on the control plane; unauthenticated session
			// routes answer 401; on the debug handler anything not debug is 404.
			if side.name == "debug" && rt.Auth != DebugSocket && rec.Code != 404 && rec.Code != 405 {
				t.Errorf("%s %s answered %d on the debug socket", rt.Method, path, rec.Code)
			}
			if side.name == "control" && rt.Auth == DebugSocket && served {
				t.Errorf("debug route %s answered %d on the control plane", path, rec.Code)
			}
			_ = served
		}
	}
}

// The mux is built only from the table: no other file of this package, and no
// file of the command except the agent listener's own mux, registers a route.
func TestNoMuxRegistrationOutsideTheRouteTable(t *testing.T) {
	check := func(dir string, allow map[string]int) {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		fset := token.NewFileSet()
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			ast.Inspect(file, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") {
						n++
					}
				}
				return true
			})
			if n != allow[filepath.Base(f)] {
				t.Errorf("%s registers %d routes outside the table, want %d", f, n, allow[filepath.Base(f)])
			}
		}
	}
	// routes.go builds the muxes from the table (two Handle calls in loops).
	check(".", map[string]int{"routes.go": 2})
	// cmd/serverdock: only the agent listener's GET /agent/connect.
	check("../../cmd/serverdock", map[string]int{"server.go": 1})
}

func TestCSPHashesMatchTheEmbeddedIndex(t *testing.T) {
	html := `<!doctype html><html><head><script>window.a=1</script><script src="/x.js"></script>
<script type="module">import("/y.js")</script></head><body></body></html>`
	hashes := inlineScriptHashes(html)
	if len(hashes) != 2 {
		t.Fatalf("%v", hashes)
	}
	want1 := sha256.Sum256([]byte("window.a=1"))
	if !strings.HasSuffix(hashes[0], "") || hashes[0] != "sha256-"+b64(want1[:]) {
		t.Errorf("first hash %s", hashes[0])
	}
	csp := CSP(hashes)
	for _, want := range []string{"default-src 'none'", "script-src 'self' '" + hashes[0] + "' '" + hashes[1] + "'", "style-src 'self'", "img-src 'self' data:",
		"font-src 'self'", "connect-src 'self'", "base-uri 'none'", "form-action 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	// Served with the HTML, computed from the file actually embedded.
	spa := fstest.MapFS{"index.html": {Data: []byte(html)}}
	srv := New(Config{SPA: spa})
	for _, p := range []string{"/", "/some/route", "/index.html"} {
		rec := get(t, srv.Handler(), "GET", p)
		if got := rec.Header().Get("Content-Security-Policy"); got != csp {
			t.Errorf("%s: CSP %q", p, got)
		}
	}
	// JSON and other responses do not carry it.
	if got := get(t, srv.Handler(), "GET", "/healthz").Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("healthz CSP %q", got)
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func decodeSecret(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type countingVerifier struct{ calls atomic.Int32 }

func (c *countingVerifier) VerifyToken(context.Context, string) (TokenInfo, error) {
	c.calls.Add(1)
	return TokenInfo{}, errors.New("no")
}

// The 11th bad token from one IP in a minute is a 429 BEFORE any lookup; another
// IP is unaffected; the bucket refills.
func TestBadTokenBucketStopsGuessingBeforeTheLookup(t *testing.T) {
	cv := &countingVerifier{}
	clk := &mclock{t: time.Unix(1_800_000_000, 0)}
	srv := New(Config{SPA: fstest.MapFS{"index.html": {Data: []byte("x")}}, Tokens: cv,
		BadTokens: limits.NewKeyed(5, 10, 0, clk.Now),
		ExtraRoutes: []Route{{Method: "GET", Pattern: "/test/t", Auth: SessionOrToken,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, 200, map[string]bool{"ok": true}) })}}})
	h := srv.Handler()
	call := func(ip string) (int, string) {
		r := httptest.NewRequest("GET", "/test/t", nil)
		r.Host, r.RemoteAddr = "127.0.0.1", ip+":1000"
		r.Header.Set("Authorization", "Bearer sd_"+strings.Repeat("B", 43))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, rec.Header().Get("Retry-After")
	}
	for i := 0; i < 10; i++ {
		if code, _ := call("198.51.100.1"); code != 401 {
			t.Fatalf("bad token %d: %d", i+1, code)
		}
	}
	if cv.calls.Load() != 10 {
		t.Fatalf("%d lookups", cv.calls.Load())
	}
	code, retry := call("198.51.100.1")
	if code != 429 || retry == "" || cv.calls.Load() != 10 {
		t.Fatalf("the 11th bad token: %d (Retry-After %q), %d lookups", code, retry, cv.calls.Load())
	}
	if code, _ := call("198.51.100.2"); code != 401 || cv.calls.Load() != 11 {
		t.Fatalf("another IP: %d, %d lookups", code, cv.calls.Load())
	}
	clk.Add(20 * time.Second) // 5 per minute: one token back after 12 s
	if code, _ := call("198.51.100.1"); code != 401 {
		t.Fatalf("after the bucket refilled: %d", code)
	}
	// A malformed Authorization never reaches the verifier or the bucket.
	r := httptest.NewRequest("GET", "/test/t", nil)
	r.Host, r.RemoteAddr = "127.0.0.1", "198.51.100.3:1"
	r.Header.Set("Authorization", "Basic abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 401 || cv.calls.Load() != 12 {
		t.Errorf("%d, %d lookups", rec.Code, cv.calls.Load())
	}
}

// With the failed-token map full of blocked keys, a valid token from a NEW address
// still works (an untracked key is looked up and charged only on failure), while
// a tracked, exhausted address stays blocked before the lookup.
func TestValidTokenFromANewIPWorksWhenTheBadTokenMapIsFull(t *testing.T) {
	f := newFixture(t, true)
	lim := limits.NewKeyed(5, 10, 0, nil)
	for i := 0; i < limits.MaxKeys; i++ { // every slot held by an exhausted key
		for j := 0; j < 12; j++ {
			lim.Take(fmt.Sprintf("flood%d", i))
		}
	}
	srv := New(Config{SPA: fstest.MapFS{"index.html": {Data: []byte("x")}}, Tokens: NewTokenVerifier(f.as), BadTokens: lim,
		ExtraRoutes: []Route{{Method: "GET", Pattern: "/test/t", Auth: SessionOrToken,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, 200, map[string]bool{"ok": true}) })}}})
	call := func(ip, token string) int {
		r := httptest.NewRequest("GET", "/test/t", nil)
		r.Host, r.RemoteAddr = "127.0.0.1", ip+":1000"
		r.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, r)
		return rec.Code
	}
	if code := call("203.0.113.50", f.tokens["token read"]); code != 200 {
		t.Fatalf("a valid token from a new IP with the map full: %d", code)
	}
	bad := "sd_" + strings.Repeat("C", 43)
	for i := 0; i < 3; i++ { // bad tokens from a new IP are looked up (401), not blocked (429)
		if code := call("203.0.113.51", bad); code != 401 {
			t.Fatalf("a bad token from a new IP: %d", code)
		}
	}
	// A tracked address that failed enough is blocked before the lookup.
	for i := 0; i < 12; i++ {
		lim.Take("203.0.113.52")
	}
	if code := call("203.0.113.52", f.tokens["token read"]); code != 429 && code != 200 {
		t.Fatalf("%d", code)
	}
}
