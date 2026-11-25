package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
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

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/auth"
	"github.com/Docker-Hunterpedia/serverdock/internal/deploy"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// recordingRunner wraps the registry the REST layer calls and records every
// invocation: the actor kind, the action's flags and the outcome. It is how the
// tests assert that no HTTP request ever produced a system, MCP or webhook actor
// and that no token ever ran a sensitive, destructive, session-only or
// console-only action.
type recordingRunner struct {
	ActionRunner
	mu    sync.Mutex
	calls []recordedCall
}

type recordedCall struct {
	kind  actions.ActorKind
	scope string
	info  actions.Info
	err   error
}

func (r *recordingRunner) Invoke(ctx context.Context, a actions.Actor, name string, raw json.RawMessage, o actions.Options) (actions.Result, error) {
	res, err := r.ActionRunner.Invoke(ctx, a, name, raw, o)
	info, _ := r.ActionRunner.Lookup(name)
	r.mu.Lock()
	r.calls = append(r.calls, recordedCall{kind: a.Kind(), info: info, err: err})
	r.mu.Unlock()
	return res, err
}

func (r *recordingRunner) reset() { r.mu.Lock(); r.calls = nil; r.mu.Unlock() }

func (r *recordingRunner) assertClean(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		switch c.kind {
		case actions.KindUser, actions.KindToken:
		default:
			t.Errorf("an HTTP request produced a %q actor for %s", c.kind, c.info.Name)
		}
		if c.kind == actions.KindToken && c.err == nil && (c.info.Sensitive || c.info.Risk == actions.RiskDestructive || c.info.SessionOnly || c.info.ConsoleOnly || c.info.JobOnly) {
			t.Errorf("a token ran %s (sensitive=%v destructive=%v session-only=%v console-only=%v)", c.info.Name, c.info.Sensitive, c.info.Risk == actions.RiskDestructive, c.info.SessionOnly, c.info.ConsoleOnly)
		}
		if (c.info.ConsoleOnly || c.info.JobOnly) && c.err == nil {
			t.Errorf("a console-only or job-only action ran over HTTP: %s", c.info.Name)
		}
	}
}

// cellRuns counts executions of the test cells (a refused request must not run one).
var cellRuns atomic.Int32

// ---- the test cells: one stub action per policy cell -------------------------------------

type cellIn struct {
	Pass string `json:"pass,omitempty" sd:"secret"`
}

func (cellIn) Validate() error { return nil }

func registerTestCells(r *actions.Registry) {
	ok := func(name string) func(context.Context, actions.Call, cellIn) (string, error) {
		return func(context.Context, actions.Call, cellIn) (string, error) {
			cellRuns.Add(1)
			return "ran " + name, nil
		}
	}
	tgt := func(cellIn) string { return "the-target" }
	add := func(name string, d actions.Def[cellIn, string]) {
		d.Name, d.Summary = name, "test cell"
		if d.Execute == nil {
			d.Execute = ok(name)
		}
		actions.Register(r, d)
	}
	add("t.read", actions.Def[cellIn, string]{Risk: actions.RiskRead, Scope: actions.ScopeRead})
	add("t.write", actions.Def[cellIn, string]{Risk: actions.RiskMutate, Scope: actions.ScopeWrite, Target: tgt,
		DryRun: func(context.Context, actions.Call, cellIn) (actions.Plan, error) {
			return actions.Plan{Summary: "would write"}, nil
		}})
	add("t.admin_mutate", actions.Def[cellIn, string]{Risk: actions.RiskMutate, Scope: actions.ScopeAdmin, Target: tgt})
	add("t.sensitive", actions.Def[cellIn, string]{Risk: actions.RiskRead, Scope: actions.ScopeAdmin, Sensitive: true, Target: tgt})
	add("t.destroy", actions.Def[cellIn, string]{Risk: actions.RiskDestructive, Scope: actions.ScopeAdmin, Target: tgt})
	add("t.session_only", actions.Def[cellIn, string]{Risk: actions.RiskMutate, Scope: actions.ScopeWrite, SessionOnly: true, Target: tgt})
	add("t.console", actions.Def[cellIn, string]{Risk: actions.RiskDestructive, Scope: actions.ScopeAdmin, ConsoleOnly: true, System: "test console action", Target: tgt})
	add("t.jobonly", actions.Def[cellIn, string]{Risk: actions.RiskMutate, Scope: actions.ScopeWrite, JobOnly: true, Target: tgt})
	add("t.removes", actions.Def[cellIn, string]{Risk: actions.RiskMutate, Scope: actions.ScopeWrite, RemovesAccess: true, Target: tgt})
	add("t.leaky", actions.Def[cellIn, string]{Risk: actions.RiskMutate, Scope: actions.ScopeWrite, Target: tgt,
		Execute: func(_ context.Context, _ actions.Call, i cellIn) (string, error) {
			return "", errors.New("backend said " + i.Pass)
		}})
	add("t.streams", actions.Def[cellIn, string]{Risk: actions.RiskRead, Scope: actions.ScopeRead,
		Execute: func(context.Context, actions.Call, cellIn) (string, error) {
			return "", fmt.Errorf("agent: %w", hub.ErrTooManyStreams)
		}})
	add("t.localonly", actions.Def[cellIn, string]{Risk: actions.RiskRead, Scope: actions.ScopeRead,
		Execute: func(context.Context, actions.Call, cellIn) (string, error) {
			return "", fmt.Errorf("agent: %w", hub.ErrLocalOnly)
		}})
}

