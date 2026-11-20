package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/secretbox"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// countingHasher stands in for argon2 at the HTTP layer: a password is right when
// it equals goodPassword, and every call is counted (the order of checks and the
// cost of unknown users are asserted through the counts).
type countingHasher struct{ verifies, dummies atomic.Int32 }

func (h *countingHasher) Verify(_ context.Context, _ string, pw string) (bool, error) {
	h.verifies.Add(1)
	return pw == testPassword, nil
}
func (h *countingHasher) VerifyDummy(context.Context, string) error { h.dummies.Add(1); return nil }
func (h *countingHasher) total() int32                              { return h.verifies.Load() + h.dummies.Load() }

// failingAudit is an audit sink whose writes fail on demand.
type failingAudit struct {
	*audit.Logger
	fail atomic.Bool
}

func (f *failingAudit) Append(ctx context.Context, e audit.Entry) (int64, error) {
	if f.fail.Load() {
		return 0, errors.New("disk full")
	}
	return f.Logger.Append(ctx, e)
}

type flow struct {
	t      *testing.T
	dir    string
	st     *store.Store
	as     *auth.Store
	clk    *mclock
	lim    *Limits
	hasher *countingHasher
	aud    *failingAudit
	logs   *lockedBuffer
	srv    *Server
	h      http.Handler
	seed   []byte
	userID int64
	codes  []string
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func newFlow(t *testing.T) *flow {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secretbox.New(make([]byte, 32))
	f := &flow{t: t, dir: dir, st: st, clk: &mclock{t: time.Unix(1_800_000_000, 0)}, hasher: &countingHasher{}, logs: &lockedBuffer{}}
	log := slog.New(slog.NewTextHandler(f.logs, nil))
	f.as = auth.NewStore(st, box, auth.NewHasher(), auth.Options{Now: f.clk.Now, Log: log})
	al, err := audit.New(st, log, audit.WithClock(f.clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	f.aud = &failingAudit{Logger: al}
	f.lim = NewLimits(f.clk.Now)
	f.srv = New(Config{
		SPA: fstest.MapFS{"index.html": {Data: []byte("<html></html>")}}, Sessions: f.as, PanelHosts: []string{"panel.example.com"}, Log: log,
		Auth: NewAuthHandlers(AuthDeps{Auth: f.as, Hasher: f.hasher, Audit: f.aud, Limits: f.lim, Log: log}),
	})
	f.h = f.srv.Handler()
	if _, err := f.as.PrepareSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *flow) token() string {
	b, err := os.ReadFile(filepath.Join(f.dir, auth.SetupTokenFile))
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// client is a browser: it keeps cookies and the CSRF token.
type client struct {
	f       *flow
	remote  string
	cookies map[string]string
	csrf    string
}

func (f *flow) client(remote string) *client {
	return &client{f: f, remote: remote + ":4000", cookies: map[string]string{}}
}

type resp struct {
	Code   int
	Body   map[string]any
	Raw    string
	Header http.Header
}

func (c *client) do(method, path string, body any, mut ...func(*http.Request)) resp {
	c.f.t.Helper()
	var rd io.Reader = strings.NewReader("")
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Host, r.RemoteAddr = "127.0.0.1:8443", c.remote
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://"+r.Host)
		if c.csrf != "" {
			r.Header.Set(CSRFHeader, c.csrf)
		}
	}
	for n, v := range c.cookies {
		r.AddCookie(&http.Cookie{Name: n, Value: v})
	}
	for _, m := range mut {
		m(r)
	}
	rec := httptest.NewRecorder()
	c.f.h.ServeHTTP(rec, r)
	for _, sc := range rec.Result().Cookies() {
		if sc.MaxAge < 0 {
			delete(c.cookies, sc.Name)
		} else {
			c.cookies[sc.Name] = sc.Value
		}
	}
	out := resp{Code: rec.Code, Raw: rec.Body.String(), Header: rec.Header()}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.Body)
	if v, ok := out.Body["csrf"].(string); ok {
		c.csrf = v
	}
	return out
}

func (r resp) code() string {
	if e, ok := r.Body["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func (f *flow) code() string {
	return auth.TOTPCode(f.seed, auth.TOTPCounter(f.clk.Now()))
}

func (f *flow) nextCode() string { f.clk.Add(31 * time.Second); return f.code() }

// setup completes first run over HTTP and returns the signed-in client.
func (f *flow) setup() *client {
	f.t.Helper()
	c := f.client("203.0.113.5")
	tok := f.token()
	en := c.do("POST", "/api/v1/setup/enroll", map[string]string{"token": tok, "username": "admin"})
	if en.Code != 200 {
		f.t.Fatalf("enroll: %d %s", en.Code, en.Raw)
	}
	f.seed = decodeSecret(f.t, en.Body["secret"].(string))
	done := c.do("POST", "/api/v1/setup/complete", map[string]string{"token": tok, "enrollment_id": en.Body["enrollment_id"].(string),
		"username": "admin", "password": testPassword, "code": f.code()})
	if done.Code != 200 {
		f.t.Fatalf("complete: %d %s", done.Code, done.Raw)
	}
	for _, rc := range done.Body["recovery_codes"].([]any) {
		f.codes = append(f.codes, rc.(string))
	}
	u, _, _ := f.as.UserByName(context.Background(), "admin")
	f.userID = u.ID
	return c
}

// signIn runs password + 2FA from a fresh client at remote.
func (f *flow) signIn(remote string) (*client, resp) {
	c := f.client(remote)
	l := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword})
	if l.Code != 200 {
		f.t.Fatalf("login: %d %s", l.Code, l.Raw)
	}
	return c, c.do("POST", "/api/v1/auth/2fa", map[string]string{"code": f.nextCode()})
}

func (f *flow) auditRows(action, result string) []map[string]any {
	f.t.Helper()
	rows, err := f.st.R.Query(`SELECT actor_kind, actor_id, target, args_json, phase FROM audit_log WHERE action = ? AND result = ? ORDER BY id`, action, result)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var k, id, target, args, phase string
		rows.Scan(&k, &id, &target, &args, &phase)
		out = append(out, map[string]any{"kind": k, "actor": id, "target": target, "args": args, "phase": phase})
	}
	return out
}

