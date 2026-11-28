package actions

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRegisterRefusesJobOnlyWithSystemReason(t *testing.T) {
	e := newEnv(t)
	base := Def[in, string]{Name: "t.job_bad", Summary: "x", Risk: RiskMutate, Scope: ScopeWrite, Target: target, JobOnly: true,
		Execute: exec0[in]}
	withSystem := base
	withSystem.System = "a job never runs as the system"
	mustPanic(t, "JobOnly with a System reason", func() { Register(e.reg, withSystem) })
	both := base
	both.ConsoleOnly, both.System = true, "x"
	mustPanic(t, "JobOnly and ConsoleOnly", func() { Register(e.reg, both) })
	Register(e.reg, base) // JobOnly alone is fine
	if info, ok := e.reg.Lookup("t.job_bad"); !ok || !info.JobOnly {
		t.Errorf("Info does not report JobOnly: %+v", info)
	}
}

// A JobOnly action runs only through InvokeAs for a non-system actor, under that
// actor's own policy; every other way in is denied and nothing executes.
func TestJobOnlyActionsRunOnlyThroughInvokeAs(t *testing.T) {
	e := newEnv(t)
	e.add("t.job_run", Def[in, string]{Risk: RiskMutate, Scope: ScopeWrite, Target: target, JobOnly: true})
	inv := e.reg.SystemInvoker()
	fresh := e.fresh()
	for name, a := range map[string]Actor{
		"user admin": UserActor("1", ScopeAdmin, fresh), "token admin": TokenActor("t", ScopeAdmin), "mcp": MCPActor("m"), "webhook": WebhookActor("h", "app:x"),
	} {
		if _, err := e.reg.Invoke(ctx, a, "t.job_run", raw("x"), Options{}); !isDenied(err) || !strings.Contains(err.Error(), "job runner") {
			t.Errorf("Invoke as %s: %v", name, err)
		}
		if err := e.reg.Authorize(ctx, a, "t.job_run", raw("x"), Options{}); !isDenied(err) {
			t.Errorf("Authorize as %s: %v", name, err)
		}
	}
	if _, err := inv.Invoke(ctx, "t.job_run", raw("x"), Options{}); !isDenied(err) {
		t.Errorf("the system actor through Invoke: %v", err)
	}
	if _, err := inv.InvokeAs(ctx, Actor{kind: KindSystem, id: "system"}, "t.job_run", raw("x"), Options{}); !isDenied(err) {
		t.Errorf("InvokeAs with the system actor: %v", err)
	}
	if n := e.execs["t.job_run"].Load(); n != 0 {
		t.Fatalf("a refused call executed %d times", n)
	}
	// The queuing actor's own policy still applies at run time.
	if _, err := inv.InvokeAs(ctx, UserActor("1", ScopeRead, fresh), "t.job_run", raw("x"), Options{}); !isDenied(err) {
		t.Errorf("a read user through InvokeAs: %v", err)
	}
	if _, err := inv.InvokeAs(ctx, TokenActor("t", ScopeWrite), "t.job_run", raw("x"), Options{}); err != nil {
		t.Fatalf("a write token through InvokeAs: %v", err)
	}
}

func TestJobAuditRowsCarryTheActorWithExecutorSystem(t *testing.T) {
	e := newEnv(t)
	e.add("t.job_run", Def[in, string]{Risk: RiskMutate, Scope: ScopeWrite, Target: target, JobOnly: true})
	inv := e.reg.SystemInvoker()
	if _, err := inv.InvokeAs(ctx, TokenActor("tok-3", ScopeWrite), "t.job_run", raw("x"), Options{RequestID: "job-7-a1"}); err != nil {
		t.Fatal(err)
	}
	rows, err := e.st.R.Query(`SELECT actor_kind, actor_id, COALESCE(executor, ''), phase, request_id FROM audit_log WHERE action = 't.job_run' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var kind, id, ex, phase, req string
		if err := rows.Scan(&kind, &id, &ex, &phase, &req); err != nil {
			t.Fatal(err)
		}
		n++
		if kind != "token" || id != "tok-3" || ex != "system" || req != "job-7-a1" {
			t.Errorf("%s row: %s/%s executor=%q request=%s", phase, kind, id, ex, req)
		}
	}
	if n < 2 {
		t.Errorf("%d audit rows, want intent and outcome", n)
	}
}

func TestActorFromJobRefusesSystem(t *testing.T) {
	e := newEnv(t)
	inv := e.reg.SystemInvoker()
	sys := Actor{kind: KindSystem, id: "system"}
	if sys.JobBlob() != "" {
		t.Error("the system actor has a job blob")
	}
	if _, err := inv.ActorFromJob(`{"v":1,"k":"system","id":"system"}`); err != ErrSystemJobActor {
		t.Errorf("system blob: %v", err)
	}
	for name, blob := range map[string]string{
		"empty": "", "not json": "{", "unknown kind": `{"v":1,"k":"root","id":"1","s":3}`, "unknown version": `{"v":2,"k":"user","id":"1","s":3}`,
		"no id": `{"v":1,"k":"user","id":"","s":3}`, "no scope": `{"v":1,"k":"user","id":"1"}`, "scope too high": `{"v":1,"k":"token","id":"1","s":9}`,
		"unknown field": `{"v":1,"k":"user","id":"1","s":3,"x":1}`, "trailing": `{"v":1,"k":"user","id":"1","s":3} {}`,
		"stray brace": `{"v":1,"k":"user","id":"1","s":3}}`, "stray bracket": `{"v":1,"k":"user","id":"1","s":3}]`, "huge": strings.Repeat("x", maxJobBlob+1),
	} {
		if _, err := inv.ActorFromJob(blob); err != ErrBadJobActor {
			t.Errorf("%s: %v (want the fixed ErrBadJobActor, no decoder text)", name, err)
		}
	}
}

func TestJobBlobRoundTrip(t *testing.T) {
	e := newEnv(t)
	inv := e.reg.SystemInvoker()
	until := time.Date(2026, 11, 20, 12, 5, 0, 0, time.UTC)
	for name, a := range map[string]Actor{
		"user":  UserSessionActor("42", "pub-1", ScopeAdmin, until).WithHostKind("ip"),
		"token": TokenActor("9", ScopeWrite),
		"mcp":   MCPActor("5"),
	} {
		b := a.JobBlob()
		got, err := inv.ActorFromJob(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != a {
			t.Errorf("%s: %+v != %+v", name, got, a)
		}
		if strings.ContainsAny(b, "\n") || len(b) > 256 {
			t.Errorf("%s: blob %q", name, b)
		}
	}
}

func isDenied(err error) bool { return errors.Is(err, ErrDenied) }