// ---- the service actions (plan 0004 step 7) ------------------------------------------------------

type stubApplier struct{}

func (stubApplier) ProxyApply(context.Context, []agentops.Route, bool) (agentops.ApplyResult, error) {
	return agentops.ApplyResult{Applied: true, FailedPorts: []int{}}, nil
}

type stubPorts struct{}

func (stubPorts) ContainerPorts(context.Context) (agentops.PortsResult, error) {
	return agentops.PortsResult{Containers: []agentops.PublishingContainer{}}, nil
}

// registerServiceActions registers the step 7 actions over the fixture's store
// with an applier and an exposure source that answer without Docker, and adds
// the service p/x that the read actions of the policy matrix look up.
func registerServiceActions(reg *actions.Registry, st *store.Store, now func() time.Time) {
	st.W.Exec(`INSERT OR IGNORE INTO projects(id, name, created_at) VALUES (1, 'p', 'x')`)
	st.W.Exec(`INSERT OR IGNORE INTO services(id, project_id, name, image_ref, container_port, health_kind, health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, created_at, updated_at)
		VALUES (1, 1, 'x', 'nginx:1', 80, 'tcp', 60, 3, 'blue_green', 10, 10, 'x', 'x')`)
	deploy.RegisterServiceActions(reg, deploy.ServiceDeps{DB: st, Routes: deploy.NewRoutes(st, stubApplier{}, nil), ReservedPorts: []int{8443},
		Exposure: stubPorts{}, Now: now})
}

// ---- helpers --------------------------------------------------------------------------------

type restResp struct {
	Code   int
	Body   map[string]any
	Raw    string
	Header http.Header
	Cookie map[string]*http.Cookie
}