func (f *flow) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.st.R.QueryRow(q, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// ---- setup over HTTP --------------------------------------------------------------

func TestFirstRunOverHTTP(t *testing.T) {
	f := newFlow(t)
	st := f.client("203.0.113.5").do("GET", "/api/v1/setup/status", nil)
	if st.Code != 200 || st.Body["setup_required"] != true || st.Body["token_file"] != filepath.Join(f.dir, "setup-token") || st.Body["token_expires_at"] == nil || st.Body["server_time"] == nil {
		t.Fatalf("%d %s", st.Code, st.Raw)
	}
	c := f.setup()
	if c.cookies[SessionCookie] == "" || c.csrf == "" {
		t.Fatal("setup did not sign the admin in")
	}
	info := c.do("GET", "/api/v1/auth/session", nil)
	if info.Code != 200 || info.Body["csrf"] != c.csrf || info.Body["user"].(map[string]any)["username"] != "admin" || info.Body["recovery_codes_left"] != float64(10) {
		t.Fatalf("%d %s", info.Code, info.Raw)
	}
	if out := c.do("POST", "/api/v1/auth/logout", map[string]string{}); out.Code != 200 || c.cookies[SessionCookie] != "" {
		t.Fatalf("logout: %d %s", out.Code, out.Raw)
	}
	if got := c.do("GET", "/api/v1/auth/session", nil); got.Code != 401 {
		t.Fatalf("after logout: %d", got.Code)
	}
	// Setup endpoints are 404 once an admin exists; status says so, and nothing more.
	for _, ep := range []string{"enroll", "complete"} {
		if r := c.do("POST", "/api/v1/setup/"+ep, map[string]string{"token": f.token0(), "username": "admin"}); r.Code != 404 {
			t.Errorf("setup/%s with an admin: %d", ep, r.Code)
		}
	}
	st = c.do("GET", "/api/v1/setup/status", nil)
	if st.Body["setup_required"] != false || st.Body["token_file"] != nil || st.Body["token_expires_at"] != nil || len(st.Body) != 2 {
		t.Fatalf("status after setup: %s", st.Raw)
	}
	if _, err := os.Stat(filepath.Join(f.dir, auth.SetupTokenFile)); err == nil {
		t.Error("the token file is still there")
	}
}

func (f *flow) token0() string { return "sd_setup_" + strings.Repeat("x", 43) }

func TestSetupStatusNeverReturnsFingerprint(t *testing.T) {
	f := newFlow(t)
	r := f.client("203.0.113.5").do("GET", "/api/v1/setup/status", nil)
	low := strings.ToLower(strings.ReplaceAll(r.Raw, f.dir, "")) // the temp dir's name contains the test's name
	for _, bad := range []string{"fingerprint", "sha256", "cert"} {
		if strings.Contains(low, bad) {
			t.Errorf("the unauthenticated status mentions %q: %s", bad, r.Raw)
		}
	}
	for _, k := range []string{"setup_required", "token_expires_at", "token_file", "server_time"} {
		if _, ok := r.Body[k]; !ok {
			t.Errorf("status lacks %s", k)
		}
	}
}

func TestSetupRefusesATokenInTheURLAndBadInput(t *testing.T) {
	f := newFlow(t)
	c := f.client("203.0.113.5")
	tok := f.token()
	for _, p := range []string{"/api/v1/setup/enroll?token=" + tok, "/api/v1/setup/complete?token=" + tok, "/api/v1/setup/enroll?setup_token=x"} {
		if r := c.do("POST", p, map[string]string{"token": tok, "username": "admin"}); r.Code != 400 {
			t.Errorf("%s: %d", p, r.Code)
		}
	}
	for name, body := range map[string]any{
		"unknown field":     map[string]string{"token": tok, "username": "admin", "extra": "x"},
		"bad username":      map[string]string{"token": tok, "username": "a b"},
		"empty":             map[string]string{},
		"not an object":     []int{1},
		"username too long": map[string]string{"token": tok, "username": strings.Repeat("a", 80)},
	} {
		if r := c.do("POST", "/api/v1/setup/enroll", body); r.Code != 400 {
			t.Errorf("%s: %d %s", name, r.Code, r.Raw)
		}
	}
	// Trailing data and an oversized body are refused without echoing the body.
	r := c.do("POST", "/api/v1/setup/enroll", nil, func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(`{"token":"SECRETVALUE","username":"admin"} {}`))
	})
	if r.Code != 400 || strings.Contains(r.Raw, "SECRETVALUE") {
		t.Errorf("trailing data: %d %s", r.Code, r.Raw)
	}
	big := `{"token":"` + strings.Repeat("x", 100<<10) + `"}`
	if r := c.do("POST", "/api/v1/setup/enroll", nil, func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(big)) }); r.Code != 400 {
		t.Errorf("oversized body: %d", r.Code)
	}
}

