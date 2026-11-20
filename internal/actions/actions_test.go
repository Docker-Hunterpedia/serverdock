package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

var ctx = context.Background()
var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type in struct {
	Name   string `json:"name"`
	Secret string `json:"secret,omitempty" sd:"secret"`
}

func (i in) Validate() error {
	if i.Name == "" {
		return errors.New("name is required")
	}
	return nil
}

func target(i in) string { return "app:" + i.Name }

// env is a registry over a real store and audit log, with one action per cell of
// the policy table, each counting its executions.
type env struct {
	reg   *Registry
	st    *store.Store
	log   *audit.Logger
	clk   *clock
	execs map[string]*atomic.Int32
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &clock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	al, err := audit.New(st, quiet, audit.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{st: st, log: al, clk: clk, execs: map[string]*atomic.Int32{}}
	e.reg = New(al, quiet, WithClock(clk.Now))
	e.add("t.read", Def[in, string]{Risk: RiskRead, Scope: ScopeRead})
	e.add("t.sensitive", Def[in, string]{Risk: RiskRead, Scope: ScopeAdmin, Sensitive: true, Target: target})
	e.add("t.mutate", Def[in, string]{Risk: RiskMutate, Scope: ScopeWrite, Target: target})
	e.add("t.mutate_hook", Def[in, string]{Risk: RiskMutate, Scope: ScopeWrite, Target: target, WebhookAllowed: true})
	e.add("t.mutate_sys", Def[in, string]{Risk: RiskMutate, Scope: ScopeWrite, Target: target, System: "scheduled cleanup"})
	e.add("t.destroy", Def[in, string]{Risk: RiskDestructive, Scope: ScopeAdmin, Target: target})
	e.add("t.destroy_sys", Def[in, string]{Risk: RiskDestructive, Scope: ScopeAdmin, Target: target, System: "retention sweep"})
	e.add("t.session", Def[in, string]{Risk: RiskMutate, Scope: ScopeWrite, Target: target, SessionOnly: true})
	return e
}

func (e *env) add(name string, d Def[in, string]) {
	d.Name, d.Summary = name, "test action"
	cnt := &atomic.Int32{}
	e.execs[name] = cnt
	d.Execute = func(_ context.Context, _ Call, i in) (string, error) {
		cnt.Add(1)
		return "did " + i.Name, nil
	}
	d.DryRun = func(_ context.Context, _ Call, i in) (Plan, error) { return Plan{Summary: "would " + i.Name}, nil }
	Register(e.reg, d)
}

func raw(name string) json.RawMessage { return json.RawMessage(fmt.Sprintf(`{"name":%q}`, name)) }

func (e *env) fresh() time.Time { return e.clk.Now().Add(5 * time.Minute) }

func TestPolicyMatrix(t *testing.T) {
	e := newEnv(t)
	userAdmin := UserActor("u-admin", ScopeAdmin, e.fresh())
	userStale := UserActor("u-stale", ScopeAdmin, e.clk.Now().Add(-time.Minute))
	userWrite := UserActor("u-write", ScopeWrite, e.fresh())
	userRead := UserActor("u-read", ScopeRead, e.fresh())
	tokAdmin := TokenActor("tok-admin", ScopeAdmin)
	tokWrite := TokenActor("tok-write", ScopeWrite)
	tokRead := TokenActor("tok-read", ScopeRead)
	mcp := MCPActor("mcp-1")
	hook := WebhookActor("hook-1", "app:web")
	hookOther := WebhookActor("hook-2", "app:other")
	sys := Actor{kind: KindSystem, id: "system"} // only the SystemInvoker builds this outside the package

	type cell struct {
		actor   Actor
		action  string
		confirm string
		allow   bool
	}
	cells := []cell{
		// read
		{userRead, "t.read", "", true}, {tokRead, "t.read", "", true}, {mcp, "t.read", "", true}, {hook, "t.read", "", false}, {sys, "t.read", "", true},
		// sensitive (admin, read risk): user needs scope + fresh MFA; everything else but system denied
		{userAdmin, "t.sensitive", "", true}, {userStale, "t.sensitive", "", false}, {userWrite, "t.sensitive", "", false},
		{tokAdmin, "t.sensitive", "", false}, {mcp, "t.sensitive", "", false}, {hook, "t.sensitive", "", false}, {sys, "t.sensitive", "", true},
		// mutate
		{userWrite, "t.mutate", "", true}, {userRead, "t.mutate", "", false}, {tokWrite, "t.mutate", "", true}, {tokRead, "t.mutate", "", false},
		{mcp, "t.mutate", "", false}, {hook, "t.mutate", "", false}, {sys, "t.mutate", "", false},
		{hook, "t.mutate_hook", "", true}, {hookOther, "t.mutate_hook", "", false}, {mcp, "t.mutate_hook", "", false},
		{sys, "t.mutate_sys", "", true}, {userWrite, "t.mutate_sys", "", true},
		// destructive: user needs scope + fresh MFA + the exact target
		{userAdmin, "t.destroy", "app:web", true}, {userAdmin, "t.destroy", "", false}, {userAdmin, "t.destroy", "app:wrong", false},
		{userStale, "t.destroy", "app:web", false}, {userWrite, "t.destroy", "app:web", false},
		{tokAdmin, "t.destroy", "app:web", false}, {mcp, "t.destroy", "app:web", false}, {hook, "t.destroy", "app:web", false},
		{sys, "t.destroy", "", false}, {sys, "t.destroy_sys", "", true},
		// SessionOnly refuses everyone but a user
		{userWrite, "t.session", "", true}, {tokAdmin, "t.session", "", false}, {tokWrite, "t.session", "", false},
		{mcp, "t.session", "", false}, {hook, "t.session", "", false}, {sys, "t.session", "", false},
	}
	for _, c := range cells {
		name := fmt.Sprintf("%s/%s/%s", string(c.actor.kind)+":"+c.actor.id, c.action, c.confirm)
		t.Run(name, func(t *testing.T) {
			before := e.execs[c.action].Load()
			nm := "web"
			res, err := e.reg.invoke(ctx, c.actor, "", c.action, raw(nm), Options{Confirm: c.confirm})
			ran := e.execs[c.action].Load() - before
			if c.allow {
				if err != nil || ran != 1 || res.Out != "did web" {
					t.Fatalf("expected allow: out=%v err=%v ran=%d", res.Out, err, ran)
				}
				return
			}
			if !errors.Is(err, ErrDenied) || ran != 0 {
				t.Fatalf("expected deny: err=%v ran=%d", err, ran)
			}
			var de *DeniedError
			if !errors.As(err, &de) || de.Reason == "" {
				t.Errorf("denied without a reason: %v", err)
			}
		})
	}
	// The zero Actor is no one.
	if _, err := e.reg.Invoke(ctx, Actor{}, "t.read", raw("x"), Options{}); !errors.Is(err, ErrDenied) {
		t.Errorf("zero actor: %v", err)
	}
}

func TestDeniedAttemptsAreAuditedAndRateLimited(t *testing.T) {
	e := newEnv(t)
	tok := TokenActor("tok-1", ScopeAdmin)
	for i := 0; i < 15; i++ {
		_, _ = e.reg.Invoke(ctx, tok, "t.destroy", raw("web"), Options{RequestID: "rq"})
	}
	var n int
	var kind, id, result, phase string
	e.st.R.QueryRow(`SELECT count(*) FROM audit_log WHERE phase='denied'`).Scan(&n)
	e.st.R.QueryRow(`SELECT actor_kind, actor_id, result, phase FROM audit_log WHERE phase='denied' LIMIT 1`).Scan(&kind, &id, &result, &phase)
	if n != 10 || kind != "token" || id != "tok-1" || result != "denied" {
		t.Fatalf("denied rows: n=%d %s/%s %s", n, kind, id, result)
	}
	if e.execs["t.destroy"].Load() != 0 {
		t.Error("a denied action ran")
	}
}

func TestSessionOnlyRefusesTokens(t *testing.T) {
	e := newEnv(t)
	if _, err := e.reg.Invoke(ctx, TokenActor("t", ScopeAdmin), "t.session", raw("x"), Options{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("token on a SessionOnly action: %v", err)
	}
	if _, err := e.reg.Invoke(ctx, UserActor("u", ScopeWrite, e.fresh()), "t.session", raw("x"), Options{}); err != nil {
		t.Fatalf("user on a SessionOnly action: %v", err)
	}
}

func TestSecondSystemInvokerPanics(t *testing.T) {
	e := newEnv(t)
	_ = e.reg.SystemInvoker()
	defer func() {
		if recover() == nil {
			t.Fatal("a second SystemInvoker() did not panic")
		}
	}()
	e.reg.SystemInvoker()
}

func TestSystemInvokerRunsAsTheSystemActor(t *testing.T) {
	e := newEnv(t)
	inv := e.reg.SystemInvoker()
	if _, err := inv.Invoke(ctx, "t.mutate_sys", raw("web"), Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := inv.Invoke(ctx, "t.mutate", raw("web"), Options{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("system without a System reason: %v", err)
	}
	var kind, id string
	var ex *string
	if err := e.st.R.QueryRow(`SELECT actor_kind, actor_id, executor FROM audit_log WHERE action='t.mutate_sys' AND phase='intent'`).Scan(&kind, &id, &ex); err != nil {
		t.Fatal(err)
	}
	if kind != "system" || id != "system" || ex != nil {
		t.Errorf("system row: %s/%s executor=%v", kind, id, ex)
	}
}

// A job runs as the actor that queued it: the policy is checked at enqueue
// (Authorize) and again, now, for each action the job runs (InvokeAs); the audit
// row records the queuing actor with executor = 'system'.
func TestInvokeAsChecksTheOriginalActorAtEnqueueAndAtRunTime(t *testing.T) {
	e := newEnv(t)
	inv := e.reg.SystemInvoker()
	userAdmin := UserActor("u1", ScopeAdmin, e.fresh())
	opts := Options{Confirm: "app:web"}

	// Enqueue time: allowed now.
	if err := e.reg.Authorize(ctx, userAdmin, "t.destroy", raw("web"), opts); err != nil {
		t.Fatalf("enqueue check: %v", err)
	}
	if e.execs["t.destroy"].Load() != 0 {
		t.Fatal("Authorize executed the action")
	}
	// Run time, while still fresh: allowed, recorded with executor 'system'.
	if _, err := inv.InvokeAs(ctx, userAdmin, "t.destroy", raw("web"), opts); err != nil {
		t.Fatalf("run while fresh: %v", err)
	}
	var kind, id, executor string
	if err := e.st.R.QueryRow(`SELECT actor_kind, actor_id, executor FROM audit_log WHERE action='t.destroy' AND phase='intent'`).Scan(&kind, &id, &executor); err != nil {
		t.Fatal(err)
	}
	if kind != "user" || id != "u1" || executor != "system" {
		t.Errorf("row: %s/%s executor=%s", kind, id, executor)
	}
	// Later the 2FA window has expired: the same queued job is refused at run time.
	e.clk.Add(10 * time.Minute)
	if _, err := inv.InvokeAs(ctx, userAdmin, "t.destroy", raw("web"), opts); !errors.Is(err, ErrDenied) {
		t.Fatalf("run after the 2FA window: %v", err)
	}
	if e.execs["t.destroy"].Load() != 1 {
		t.Errorf("executions = %d, want 1", e.execs["t.destroy"].Load())
	}
}

// The job-executor marker never widens what an actor may do: a token running a
// destructive action through InvokeAs is denied, and the denied row records the
// token actor with executor = 'system'.
func TestInvokeAsDoesNotWidenATokensPolicy(t *testing.T) {
	e := newEnv(t)
	inv := e.reg.SystemInvoker()
	tok := TokenActor("tok-9", ScopeAdmin)
	if err := e.reg.Authorize(ctx, tok, "t.destroy", raw("web"), Options{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("enqueue check: %v", err)
	}
	if _, err := inv.InvokeAs(ctx, tok, "t.destroy", raw("web"), Options{Confirm: "app:web"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("InvokeAs: %v", err)
	}
	if e.execs["t.destroy"].Load() != 0 {
		t.Error("the action ran")
	}
	var kind, id, executor, phase string
	if err := e.st.R.QueryRow(`SELECT actor_kind, actor_id, executor, phase FROM audit_log WHERE action='t.destroy' ORDER BY id DESC LIMIT 1`).Scan(&kind, &id, &executor, &phase); err != nil {
		t.Fatal(err)
	}
	if kind != "token" || id != "tok-9" || executor != "system" || phase != "denied" {
		t.Errorf("row: %s/%s executor=%s phase=%s", kind, id, executor, phase)
	}
}

func TestInputIsDecodedStrictlyAndValidated(t *testing.T) {
	e := newEnv(t)
	u := UserActor("u", ScopeAdmin, e.fresh())
	bad := []json.RawMessage{
		json.RawMessage(`{"name":"x","extra":1}`),
		json.RawMessage(`{"name":"x"} {"name":"y"}`),
		json.RawMessage(`{"name":"x"} trailing`),
		json.RawMessage(`{"name":`),
		json.RawMessage(`{}`),                                                // Validate: name required
		json.RawMessage(`{"name":5}`),                                        // wrong type
		json.RawMessage(strings.Repeat(" ", MaxInputBytes) + `{"name":"x"}`), // over the size cap
	}
	for i, b := range bad {
		if _, err := e.reg.Invoke(ctx, u, "t.mutate", b, Options{}); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("case %d: %v", i, err)
		}
	}
	if e.execs["t.mutate"].Load() != 0 {
		t.Error("invalid input reached Execute")
	}
	if _, err := e.reg.Invoke(ctx, u, "no.such", raw("x"), Options{}); !errors.Is(err, ErrUnknownAction) {
		t.Errorf("unknown action: %v", err)
	}
}

func TestDryRunNeverExecutes(t *testing.T) {
	e := newEnv(t)
	u := UserActor("u", ScopeAdmin, e.fresh())
	res, err := e.reg.Invoke(ctx, u, "t.destroy", raw("web"), Options{DryRun: true, Confirm: "app:web"})
	if err != nil || res.Plan == nil || res.Plan.Summary != "would web" || res.Out != nil {
		t.Fatalf("%+v %v", res, err)
	}
	if e.execs["t.destroy"].Load() != 0 {
		t.Fatal("a dry run executed")
	}
	var dry int
	e.st.R.QueryRow(`SELECT dry_run FROM audit_log WHERE action='t.destroy' AND phase='intent'`).Scan(&dry)
	if dry != 1 {
		t.Error("the audit row does not say dry run")
	}
	// An action without a DryRun refuses it, still without executing.
	st := newEnv(t)
	Register(st.reg, Def[in, string]{Name: "t.nodry", Summary: "x", Risk: RiskMutate, Scope: ScopeWrite, Target: target,
		Execute: func(context.Context, Call, in) (string, error) { t.Fatal("executed"); return "", nil }})
	if _, err := st.reg.Invoke(ctx, UserActor("u", ScopeWrite, st.fresh()), "t.nodry", raw("x"), Options{DryRun: true}); !errors.Is(err, ErrNoDryRun) {
		t.Errorf("no dry run: %v", err)
	}
}

func TestPanicBecomesAnErrorOutcome(t *testing.T) {
	e := newEnv(t)
	Register(e.reg, Def[in, string]{Name: "t.boom", Summary: "x", Risk: RiskMutate, Scope: ScopeWrite, Target: target,
		Execute: func(context.Context, Call, in) (string, error) { panic("kaboom") }})
	res, err := e.reg.Invoke(ctx, UserActor("u", ScopeWrite, e.fresh()), "t.boom", raw("x"), Options{})
	if err == nil || !strings.Contains(err.Error(), "panicked") || !res.Audited {
		t.Fatalf("%+v %v", res, err)
	}
	var result string
	e.st.R.QueryRow(`SELECT result FROM audit_log WHERE action='t.boom' AND phase='outcome'`).Scan(&result)
	if result != "error" {
		t.Errorf("outcome %q", result)
	}
	// A plain read that panics is also an error, not a crash.
	Register(e.reg, Def[in, string]{Name: "t.boomread", Summary: "x", Risk: RiskRead, Scope: ScopeRead,
		Execute: func(context.Context, Call, in) (string, error) { panic("read kaboom") }})
	if _, err := e.reg.Invoke(ctx, TokenActor("t", ScopeRead), "t.boomread", raw("x"), Options{}); err == nil {
		t.Error("a panicking read returned no error")
	}
}

type failingSink struct {
	inner  Sink
	failAt int
	calls  int
}

func (f *failingSink) Append(ctx context.Context, e audit.Entry) (int64, error) {
	f.calls++
	if f.calls == f.failAt {
		return 0, errors.New("disk full")
	}
	return f.inner.Append(ctx, e)
}
func (f *failingSink) Denied(ctx context.Context, e audit.Entry) (bool, error) {
	return f.inner.Denied(ctx, e)
}

func TestSensitiveReadWhoseIntentWriteFailsRevealsNothing(t *testing.T) {
	e := newEnv(t)
	reveal := &atomic.Int32{}
	reg := New(&failingSink{inner: e.log, failAt: 1}, quiet, WithClock(e.clk.Now))
	Register(reg, Def[in, string]{Name: "secret.reveal", Summary: "x", Risk: RiskRead, Scope: ScopeAdmin, Sensitive: true, Target: target,
		Execute: func(context.Context, Call, in) (string, error) { reveal.Add(1); return "THE-SECRET", nil }})
	res, err := reg.Invoke(ctx, UserActor("u", ScopeAdmin, e.fresh()), "secret.reveal", raw("db"), Options{})
	if !errors.Is(err, audit.ErrAuditUnavailable) || res.Out != nil || reveal.Load() != 0 {
		t.Fatalf("out=%v err=%v reveals=%d", res.Out, err, reveal.Load())
	}
	// A failed outcome write still returns the result, flagged.
	reg2 := New(&failingSink{inner: e.log, failAt: 2}, quiet, WithClock(e.clk.Now))
	Register(reg2, Def[in, string]{Name: "t.ok", Summary: "x", Risk: RiskMutate, Scope: ScopeWrite, Target: target,
		Execute: func(context.Context, Call, in) (string, error) { return "done", nil }})
	res, err = reg2.Invoke(ctx, UserActor("u", ScopeWrite, e.fresh()), "t.ok", raw("x"), Options{})
	if !errors.Is(err, audit.ErrAuditOutcome) || res.Out != "done" || res.AuditOutcomeRecorded {
		t.Fatalf("outcome failure: %+v %v", res, err)
	}
}

func TestSentinelSecretNeverReachesTheDatabaseBytes(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	al, _ := audit.New(st, quiet)
	reg := New(al, quiet)
	const sentinel = "SENTINEL-do-not-store-9f3a"
	Register(reg, Def[in, string]{Name: "t.secret", Summary: "x", Risk: RiskMutate, Scope: ScopeWrite, Target: target,
		Execute: func(_ context.Context, _ Call, i in) (string, error) {
			return "", fmt.Errorf("connect failed using %s", i.Secret)
		}})
	_, err = reg.Invoke(ctx, UserActor("u", ScopeWrite, time.Now().Add(time.Hour)), "t.secret",
		json.RawMessage(`{"name":"db","secret":"`+sentinel+`"}`), Options{})
	if err == nil {
		t.Fatal("expected the action to fail")
	}
	st.Close() // checkpoint into the main file
	var all []byte
	files, _ := filepath.Glob(filepath.Join(dir, "serverdock.db*"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		all = append(all, b...)
	}
	if len(all) == 0 {
		t.Fatal("no database files read")
	}
	if strings.Contains(string(all), "SENTINEL-do-not-store") {
		t.Fatal("the secret is in the database bytes")
	}
	if !strings.Contains(string(all), "[redacted]") {
		t.Error("expected the redaction placeholder in the stored rows")
	}
}

func TestBuiltinsAndSystemInfoReturnsTheAuditHead(t *testing.T) {
	e := newEnv(t)
	reg := New(e.log, quiet)
	RegisterBuiltins(reg, Deps{Version: "v-test", Started: time.Now().Add(-90 * time.Second), Audit: e.log, DB: e.st.R,
		Docker: fakeDocker{}})
	// Give the log some rows first.
	for i := 0; i < 3; i++ {
		e.log.Append(ctx, audit.Entry{ActorKind: "user", ActorID: "u", Action: "app.x", Target: "t", Risk: "mutate",
			Phase: audit.PhaseIntent, Result: audit.ResultPending, ArgsJSON: "{}", RequestID: "r"})
	}
	res, err := reg.Invoke(ctx, MCPActor("m"), "system.info", nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	info := res.Out.(SystemInfo)
	h, _ := e.log.Head(ctx)
	if info.AuditRows != 3 || info.AuditHead != h.HashHex() || info.AuditHead == "" || info.Version != "v-test" || info.UptimeSeconds < 89 || info.Goroutines == 0 {
		t.Fatalf("info %+v head %s", info, h.HashHex())
	}
	res, err = reg.Invoke(ctx, MCPActor("m"), "system.docker", nil, Options{})
	if d := res.Out.(DockerInfo); err != nil || !d.Available || d.Engine != "25.0.0" || d.API != "1.44" {
		t.Fatalf("docker %+v %v", res.Out, err)
	}
	// audit.verify is admin-only (a full scan) and reports the chain.
	if _, err := reg.Invoke(ctx, TokenActor("t", ScopeRead), "audit.verify", nil, Options{}); !errors.Is(err, ErrDenied) {
		t.Errorf("audit.verify as a read token: %v", err)
	}
	res, err = reg.Invoke(ctx, TokenActor("t", ScopeAdmin), "audit.verify", nil, Options{})
	if v := res.Out.(AuditVerifyResult); err != nil || !v.OK || v.Rows < 3 || v.Head == "" {
		t.Fatalf("verify %+v %v", res.Out, err)
	}
}

type fakeDocker struct{}

func (fakeDocker) Info(context.Context) (string, string, error) { return "25.0.0", "1.44", nil }

// newFuzzEnv is newEnv with a shared store per fuzz worker process (opening a
// database per input would dominate the run).
var fuzzOnce sync.Once
var fuzzShared *env

func newFuzzEnv(t *testing.T) *env {
	fuzzOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sdfuzz")
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		clk := &clock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
		al, _ := audit.New(st, quiet, audit.WithClock(clk.Now))
		e := &env{st: st, log: al, clk: clk, execs: map[string]*atomic.Int32{}}
		e.reg = New(al, quiet, WithClock(clk.Now))
		e.add("t.mutate", Def[in, string]{Risk: RiskMutate, Scope: ScopeWrite, Target: target})
		fuzzShared = e
	})
	return fuzzShared
}