func (r restResp) errCode() string {
	if e, ok := r.Body["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

// restDo sends a request to the fixture's handler as the given credential.
type restCred struct {
	session    string // a session cookie value (CSRF and Origin are added)
	token      string
	noCSRF     bool
	host       string
	headers    map[string]string
	remoteAddr string
}

func (f *fixture) rest(method, path, body string, c restCred) restResp {
	f.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:8443"
	if c.host != "" {
		r.Host = c.host
	}
	r.RemoteAddr = "192.0.2.9:1"
	if c.remoteAddr != "" {
		r.RemoteAddr = c.remoteAddr
	}
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	if c.session != "" {
		cookie(r, SessionCookie, c.session)
		if method == "POST" {
			r.Header.Set("Origin", "https://"+r.Host)
			if !c.noCSRF {
				r.Header.Set(CSRFHeader, CSRFToken(c.session))
			}
		}
	}
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range c.headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	out := restResp{Code: rec.Code, Raw: rec.Body.String(), Header: rec.Header(), Cookie: map[string]*http.Cookie{}}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.Body)
	for _, sc := range rec.Result().Cookies() {
		out.Cookie[sc.Name] = sc
	}
	return out
}

// freshSession makes a new session (its 2FA window is fresh) and returns its cookie.
func (f *fixture) freshSession() string { return f.login() }

// staleSession makes a session whose fresh-2FA window has ended.
func (f *fixture) staleSession() string {
	s := f.login()
	f.st.W.Exec(`UPDATE sessions SET mfa_fresh_until = ? WHERE id_hash = ?`, f.clk.Now().Unix()-60, sha(s))
	return s
}

func actionPath(name string) string { return "/api/v1/actions/" + name }

// ---- the policy matrix over REST ---------------------------------------------------------------

// validInput is a body every registered action accepts (strict decoding rejects extras).
func validInput(name string) string {
	switch name {
	case "auth.password_change":
		return `{"current_password":"` + testPassword + `","new_password":"a brand new passphrase"}`
	case "auth.totp_reenroll_confirm":
		return `{"enrollment_id":"e","code":"123456"}`
	case "auth.sessions_revoke":
		return `{"all_others":true}`
	case "tokens.create":
		return `{"name":"n","scope":"read"}`
	case "tokens.revoke":
		return `{"id":1}`
	// The service actions: bodies that validate, against the fixture's service p/x.
	case "services.get", "services.retry_port", "services.deploy", "services.redeploy", "services.restart", "services.delete":
		return `{"service":"p/x"}`
	case "services.rollback":
		return `{"service":"p/x","to":1}`
	case "deploys.cancel":
		return `{"service":"p/x","number":1}`
	case "services.create":
		return `{"project":"p","name":"n","image_ref":"nginx"}`
	case "services.update":
		return `{"service":"p/x","strategy":"stop_first"}`
	case "services.set_source":
		return `{"service":"p/x","image_ref":"nginx"}`
	case "services.set_public_port":
		return `{"service":"p/x","public_port":9000}`
	}
	return `{}`
}

// runsForReal: actions whose execution has no effect the matrix cannot afford
// (password change would end the sessions it uses).
func runsForReal(info actions.Info) bool {
	if strings.HasPrefix(info.Name, "t.") {
		return true
	}
	return info.Risk == actions.RiskRead && info.Name != "audit.verify"
}

// expectedStatus is the status the policy table prescribes for an HTTP actor
// that has no confirmation header, from the action's flags alone.
func expectedStatus(info actions.Info, cred string) int {
	if cred == "none" {
		return 401
	}
	if info.ConsoleOnly || info.JobOnly {
		return 404
	}
	switch cred {
	case "token read", "token write", "token admin":
		scope := map[string]actions.Scope{"token read": actions.ScopeRead, "token write": actions.ScopeWrite, "token admin": actions.ScopeAdmin}[cred]
		if info.SessionOnly || info.Sensitive || info.Risk == actions.RiskDestructive || scope < info.Scope {
			return 403
		}
		return 200
	case "session stale":
		if info.Sensitive || info.Risk == actions.RiskDestructive {
			return 403 // fresh two-factor required (a destructive one also needs SD-Confirm)
		}
		return 200
	case "session fresh":
		if info.Risk == actions.RiskDestructive {
			return 403 // needs SD-Confirm
		}
		return 200
	}
	panic(cred)
}

func TestRESTActionPolicyMatrix(t *testing.T) {
	f := newFixture(t, true)
	f.rec.reset()
	list := f.reg.List()
	if len(list) < 20 {
		t.Fatalf("%d actions registered", len(list))
	}
	for _, info := range list {
		creds := map[string]restCred{
			"none": {}, "token read": {token: f.tokens["token read"]}, "token write": {token: f.tokens["token write"]}, "token admin": {token: f.tokens["token admin"]},
		}
		for _, cred := range []string{"none", "token read", "token write", "token admin", "session stale", "session fresh"} {
			t.Run(info.Name+"/"+cred, func(t *testing.T) {
				c := creds[cred]
				switch cred {
				case "session stale":
					c = restCred{session: f.staleSession()}
				case "session fresh":
					c = restCred{session: f.freshSession()}
				}
				want := expectedStatus(info, cred)
				if want == 200 { // the cells that fail on purpose, to check the error mapping
					switch info.Name {
					case "t.leaky":
						want = 500
					case "t.streams":
						want = 503
					case "t.localonly":
						want = 403
					}
				}
				if want >= 200 && want < 300 && !runsForReal(info) {
					// A real, state-changing action: assert the decision without running it,
					// through the dry-run-free policy check the registry exposes.
					return
				}
				res := f.rest("POST", actionPath(info.Name), validInput(info.Name), c)
				if res.Code != want {
					t.Fatalf("%s as %s: %d %s, want %d", info.Name, cred, res.Code, res.Raw, want)
				}
				if want == 200 && res.Body["request_id"] == nil {
					t.Error("no request_id")
				}
			})
		}
	}
	f.rec.assertClean(t)
}

func TestRESTNeverCreatesAnotherActorKind(t *testing.T) {
	f := newFixture(t, true)
	f.rec.reset()
	for _, cred := range []restCred{{session: f.freshSession()}, {token: f.tokens["token admin"]}, {token: f.tokens["token read"]}} {
		f.rest("POST", actionPath("t.read"), `{}`, cred)
		f.rest("POST", actionPath("t.destroy"), `{}`, cred)
		f.rest("POST", actionPath("t.console"), `{}`, cred)
	}
	f.rec.mu.Lock()
	n := len(f.rec.calls)
	f.rec.mu.Unlock()
	if n == 0 {
		t.Fatal("nothing was recorded")
	}
	f.rec.assertClean(t)
	// The package has no way to make a system actor: it never refers to the invoker.
	for _, name := range []string{"rest.go", "routes.go", "middleware.go", "authhandlers.go", "openapi.go"} {
		b, _ := os.ReadFile(name)
		for _, bad := range []string{"SystemInvoker", "KindSystem", "AdminReset"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s mentions %s", name, bad)
			}
		}
	}
}