func TestSetupWrongTokenIsCountedAndAuditedAsAnonymous(t *testing.T) {
	f := newFlow(t)
	c := f.client("203.0.113.5")
	for i := 0; i < 6; i++ {
		if r := c.do("POST", "/api/v1/setup/enroll", map[string]string{"token": f.token0(), "username": "admin"}); r.Code != 401 || r.code() != "invalid_token" {
			t.Fatalf("attempt %d: %d %s", i, r.Code, r.Raw)
		}
	}
	// 5 free failures, then the per-IP back-off blocks (even the right token).
	if r := c.do("POST", "/api/v1/setup/enroll", map[string]string{"token": f.token(), "username": "admin"}); r.Code != 429 || r.Header.Get("Retry-After") == "" || r.code() != "rate_limited" {
		t.Fatalf("after 6 wrong tokens: %d %s", r.Code, r.Raw)
	}
	rows := f.auditRows("auth.setup", "denied")
	if len(rows) == 0 || rows[0]["kind"] != "anonymous" || rows[0]["actor"] != "anonymous:auth.setup" {
		t.Fatalf("denied rows: %v", rows)
	}
	for _, row := range rows {
		if strings.Contains(row["args"].(string), "sd_setup_") {
			t.Error("a token in an audit row")
		}
	}
	// Another network is not blocked.
	if r := f.client("198.51.100.7").do("POST", "/api/v1/setup/enroll", map[string]string{"token": f.token(), "username": "admin"}); r.Code != 200 {
		t.Errorf("another IP: %d %s", r.Code, r.Raw)
	}
}

func TestAdminCreatedLoggedAndStored(t *testing.T) {
	f := newFlow(t)
	f.setup()
	var ip, host string
	var at int64
	f.st.R.QueryRow(`SELECT setup_ip, setup_at, totp_enrolled_host FROM users`).Scan(&ip, &at, &host)
	if ip != "203.0.113.5" || at != f.clk.Now().Unix() || host != "ip" {
		t.Fatalf("stored: %q %d %q", ip, at, host)
	}
	out := f.logs.String()
	if !strings.Contains(out, "admin account created") || !strings.Contains(out, "from=203.0.113.5") || !strings.Contains(out, "username=admin") {
		t.Errorf("log: %s", out)
	}
	for _, secret := range []string{testPassword, f.seedText(), "sd_setup_"} {
		if strings.Contains(out, secret) {
			t.Errorf("a secret in the log: %q", secret)
		}
	}
}

func (f *flow) seedText() string { return auth.TOTPSecret(f.seed) }

// ---- sign in -----------------------------------------------------------------------

func TestLoginTwoStepAndSessionRotation(t *testing.T) {
	f := newFlow(t)
	f.setup()
	c := f.client("203.0.113.5")
	// A pending cookie is not a session.
	l := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "ADMIN", "password": testPassword}) // user names are case-insensitive
	if l.Code != 200 || l.Body["two_factor_required"] != true || c.cookies[PendingCookie] == "" || c.cookies[SessionCookie] != "" {
		t.Fatalf("%d %s", l.Code, l.Raw)
	}
	if r := c.do("GET", "/api/v1/auth/session", nil); r.Code != 401 {
		t.Fatalf("TestPendingIsNotASession: a pending cookie opened the session route: %d", r.Code)
	}
	pending := c.cookies[PendingCookie]
	r := c.do("POST", "/api/v1/auth/2fa", map[string]string{"code": f.nextCode()})
	if r.Code != 200 || c.cookies[SessionCookie] == "" || c.cookies[PendingCookie] != "" {
		t.Fatalf("2fa: %d %s", r.Code, r.Raw)
	}
	if c.cookies[SessionCookie] == pending {
		t.Fatal("the session id is the pending id")
	}
	if got := c.do("GET", "/api/v1/auth/session", nil); got.Code != 200 {
		t.Fatalf("the new session does not work: %d", got.Code)
	}
	// The pending cookie is single use.
	c2 := f.client("203.0.113.5")
	c2.cookies[PendingCookie] = pending
	if r := c2.do("POST", "/api/v1/auth/2fa", map[string]string{"code": f.nextCode()}); r.Code != 401 {
		t.Errorf("a used pending cookie: %d", r.Code)
	}
}