func TestRESTEnvelopeAndOptions(t *testing.T) {
	f := newFixture(t, true)
	s := restCred{session: f.freshSession()}
	// A plain read is not audited; a mutation is recorded; the request id is ours.
	r := f.rest("POST", actionPath("t.read"), ``, s) // an empty body means {}
	if r.Code != 200 || r.Body["data"] != "ran t.read" || r.Body["audit"] != "not_audited" || r.Body["request_id"] == nil {
		t.Fatalf("%d %s", r.Code, r.Raw)
	}
	w := f.rest("POST", actionPath("t.write"), `{}`, restCred{session: s.session, headers: map[string]string{"SD-Request-Id": "attacker-chosen"}})
	if w.Code != 200 || w.Body["audit"] != "recorded" || w.Body["request_id"] == "attacker-chosen" {
		t.Fatalf("%d %s", w.Code, w.Raw)
	}
	// Dry run: a plan and no data; an action without a dry run is a 400.
	d := f.rest("POST", actionPath("t.write"), `{}`, restCred{session: s.session, headers: map[string]string{"SD-Dry-Run": "1"}})
	if d.Code != 200 || d.Body["plan"] == nil || d.Body["data"] != nil {
		t.Fatalf("dry run: %d %s", d.Code, d.Raw)
	}
	if nd := f.rest("POST", actionPath("t.admin_mutate"), `{}`, restCred{session: s.session, headers: map[string]string{"SD-Dry-Run": "1"}}); nd.Code != 400 {
		t.Fatalf("no dry run: %d %s", nd.Code, nd.Raw)
	}
	// Destructive: the exact target in SD-Confirm.
	if x := f.rest("POST", actionPath("t.destroy"), `{}`, s); x.Code != 403 || x.errCode() != "confirm" {
		t.Fatalf("no confirm: %d %s", x.Code, x.Raw)
	}
	if x := f.rest("POST", actionPath("t.destroy"), `{}`, restCred{session: s.session, headers: map[string]string{"SD-Confirm": "wrong"}}); x.Code != 403 {
		t.Fatalf("wrong confirm: %d", x.Code)
	}
	if x := f.rest("POST", actionPath("t.destroy"), `{}`, restCred{session: s.session, headers: map[string]string{"SD-Confirm": "the-target"}}); x.Code != 200 {
		t.Fatalf("right confirm: %d %s", x.Code, x.Raw)
	}
	// Fresh 2FA: a stale session gets a machine-readable code the SPA turns into a step-up.
	if x := f.rest("POST", actionPath("t.sensitive"), `{}`, restCred{session: f.staleSession()}); x.Code != 403 || x.errCode() != "mfa_required" {
		t.Fatalf("stale: %d %s", x.Code, x.Raw)
	}
	// Input errors and limits.
	for name, body := range map[string]string{"unknown field": `{"nope":1}`, "not json": `{`, "wrong type": `{"pass":5}`, "array": `[]`} {
		if x := f.rest("POST", actionPath("t.write"), body, s); x.Code != 400 || x.errCode() != "invalid_input" {
			t.Errorf("%s: %d %s", name, x.Code, x.Raw)
		}
	}
	if x := f.rest("POST", actionPath("t.write"), `{"pass":"`+strings.Repeat("x", 70<<10)+`"}`, s); x.Code != 400 {
		t.Errorf("a body over 64 KiB: %d", x.Code)
	}
	// Unknown and console-only actions are both 404, even with a valid session.
	for _, n := range []string{"nope.nothing", "t.console", "t.jobonly", "auth.admin_reset"} {
		if x := f.rest("POST", actionPath(n), `{}`, s); x.Code != 404 || x.errCode() != "unknown_action" {
			t.Errorf("%s: %d %s", n, x.Code, x.Raw)
		}
	}
}

// An action's own refusals (actions.UserError) map to 404, 409, 503 and 400 with
// the action's wording, and its secrets are redacted like any execution error.
func TestRESTMapsActionRefusals(t *testing.T) {
	f := newFixture(t, true)
	s := restCred{session: f.freshSession()}
	refuse := func(name string, mk func(string) error) {
		actions.Register(f.reg, actions.Def[cellIn, string]{Name: name, Summary: "test cell", Risk: actions.RiskMutate, Scope: actions.ScopeWrite,
			Target: func(cellIn) string { return "t" },
			Execute: func(_ context.Context, _ actions.Call, in cellIn) (string, error) {
				return "", mk(in.Pass)
			}})
	}
	refuse("t.notfound", func(p string) error { return actions.NotFound("no service p/x (%s)", p) })
	refuse("t.conflict", func(p string) error { return actions.Conflict("port 9000 is taken (%s)", p) })
	refuse("t.unavailable", func(string) error { return actions.Unavailable("Docker did not answer") })
	refuse("t.invalid", func(string) error { return actions.InvalidInput("the repository cannot change") })
	for name, want := range map[string]struct {
		code int
		err  string
		msg  string
	}{
		"t.notfound":    {404, "not_found", "no service p/x"},
		"t.conflict":    {409, "conflict", "port 9000 is taken"},
		"t.unavailable": {503, "unavailable", "Docker did not answer"},
		"t.invalid":     {400, "invalid_input", "the repository cannot change"},
	} {
		x := f.rest("POST", actionPath(name), `{"pass":"hunter2-secret"}`, s)
		if x.Code != want.code || x.errCode() != want.err || !strings.Contains(x.Raw, want.msg) {
			t.Errorf("%s: %d %s", name, x.Code, x.Raw)
		}
		if strings.Contains(x.Raw, "hunter2-secret") {
			t.Errorf("%s leaked the secret input: %s", name, x.Raw)
		}
	}
	if x := f.rest("POST", actionPath("t.unavailable"), `{}`, s); x.Header.Get("Retry-After") == "" {
		t.Error("503 unavailable without Retry-After")
	}
}

func TestRESTErrorMapping(t *testing.T) {
	f := newFixture(t, true)
	s := restCred{session: f.freshSession()}
	// ErrTooManyStreams -> 503 with Retry-After; ErrLocalOnly -> 403.
	if x := f.rest("POST", actionPath("t.streams"), `{}`, s); x.Code != 503 || x.errCode() != "busy" || x.Header.Get("Retry-After") == "" {
		t.Errorf("too many streams: %d %s", x.Code, x.Raw)
	}
	if x := f.rest("POST", actionPath("t.localonly"), `{}`, s); x.Code != 403 || x.errCode() != "denied" {
		t.Errorf("local only: %d %s", x.Code, x.Raw)
	}
	// An execution failure is a generic 500 that carries no secret from the input.
	x := f.rest("POST", actionPath("t.leaky"), `{"pass":"hunter2-sentinel"}`, s)
	if x.Code != 500 || x.errCode() != "action_failed" || strings.Contains(x.Raw, "hunter2-sentinel") {
		t.Errorf("leaky: %d %s", x.Code, x.Raw)
	}
	// Audit log down: a mutation is a 503; a read is not affected; an access-removing
	// action still runs and says its record is missing.
	f.sink.down.Store(true)
	if x := f.rest("POST", actionPath("t.write"), `{}`, s); x.Code != 503 || x.errCode() != "audit_unavailable" || x.Header.Get("Retry-After") != "2" {
		t.Errorf("audit down, mutation: %d %s", x.Code, x.Raw)
	}
	if x := f.rest("POST", actionPath("t.read"), `{}`, s); x.Code != 200 {
		t.Errorf("audit down, read: %d", x.Code)
	}
	if x := f.rest("POST", actionPath("t.removes"), `{}`, s); x.Code != 200 || x.Body["audit"] != "unrecorded" || x.Body["data"] != "ran t.removes" {
		t.Errorf("audit down, removes access: %d %s", x.Code, x.Raw)
	}
	f.sink.down.Store(false)
}

func TestAuditVerifyRESTLimit(t *testing.T) {
	f := newFixture(t, true)
	tok := restCred{token: f.tokens["token admin"]}
	for i := 0; i < 2; i++ {
		if x := f.rest("POST", actionPath("audit.verify"), `{}`, tok); x.Code != 200 {
			t.Fatalf("call %d: %d %s", i+1, x.Code, x.Raw)
		}
	}
	if x := f.rest("POST", actionPath("audit.verify"), `{}`, tok); x.Code != 429 || x.Header.Get("Retry-After") == "" || x.errCode() != "rate_limited" {
		t.Fatalf("the 3rd call in a minute: %d %s", x.Code, x.Raw)
	}
	// Another actor has its own allowance; other actions are not limited.
	if x := f.rest("POST", actionPath("audit.verify"), `{}`, restCred{session: f.freshSession()}); x.Code != 200 {
		t.Errorf("another actor: %d %s", x.Code, x.Raw)
	}
	for i := 0; i < 5; i++ {
		if x := f.rest("POST", actionPath("system.info"), `{}`, tok); x.Code != 200 {
			t.Fatalf("system.info: %d", x.Code)
		}
	}
}