func TestOldSessionRevokedAtLoginAndClientChosenIDRefused(t *testing.T) {
	f := newFlow(t)
	old := f.setup()
	oldSecret := old.cookies[SessionCookie]
	// Sign in again, presenting the old session cookie on both requests.
	c := f.client("203.0.113.5")
	c.cookies[SessionCookie] = oldSecret
	if l := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); l.Code != 200 {
		t.Fatalf("%d %s", l.Code, l.Raw)
	}
	// The old session was revoked at login (before anything was minted).
	if r := old.do("GET", "/api/v1/auth/session", nil); r.Code != 401 {
		t.Errorf("the old session survived the login: %d", r.Code)
	}
	if c.cookies[SessionCookie] != "" {
		t.Error("the old cookie was not cleared at login")
	}
	// A client-chosen session id is never adopted: 2FA mints its own.
	c.cookies[SessionCookie] = "attacker-chosen-session-id"
	r := c.do("POST", "/api/v1/auth/2fa", map[string]string{"code": f.nextCode()})
	if r.Code != 200 || c.cookies[SessionCookie] == "attacker-chosen-session-id" || c.cookies[SessionCookie] == "" {
		t.Fatalf("%d, session cookie %q", r.Code, c.cookies[SessionCookie])
	}
	other := f.client("203.0.113.5")
	other.cookies[SessionCookie] = "attacker-chosen-session-id"
	if g := other.do("GET", "/api/v1/auth/session", nil); g.Code != 401 {
		t.Errorf("a client-chosen id authenticated: %d", g.Code)
	}
}

// A valid session cookie presented with the 2FA step is ignored for
// authentication and revoked when the new session is minted.
func TestPendingPlusValidSessionOn2FA(t *testing.T) {
	f := newFlow(t)
	old := f.setup()
	c := f.client("203.0.113.5")
	if l := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); l.Code != 200 {
		t.Fatal(l.Raw)
	}
	c.cookies[SessionCookie] = old.cookies[SessionCookie] // login cleared none: it was not presented
	if r := c.do("POST", "/api/v1/auth/2fa", map[string]string{"code": f.nextCode()}); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Raw)
	}
	if r := old.do("GET", "/api/v1/auth/session", nil); r.Code != 401 {
		t.Errorf("the old session survived: %d", r.Code)
	}
	if c.cookies[SessionCookie] == old.cookies[SessionCookie] {
		t.Error("the old session id was reused")
	}
}

// A stale session cookie on login, 2FA and setup/complete is ignored, then cleared.
func TestStaleSessionCookieOnPreSessionEndpoints(t *testing.T) {
	f := newFlow(t)
	f.setup()
	for _, stale := range []string{"unknown", "expired"} {
		secret := "no-such-session"
		if stale == "expired" {
			c, _ := f.signIn("203.0.113.9")
			secret = c.cookies[SessionCookie]
			f.st.W.Exec(`UPDATE sessions SET idle_expires_at = 1 WHERE user_agent = ''`) // all sessions made by httptest have no UA
		}
		c := f.client("203.0.113.5")
		c.cookies[SessionCookie] = secret
		l := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "wrong password!!"})
		if l.Code != 401 || c.cookies[SessionCookie] != "" {
			t.Errorf("%s on login: %d, cookie %q", stale, l.Code, c.cookies[SessionCookie])
		}
		c.cookies[SessionCookie] = secret
		c.cookies[PendingCookie] = "no-such-pending"
		r := c.do("POST", "/api/v1/auth/2fa", map[string]string{"code": "123456"})
		if r.Code != 401 || c.cookies[PendingCookie] != "" {
			t.Errorf("%s on 2fa: %d", stale, r.Code)
		}
	}
	// setup/complete with an admin: 404, and a stale cookie is not a credential.
	c := f.client("203.0.113.5")
	c.cookies[SessionCookie] = "no-such-session"
	if r := c.do("POST", "/api/v1/setup/complete", map[string]string{"token": "x"}); r.Code != 404 {
		t.Errorf("setup/complete: %d", r.Code)
	}
	// And on a fresh server, where setup/complete is live.
	g := newFlow(t)
	tok := g.token()
	sc := g.client("203.0.113.5")
	en := sc.do("POST", "/api/v1/setup/enroll", map[string]string{"token": tok, "username": "admin"})
	g.seed = decodeSecret(t, en.Body["secret"].(string))
	sc.cookies[SessionCookie] = "no-such-session"
	done := sc.do("POST", "/api/v1/setup/complete", map[string]string{"token": tok, "enrollment_id": en.Body["enrollment_id"].(string),
		"username": "admin", "password": testPassword, "code": g.code()})
	if done.Code != 200 || sc.cookies[SessionCookie] == "" || sc.cookies[SessionCookie] == "no-such-session" {
		t.Fatalf("setup with a stale cookie: %d %s", done.Code, done.Raw)
	}
}

func TestCrossSiteOriginIsRefusedOnEveryPreSessionPost(t *testing.T) {
	f := newFlow(t)
	f.setup()
	evil := func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }
	c := f.client("203.0.113.5")
	tok := "sd_setup_" + strings.Repeat("x", 43)
	for path, body := range map[string]any{
		"/api/v1/setup/enroll":   map[string]string{"token": tok, "username": "admin"},
		"/api/v1/setup/complete": map[string]string{"token": tok},
		"/api/v1/auth/login":     map[string]string{"username": "admin", "password": testPassword},
		"/api/v1/auth/2fa":       map[string]string{"code": "123456"},
	} {
		if r := c.do("POST", path, body, evil); r.Code != 403 || r.code() != "origin" {
			t.Errorf("%s with a cross-site Origin: %d %s", path, r.Code, r.Raw)
		}
	}
	if f.hasher.total() != 0 {
		t.Error("a cross-site request reached the hasher")
	}
	if c.cookies[PendingCookie] != "" {
		t.Error("a cross-site login minted a pending cookie")
	}
}

// ---- back-off wiring ------------------------------------------------------------------

func TestBackoffCheckedBeforeHash(t *testing.T) {
	f := newFlow(t)
	f.setup()
	c := f.client("198.51.100.20")
	bad := map[string]string{"username": "admin", "password": "wrong password!!"}
	for i := 0; i < 5; i++ { // the 5 free failures hash
		if r := c.do("POST", "/api/v1/auth/login", bad); r.Code != 401 {
			t.Fatalf("%d", r.Code)
		}
	}
	before := f.hasher.total()
	if r := c.do("POST", "/api/v1/auth/login", bad); r.Code != 401 { // the 6th failure hashes and starts the block
		t.Fatalf("%d", r.Code)
	}
	before = f.hasher.total()
	for i := 0; i < 5; i++ { // blocked: 429 without any hashing, even for the right password
		r := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword})
		if r.Code != 429 || r.Header.Get("Retry-After") == "" {
			t.Fatalf("blocked attempt: %d %s", r.Code, r.Raw)
		}
	}
	if f.hasher.total() != before {
		t.Errorf("%d hashes ran while the IP was blocked", f.hasher.total()-before)
	}
	// After the wait, the right password works again.
	f.clk.Add(10 * time.Second)
	if r := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); r.Code != 200 {
		t.Fatalf("after the wait: %d %s", r.Code, r.Raw)
	}
}

func TestBackoffInstancesIndependentOverHTTP(t *testing.T) {
	f := newFlow(t)
	f.setup()
	// Block one IP (ip64/ip48); the account and other IPs are untouched.
	c := f.client("198.51.100.30")
	for i := 0; i < 7; i++ {
		c.do("POST", "/api/v1/auth/login", map[string]string{"username": "nobody", "password": "x"})
	}
	if r := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); r.Code != 429 {
		t.Fatalf("the blocked IP: %d", r.Code)
	}
	if r := f.client("198.51.100.31").do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); r.Code != 200 {
		t.Fatalf("another IP was affected: %d %s", r.Code, r.Raw)
	}
	if f.lim.Account2FA.Len() != 0 {
		t.Error("the 2FA limiter was touched by password failures")
	}
	// The /48 limiter stops an attacker rotating /64s inside one /48. Wrong setup
	// tokens feed only the per-IP limiters (not an account bucket), so 25 of them,
	// each from another /64 of one /48, exhaust the 20 free /48 failures.
	g := newFlow(t)
	for i := 0; i < 25; i++ {
		g.client(fmt.Sprintf("[2001:db8:1:%x::1]", i)).do("POST", "/api/v1/setup/enroll", map[string]string{"token": g.token0(), "username": "admin"})
	}
	good := map[string]string{"token": g.token(), "username": "admin"}
	if r := g.client("[2001:db8:1:ffff::1]").do("POST", "/api/v1/setup/enroll", good); r.Code != 429 {
		t.Fatalf("a fresh /64 of the same /48: %d", r.Code)
	}
	if r := g.client("[2001:db8:2:1::1]").do("POST", "/api/v1/setup/enroll", good); r.Code != 200 {
		t.Fatalf("a different /48: %d %s", r.Code, r.Raw)
	}
}

func TestUnknownUsernamesShareOneBucketOverHTTP(t *testing.T) {
	f := newFlow(t)
	f.setup()
	// Each attempt from its own IP, each with a different non-existent name: only
	// the shared "<unknown>" account bucket can stop them.
	for i := 0; i < 11; i++ { // 10 free failures; the 11th starts the block
		c := f.client(fmt.Sprintf("192.0.2.%d", i+1))
		if r := c.do("POST", "/api/v1/auth/login", map[string]string{"username": fmt.Sprintf("nobody%d", i), "password": "x"}); r.Code != 401 {
			t.Fatalf("attempt %d: %d", i, r.Code)
		}
	}
	if f.lim.AccountPassword.Len() != 1 {
		t.Fatalf("%d account buckets for 11 different non-existent names", f.lim.AccountPassword.Len())
	}
	if r := f.client("192.0.2.200").do("POST", "/api/v1/auth/login", map[string]string{"username": "yet-another", "password": "x"}); r.Code != 429 {
		t.Fatalf("the 12th unknown name: %d", r.Code)
	}
	// The real user is not in that bucket.
	if r := f.client("192.0.2.201").do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); r.Code != 200 {
		t.Fatalf("the real user: %d %s", r.Code, r.Raw)
	}
}

func TestSecondFactorBackoffAcrossIPs(t *testing.T) {
	f := newFlow(t)
	f.setup()
	for i := 0; i < 5; i++ { // five wrong codes, each from another network: the free failures
		c := f.client(fmt.Sprintf("192.0.2.%d", 10+i))
		if l := c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); l.Code != 200 {
			t.Fatalf("login %d: %d", i, l.Code)
		}
		if r := c.do("POST", "/api/v1/auth/2fa", map[string]string{"code": "000000"}); r.Code != 401 || r.code() != "invalid_code" {
			t.Fatalf("wrong code %d: %d %s", i, r.Code, r.Raw)
		}
	}
	right := f.nextCode() // a code of a fresh time step, valid for the next minute
	// The 6th wrong code starts the account back-off...
	c := f.client("192.0.2.30")
	c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword})
	c.do("POST", "/api/v1/auth/2fa", map[string]string{"code": "000000"})
	// ...so even the right code from a brand new IP is refused until it passes.
	d := f.client("192.0.2.31")
	d.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword})
	if r := d.do("POST", "/api/v1/auth/2fa", map[string]string{"code": right}); r.Code != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("the right code during the account back-off: %d %s", r.Code, r.Raw)
	}
	f.clk.Add(5 * time.Second)
	if r := d.do("POST", "/api/v1/auth/2fa", map[string]string{"code": right}); r.Code != 200 {
		t.Fatalf("after the wait: %d %s", r.Code, r.Raw)
	}
}