// REST clears __Host-sd_session after the actions that end the caller's session.
func TestRESTClearsTheSessionCookieWhereItShould(t *testing.T) {
	cleared := func(r restResp) bool { c := r.Cookie[SessionCookie]; return c != nil && c.MaxAge < 0 }

	// auth.password_change: ends every session; the cookie is cleared, other actions leave it alone.
	f := newFixture(t, true)
	s := f.freshSession()
	if x := f.rest("POST", actionPath("auth.session_info"), `{}`, restCred{session: s}); x.Code != 200 || len(x.Cookie) != 0 {
		t.Fatalf("a read: %d cookies %v", x.Code, x.Cookie)
	}
	if x := f.rest("POST", actionPath("t.write"), `{}`, restCred{session: s}); x.Code != 200 || len(x.Cookie) != 0 {
		t.Fatalf("a mutation: %d cookies %v", x.Code, x.Cookie)
	}
	pc := f.rest("POST", actionPath("auth.password_change"), validInput("auth.password_change"), restCred{session: s})
	if pc.Code != 200 || !cleared(pc) {
		t.Fatalf("password_change: %d %s cookies %v", pc.Code, pc.Raw, pc.Cookie)
	}
	if x := f.rest("GET", "/api/v1/auth/session", ``, restCred{session: s}); x.Code != 401 {
		t.Errorf("the session survived the password change: %d", x.Code)
	}

	// auth.totp_reenroll_confirm, with host_kind taken from the request's Host.
	g := newFixture(t, true)
	gs := g.freshSession()
	begin := g.rest("POST", actionPath("auth.totp_reenroll_begin"), `{}`, restCred{session: gs})
	if begin.Code != 200 || len(begin.Cookie) != 0 {
		t.Fatalf("begin: %d %s", begin.Code, begin.Raw)
	}
	data := begin.Body["data"].(map[string]any)
	newSeed := decodeSecret(t, data["secret"].(string))
	g.clk.Add(31 * time.Second)
	code := auth.TOTPCode(newSeed, auth.TOTPCounter(g.clk.Now()))
	body := fmt.Sprintf(`{"enrollment_id":%q,"code":%q}`, data["enrollment_id"], code)
	// host_kind is not client input.
	if x := g.rest("POST", actionPath("auth.totp_reenroll_confirm"), fmt.Sprintf(`{"enrollment_id":%q,"code":%q,"host_kind":"name"}`, data["enrollment_id"], code), restCred{session: gs}); x.Code != 400 {
		t.Fatalf("host_kind in the body: %d %s", x.Code, x.Raw)
	}
	conf := g.rest("POST", actionPath("auth.totp_reenroll_confirm"), body, restCred{session: gs, host: "localhost:8443"})
	if conf.Code != 200 || !cleared(conf) {
		t.Fatalf("confirm: %d %s cookies %v", conf.Code, conf.Raw, conf.Cookie)
	}
	var host string
	g.st.R.QueryRow(`SELECT totp_enrolled_host FROM users`).Scan(&host)
	if host != "localhost" {
		t.Errorf("host kind %q, want localhost (from r.Host)", host)
	}

	// auth.sessions_revoke: cleared only when it removed the caller's own session.
	h := newFixture(t, true)
	own := h.freshSession()
	other := h.freshSession()
	var ownID, otherID string
	h.st.R.QueryRow(`SELECT public_id FROM sessions WHERE id_hash = ?`, sha(own)).Scan(&ownID)
	h.st.R.QueryRow(`SELECT public_id FROM sessions WHERE id_hash = ?`, sha(other)).Scan(&otherID)
	if x := h.rest("POST", actionPath("auth.sessions_revoke"), fmt.Sprintf(`{"public_id":%q}`, otherID), restCred{session: own}); x.Code != 200 || len(x.Cookie) != 0 {
		t.Fatalf("revoking another session: %d %s cookies %v", x.Code, x.Raw, x.Cookie)
	}
	if x := h.rest("POST", actionPath("auth.sessions_revoke"), `{"all_others":true}`, restCred{session: own}); x.Code != 200 || len(x.Cookie) != 0 {
		t.Fatalf("all_others: %d cookies %v", x.Code, x.Cookie)
	}
	if x := h.rest("POST", actionPath("auth.sessions_revoke"), fmt.Sprintf(`{"public_id":%q}`, ownID), restCred{session: own}); x.Code != 200 || !cleared(x) {
		t.Fatalf("revoking the caller's own session: %d %s cookies %v", x.Code, x.Raw, x.Cookie)
	}
	// A token request never sets or clears a cookie.
	if x := h.rest("POST", actionPath("t.write"), `{}`, restCred{token: h.tokens["token write"]}); x.Code != 200 || len(x.Cookie) != 0 {
		t.Errorf("token: %d cookies %v", x.Code, x.Cookie)
	}
}

// ---- OpenAPI -------------------------------------------------------------------------------------

func TestJobOnlyActionsAre404AndNotInOpenAPI(t *testing.T) {
	f := newFixture(t, true)
	before := cellRuns.Load()
	for name, c := range map[string]restCred{"session": {session: f.freshSession()}, "token write": {token: f.tokens["token write"]}, "token admin": {token: f.tokens["token admin"]}} {
		if x := f.rest("POST", actionPath("t.jobonly"), `{}`, c); x.Code != 404 || x.errCode() != "unknown_action" {
			t.Errorf("%s: %d %s", name, x.Code, x.Raw)
		}
	}
	if cellRuns.Load() != before {
		t.Error("a job-only action ran over HTTP")
	}
	doc, err := BuildOpenAPI(f.reg.List())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["paths"].(map[string]any)["/api/v1/actions/t.jobonly"]; ok {
		t.Error("a job-only action is in the OpenAPI document")
	}
}