func TestKnownIPExemptFromAccountBackoff(t *testing.T) {
	f := newFlow(t)
	f.setup()
	known, _ := f.signIn("203.0.113.77") // a full sign-in makes the IP known
	_ = known
	// An attacker locks the account's password bucket from many IPs.
	for i := 0; i < 11; i++ {
		f.client(fmt.Sprintf("192.0.2.%d", 100+i)).do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "wrong password!!"})
	}
	if r := f.client("192.0.2.150").do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); r.Code != 429 {
		t.Fatalf("a stranger during the account lock: %d", r.Code)
	}
	if r := f.client("203.0.113.77").do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); r.Code != 200 {
		t.Fatalf("the admin's usual network: %d %s", r.Code, r.Raw)
	}
}

func TestKnownIPReachesHashWhenMapsFullOfBlocked(t *testing.T) {
	f := newFlow(t)
	f.setup()
	f.signIn("203.0.113.88")
	// Fill both per-IP maps with blocked keys (what a flood from many networks does).
	for i := 0; i < 4096; i++ {
		for j := 0; j < 25; j++ {
			f.lim.IP64.Fail(fmt.Sprintf("flood%d", i), false)
			f.lim.IP48.Fail(fmt.Sprintf("flood%d", i), false)
		}
	}
	before := f.hasher.total()
	// A new network is refused (fail closed) and never reaches the hasher...
	if r := f.client("198.51.100.99").do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); r.Code != 429 {
		t.Fatalf("a new IP with full maps: %d", r.Code)
	}
	if f.hasher.total() != before {
		t.Fatal("a refused request hashed")
	}
	// ...the admin's known IP is admitted and reaches the hash.
	if r := f.client("203.0.113.88").do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); r.Code != 200 {
		t.Fatalf("the known IP with full maps: %d %s", r.Code, r.Raw)
	}
	if f.hasher.total() != before+1 {
		t.Errorf("hash calls %d, want %d", f.hasher.total(), before+1)
	}
}

func TestNoUserEnumeration(t *testing.T) {
	f := newFlow(t)
	f.setup()
	f.st.W.Exec(`INSERT INTO users (username, created_at) VALUES ('resetuser', 1)`) // a reset user: no password, no TOTP
	one := func(name string, ip string) resp {
		return f.client(ip).do("POST", "/api/v1/auth/login", map[string]string{"username": name, "password": "wrong password!!"})
	}
	exists, missing, reset := one("admin", "192.0.2.1"), one("nobody-here", "192.0.2.2"), one("resetuser", "192.0.2.3")
	for _, r := range []resp{missing, reset} {
		if r.Code != exists.Code || r.code() != exists.code() || r.Body["error"].(map[string]any)["message"] != exists.Body["error"].(map[string]any)["message"] {
			t.Fatalf("different answers: %d %s vs %d %s", exists.Code, exists.Raw, r.Code, r.Raw)
		}
	}
	// One hash each: the dummy hash costs what a real one does.
	if f.hasher.verifies.Load() != 1 || f.hasher.dummies.Load() != 2 {
		t.Errorf("verifies %d dummies %d", f.hasher.verifies.Load(), f.hasher.dummies.Load())
	}
	// Blocked: the same body for a name that exists and one that does not.
	for i := 0; i < 8; i++ {
		f.client("192.0.2.50").do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "x"})
	}
	a := f.client("192.0.2.50").do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "x"})
	b := f.client("192.0.2.50").do("POST", "/api/v1/auth/login", map[string]string{"username": "ghost", "password": "x"})
	if a.Code != 429 || b.Code != 429 || a.Header.Get("Retry-After") != b.Header.Get("Retry-After") ||
		a.Body["error"].(map[string]any)["message"] != b.Body["error"].(map[string]any)["message"] {
		t.Errorf("blocked answers differ: %s vs %s", a.Raw, b.Raw)
	}
}

func TestLoginBusyHasher(t *testing.T) {
	f := newFlow(t)
	f.setup()
	f.srv = New(Config{SPA: fstest.MapFS{"index.html": {Data: []byte("x")}}, Sessions: f.as,
		Auth: NewAuthHandlers(AuthDeps{Auth: f.as, Hasher: busyHasher{}, Audit: f.aud, Limits: f.lim})})
	f.h = f.srv.Handler()
	r := f.client("192.0.2.9").do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword})
	if r.Code != 503 || r.Header.Get("Retry-After") != "2" || r.code() != "busy" {
		t.Fatalf("%d %s", r.Code, r.Raw)
	}
}

type busyHasher struct{}

func (busyHasher) Verify(context.Context, string, string) (bool, error) { return false, auth.ErrBusy }
func (busyHasher) VerifyDummy(context.Context, string) error            { return auth.ErrBusy }

// ---- reauth, notice ----------------------------------------------------------------------

func TestReauthFailuresRevokeSession(t *testing.T) {
	f := newFlow(t)
	c := f.setup()
	// A right code extends the fresh-2FA window.
	f.clk.Add(10 * time.Minute)
	r := c.do("POST", "/api/v1/auth/reauth", map[string]string{"code": f.nextCode()})
	if r.Code != 200 || r.Body["mfa_fresh_until"] == nil {
		t.Fatalf("%d %s", r.Code, r.Raw)
	}
	var fresh int64
	f.st.R.QueryRow(`SELECT mfa_fresh_until FROM sessions`).Scan(&fresh)
	if fresh != f.clk.Now().Add(5*time.Minute).Unix() {
		t.Errorf("fresh until %d", fresh)
	}
	// Wrong codes: counted, and the 5th revokes the session and clears the cookie.
	for i := 1; i <= 4; i++ {
		r := c.do("POST", "/api/v1/auth/reauth", map[string]string{"code": "000000"})
		if r.Code != 403 || r.code() != "invalid_code" || r.Body["failures_left"] != float64(5-i) {
			t.Fatalf("wrong code %d: %d %s", i, r.Code, r.Raw)
		}
		f.clk.Add(time.Minute) // clear the account back-off between tries
	}
	last := c.do("POST", "/api/v1/auth/reauth", map[string]string{"code": "000000"})
	if last.Code != 401 || last.code() != "revoked" || c.cookies[SessionCookie] != "" {
		t.Fatalf("5th wrong code: %d %s cookie %q", last.Code, last.Raw, c.cookies[SessionCookie])
	}
	if f.count(`SELECT count(*) FROM sessions`) != 0 {
		t.Error("the session was not deleted")
	}
}

func TestAdminNoticeAfterTOTPFailures(t *testing.T) {
	f := newFlow(t)
	f.setup()
	for i := 0; i < 3; i++ { // wrong second factors after a correct password
		c := f.client(fmt.Sprintf("192.0.2.%d", 70+i))
		c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword})
		c.do("POST", "/api/v1/auth/2fa", map[string]string{"code": "000000"})
	}
	// ...and a wrong step-up code from a live session counts too.
	live, r := f.signIn("203.0.113.5")
	if n := r.Body["notice"].(map[string]any); n["totp_failures"] != float64(3) || n["last_failure_ip"] == "" {
		t.Fatalf("notice at the next sign-in: %s", r.Raw)
	}
	live.do("POST", "/api/v1/auth/reauth", map[string]string{"code": "000000"})
	_, r = f.signIn("203.0.113.5")
	if n := r.Body["notice"].(map[string]any); n["totp_failures"] != float64(1) {
		t.Fatalf("a failed reauth was not counted: %s", r.Raw)
	}
	_, r = f.signIn("203.0.113.5")
	if n := r.Body["notice"].(map[string]any); len(n) != 0 {
		t.Fatalf("the notice did not reset: %s", r.Raw)
	}
}

func TestLoginRecoveryCodeOverHTTPCountsAgainstBackoff(t *testing.T) {
	f := newFlow(t)
	f.setup()
	c := f.client("203.0.113.5")
	c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword})
	r := c.do("POST", "/api/v1/auth/2fa", map[string]string{"recovery_code": f.codes[0]})
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Raw)
	}
	d := f.client("203.0.113.5")
	d.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword})
	if r := d.do("POST", "/api/v1/auth/2fa", map[string]string{"recovery_code": f.codes[0]}); r.Code != 401 || r.code() != "invalid_code" {
		t.Fatalf("a used recovery code: %d %s", r.Code, r.Raw)
	}
	if f.lim.Account2FA.Len() != 1 {
		t.Error("a wrong recovery code was not counted against the second-factor back-off")
	}
	if r := d.do("POST", "/api/v1/auth/2fa", map[string]string{"code": "123456", "recovery_code": "x"}); r.Code != 400 {
		t.Errorf("both factors: %d", r.Code)
	}
}

func TestReusedTOTPCodeHasItsOwnMessage(t *testing.T) {
	f := newFlow(t)
	f.setup() // the setup code's counter was recorded
	c := f.client("203.0.113.5")
	c.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword})
	r := c.do("POST", "/api/v1/auth/2fa", map[string]string{"code": f.code()})
	if r.Code != 401 || r.code() != "code_reused" || !strings.Contains(r.Raw, "already used") {
		t.Fatalf("%d %s", r.Code, r.Raw)
	}
}

// ---- audit -----------------------------------------------------------------------------