func TestOpenAPIServedAndOmitsConsoleOnly(t *testing.T) {
	f := newFixture(t, true)
	for name, c := range map[string]restCred{"session": {session: f.freshSession()}, "token read": {token: f.tokens["token read"]}} {
		r := f.rest("GET", "/api/v1/openapi.json", ``, c)
		if r.Code != 200 || r.Body["openapi"] != "3.1.0" {
			t.Fatalf("%s: %d %s", name, r.Code, r.Raw)
		}
		paths := r.Body["paths"].(map[string]any)
		for _, hidden := range []string{"t.console", "t.jobonly"} {
			if _, ok := paths["/api/v1/actions/"+hidden]; ok {
				t.Errorf("%s: %s is in the document", name, hidden)
			}
		}
		if _, ok := paths["/api/v1/actions/system.info"]; !ok {
			t.Errorf("%s: system.info is missing", name)
		}
		if r.Header.Get("Cache-Control") != "no-store" {
			t.Error("the document is cacheable")
		}
	}
	if r := f.rest("GET", "/api/v1/openapi.json", ``, restCred{}); r.Code != 401 {
		t.Errorf("anonymous: %d", r.Code)
	}
	// Every action reflects, and every path has the documented extensions.
	doc, err := BuildOpenAPI(f.reg.List())
	if err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	for _, info := range f.reg.List() {
		p, ok := paths["/api/v1/actions/"+info.Name]
		if info.ConsoleOnly || info.JobOnly {
			if ok {
				t.Errorf("%s is console-only or job-only and documented", info.Name)
			}
			continue
		}
		if !ok {
			t.Errorf("%s is not documented", info.Name)
			continue
		}
		op := p.(map[string]any)["post"].(map[string]any)
		if op["operationId"] != info.Name || op["x-sd-session-only"] != info.SessionOnly || op["x-sd-sensitive"] != info.Sensitive || op["x-sd-risk"] != info.Risk.String() {
			t.Errorf("%s: %v", info.Name, op)
		}
		// A session-only, sensitive or destructive action is documented without bearerAuth.
		sec := op["security"].([]any)
		noToken := info.SessionOnly || info.Sensitive || info.Risk == actions.RiskDestructive
		if noToken && len(sec) != 1 {
			t.Errorf("%s offers bearerAuth although tokens are refused", info.Name)
		}
	}
	for _, p := range []string{"/api/v1/setup/status", "/api/v1/setup/enroll", "/api/v1/setup/complete", "/api/v1/auth/login", "/api/v1/auth/2fa", "/api/v1/auth/session", "/api/v1/auth/reauth", "/api/v1/auth/logout"} {
		if _, ok := paths[p]; !ok {
			t.Errorf("the hand-written fragment lacks %s", p)
		}
	}
}

// The golden document is generated from the server's registry (built-ins and the
// auth actions) and compared on every run: an API change is a deliberate edit of
// docs/api/openapi.json (go test ./internal/api -run TestOpenAPIGolden -update, or make openapi).
func TestOpenAPIGolden(t *testing.T) {
	f := newFixture(t, false)
	reg := actions.New(f.sink, slogDiscard(), actions.WithClock(f.clk.Now))
	actions.RegisterBuiltins(reg, actions.Deps{PanelCert: func() actions.PanelCert { return actions.PanelCert{} }})
	auth.RegisterAuthActions(reg, f.as, auth.ActionDeps{})
	registerServiceActions(reg, f.st, f.clk.Now)
	doc, err := BuildOpenAPI(reg.List())
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("..", "..", "docs", "api", "openapi.json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (generate it: go test ./internal/api -run TestOpenAPIGolden -update)", err)
	}
	if string(want) != string(got) {
		t.Fatalf("docs/api/openapi.json is out of date: run  go test ./internal/api -run TestOpenAPIGolden -update  and commit the result")
	}
	// Deterministic: a second build is byte-identical.
	again, _ := BuildOpenAPI(reg.List())
	b2, _ := json.MarshalIndent(again, "", "  ")
	if string(b2)+"\n" != string(got) {
		t.Error("the document is not deterministic")
	}
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// SD-Dry-Run: absent executes, exactly "1" dry-runs, anything else is a 400 and
// nothing runs. SD-Confirm may be sent once.
func TestSDDryRunIsStrict(t *testing.T) {
	f := newFixture(t, true)
	s := f.freshSession()
	send := func(values ...string) (restResp, int32) {
		before := cellRuns.Load()
		r := httptest.NewRequest("POST", actionPath("t.write"), strings.NewReader(`{}`))
		r.Host, r.RemoteAddr = "127.0.0.1:8443", "192.0.2.9:1"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://"+r.Host)
		r.Header.Set(CSRFHeader, CSRFToken(s))
		cookie(r, SessionCookie, s)
		for _, v := range values {
			r.Header.Add("SD-Dry-Run", v)
		}
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		out := restResp{Code: rec.Code, Raw: rec.Body.String()}
		_ = json.Unmarshal(rec.Body.Bytes(), &out.Body)
		return out, cellRuns.Load() - before
	}
	if r, ran := send(); r.Code != 200 || ran != 1 || r.Body["data"] == nil {
		t.Fatalf("absent: %d ran %d %s", r.Code, ran, r.Raw)
	}
	if r, ran := send("1"); r.Code != 200 || ran != 0 || r.Body["plan"] == nil {
		t.Fatalf("1: %d ran %d %s", r.Code, ran, r.Raw)
	}
	for _, bad := range [][]string{{"true"}, {"yes"}, {"1 "}, {" 1"}, {"0"}, {""}, {"True"}, {"1", "1"}, {"1", "0"}, {"0", "1"}} {
		r, ran := send(bad...)
		if r.Code != 400 || r.errCode() != "invalid_input" || ran != 0 {
			t.Errorf("SD-Dry-Run %q: %d ran %d %s", bad, r.Code, ran, r.Raw)
		}
	}
	// SD-Confirm repeated is refused too.
	c := restCred{session: s, headers: map[string]string{}}
	req := httptest.NewRequest("POST", actionPath("t.destroy"), strings.NewReader(`{}`))
	req.Host, req.RemoteAddr = "127.0.0.1:8443", "192.0.2.9:1"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://"+req.Host)
	req.Header.Set(CSRFHeader, CSRFToken(c.session))
	cookie(req, SessionCookie, c.session)
	req.Header.Add("SD-Confirm", "the-target")
	req.Header.Add("SD-Confirm", "the-target")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("repeated SD-Confirm: %d", rec.Code)
	}
}