func TestFailedLoginAuditRowsAndUsernames(t *testing.T) {
	f := newFlow(t)
	f.setup()
	pwAsName := "hunter2 correct horse"
	for _, name := range []string{"admin", "ghost", pwAsName} {
		f.client("192.0.2.1").do("POST", "/api/v1/auth/login", map[string]string{"username": name, "password": "wrong password!!"})
		f.clk.Add(time.Minute)
	}
	rows := f.auditRows("auth.login", "denied")
	if len(rows) != 3 {
		t.Fatalf("%d denied rows", len(rows))
	}
	for _, row := range rows {
		if row["kind"] != "anonymous" || row["actor"] != "anonymous:auth.login" || !strings.Contains(row["args"].(string), `"ip_key":"192.0.2.1"`) {
			t.Errorf("row %v", row)
		}
	}
	// The user name is recorded only if that user exists.
	if !strings.Contains(rows[0]["args"].(string), `"username":"admin"`) {
		t.Errorf("existing user not recorded: %v", rows[0])
	}
	for _, i := range []int{1, 2} {
		var args map[string]any
		json.Unmarshal([]byte(rows[i]["args"].(string)), &args)
		if args["username"] != "<unknown>" {
			t.Errorf("an unknown name was recorded: %v", rows[i])
		}
	}
	// A password typed in the username field is never stored anywhere in the log.
	all, _ := f.st.R.Query(`SELECT args_json || target || error FROM audit_log`)
	defer all.Close()
	for all.Next() {
		var s string
		all.Scan(&s)
		if strings.Contains(s, pwAsName) || strings.Contains(s, "ghost") || strings.Contains(s, testPassword) {
			t.Fatalf("an audit row holds typed input: %s", s)
		}
	}
}

func TestAuthIntentFailureFailsClosed(t *testing.T) {
	f := newFlow(t)
	// Setup: nothing is created when the intent row cannot be written.
	tok := f.token()
	c := f.client("203.0.113.5")
	en := c.do("POST", "/api/v1/setup/enroll", map[string]string{"token": tok, "username": "admin"})
	f.seed = decodeSecret(t, en.Body["secret"].(string))
	f.aud.fail.Store(true)
	done := c.do("POST", "/api/v1/setup/complete", map[string]string{"token": tok, "enrollment_id": en.Body["enrollment_id"].(string),
		"username": "admin", "password": testPassword, "code": f.code()})
	if done.Code != 503 || done.code() != "audit_unavailable" || c.cookies[SessionCookie] != "" {
		t.Fatalf("setup: %d %s", done.Code, done.Raw)
	}
	if f.count(`SELECT count(*) FROM users`) != 0 || f.count(`SELECT count(*) FROM sessions`) != 0 || f.count(`SELECT count(*) FROM setup_state`) != 1 {
		t.Fatal("setup left state behind")
	}
	f.aud.fail.Store(false)
	signed := c.do("POST", "/api/v1/setup/complete", map[string]string{"token": tok, "enrollment_id": en.Body["enrollment_id"].(string),
		"username": "admin", "password": testPassword, "code": f.code()})
	if signed.Code != 200 {
		t.Fatalf("setup after the audit log came back: %d %s", signed.Code, signed.Raw)
	}
	u, _, _ := f.as.UserByName(context.Background(), "admin")
	f.userID = u.ID

	// Login (password step): no pending cookie, no pending row.
	f.aud.fail.Store(true)
	lc := f.client("203.0.113.6")
	if r := lc.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}); r.Code != 503 || r.code() != "audit_unavailable" {
		t.Fatalf("login: %d %s", r.Code, r.Raw)
	}
	if lc.cookies[PendingCookie] != "" || f.count(`SELECT count(*) FROM auth_pending`) != 0 {
		t.Fatal("login minted a pending step without an audit intent")
	}
	// 2FA: no session. (A pending row made while the audit log was up.)
	f.aud.fail.Store(false)
	two := f.client("203.0.113.7")
	two.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword})
	before := f.count(`SELECT count(*) FROM sessions`)
	f.aud.fail.Store(true)
	if r := two.do("POST", "/api/v1/auth/2fa", map[string]string{"code": f.nextCode()}); r.Code != 503 || r.code() != "audit_unavailable" {
		t.Fatalf("2fa: %d %s", r.Code, r.Raw)
	}
	if f.count(`SELECT count(*) FROM sessions`) != before || two.cookies[SessionCookie] != "" {
		t.Fatal("2fa minted a session without an audit intent")
	}
	// Logout: the session is NOT revoked when the intent cannot be written.
	f.aud.fail.Store(false)
	lo, _ := f.signIn("203.0.113.8")
	f.aud.fail.Store(true)
	if r := lo.do("POST", "/api/v1/auth/logout", map[string]string{}); r.Code != 503 {
		t.Fatalf("logout: %d", r.Code)
	}
	f.aud.fail.Store(false)
	if r := lo.do("GET", "/api/v1/auth/session", nil); r.Code != 200 {
		t.Errorf("the session was revoked although the logout was refused: %d", r.Code)
	}
}

// Intent first, then an outcome, with the user as the actor.
func TestAuthAuditRowsIntentThenOutcome(t *testing.T) {
	f := newFlow(t)
	c := f.setup()
	c.do("POST", "/api/v1/auth/logout", map[string]string{})
	f.signIn("203.0.113.5")
	rows, _ := f.st.R.Query(`SELECT action, phase, result, actor_kind, actor_id, args_json FROM audit_log ORDER BY id`)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, phase, result, kind, actor, args string
		rows.Scan(&action, &phase, &result, &kind, &actor, &args)
		got = append(got, action+"/"+phase+"/"+result+"/"+actor)
		if strings.Contains(args, testPassword) || strings.Contains(args, "sd_") {
			t.Errorf("a secret in %s: %s", action, args)
		}
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{"auth.setup/intent/pending/anonymous:auth.setup", "auth.setup/outcome/ok/anonymous:auth.setup",
		"auth.logout/intent/pending/user:1", "auth.logout/outcome/ok/user:1", "auth.login/intent/pending/user:1", "auth.login/outcome/ok/user:1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
}