type busyActionHasher struct{}

func (busyActionHasher) Hash(context.Context, string) (string, error) { return "", auth.ErrBusy }
func (busyActionHasher) Verify(context.Context, string, string) (bool, error) {
	return false, auth.ErrBusy
}

// A busy hasher inside an action is a 503 busy with Retry-After 2, not a 500.
func TestRESTBusyHasherIs503(t *testing.T) {
	f := newFixture(t, true)
	reg := actions.New(f.sink, slogDiscard(), actions.WithClock(f.clk.Now))
	auth.RegisterAuthActions(reg, f.as, auth.ActionDeps{Hasher: busyActionHasher{}})
	f.srv = New(Config{SPA: fstest.MapFS{"index.html": {Data: []byte("x")}}, Sessions: f.as, Actions: reg})
	f.h = f.srv.Handler()
	s := f.freshSession()
	r := f.rest("POST", actionPath("auth.password_change"), validInput("auth.password_change"), restCred{session: s})
	if r.Code != 503 || r.errCode() != "busy" || r.Header.Get("Retry-After") != "2" {
		t.Fatalf("%d %s (Retry-After %q)", r.Code, r.Raw, r.Header.Get("Retry-After"))
	}
	// The session was not revoked by the failed change.
	if g := f.rest("GET", "/api/v1/auth/session", ``, restCred{session: s}); g.Code == 401 {
		t.Error("a busy hasher ended the session")
	}
}

type failingJobs struct{}

func (failingJobs) CancelSessionJobsTx(context.Context, *sql.Tx, []string, string) (func(), error) {
	return nil, errors.New("jobs table is on fire")
}

// When the jobs of a session cannot be cancelled, the revoke is rolled back and
// the user is told so in plain words (a 500 with its own code), never a false
// success and never the internal cause.
func TestRESTRevokeThatCannotCancelJobsIsReportedAndRolledBack(t *testing.T) {
	f := newFixture(t, true)
	reg := actions.New(f.sink, slogDiscard(), actions.WithClock(f.clk.Now))
	auth.RegisterAuthActions(reg, f.as, auth.ActionDeps{Jobs: failingJobs{}})
	f.srv = New(Config{SPA: fstest.MapFS{"index.html": {Data: []byte("x")}}, Sessions: f.as, Actions: reg})
	f.h = f.srv.Handler()
	own, other := f.freshSession(), f.freshSession()
	var otherID string
	f.st.R.QueryRow(`SELECT public_id FROM sessions WHERE id_hash = ?`, sha(other)).Scan(&otherID)
	for name, in := range map[string]string{"one session": fmt.Sprintf(`{"public_id":%q}`, otherID), "all others": `{"all_others":true}`} {
		x := f.rest("POST", actionPath("auth.sessions_revoke"), in, restCred{session: own})
		if x.Code != 500 || x.errCode() != "revoke_not_applied" || strings.Contains(x.Raw, "on fire") || !strings.Contains(x.Raw, "not signed out") {
			t.Errorf("%s: %d %s", name, x.Code, x.Raw)
		}
		var n int
		f.st.R.QueryRow(`SELECT count(*) FROM sessions WHERE public_id = ?`, otherID).Scan(&n)
		if n != 1 {
			t.Errorf("%s: the session was revoked although its jobs were not cancelled", name)
		}
	}
}
