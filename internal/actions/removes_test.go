package actions

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
)

// downSink is an audit sink whose intent writes fail on demand.
type downSink struct {
	*audit.Logger
	down atomic.Bool
}

func (d *downSink) Append(c context.Context, e audit.Entry) (int64, error) {
	if d.down.Load() {
		return 0, errors.New("disk full")
	}
	return d.Logger.Append(c, e)
}

type secretIn struct {
	Name string `json:"name"`
	Pass string `json:"pass" sd:"secret"`
}

func (secretIn) Validate() error { return nil }

func TestRemovesAccessActionRunsWhenTheAuditLogIsDown(t *testing.T) {
	e := newEnv(t)
	sink := &downSink{Logger: e.log}
	r := New(sink, quiet, WithClock(e.clk.Now))
	var ran atomic.Int32
	Register(r, Def[in, string]{Name: "t.revoke", Summary: "x", Risk: RiskMutate, Scope: ScopeWrite, RemovesAccess: true, Target: target,
		Execute: func(context.Context, Call, in) (string, error) { ran.Add(1); return "revoked", nil }})
	Register(r, Def[in, string]{Name: "t.grant", Summary: "x", Risk: RiskMutate, Scope: ScopeWrite, Target: target,
		Execute: func(context.Context, Call, in) (string, error) { ran.Add(1); return "granted", nil }})
	actor := UserActor("1", ScopeAdmin, e.fresh())

	// Log up: both run and are audited.
	if res, err := r.Invoke(ctx, actor, "t.revoke", raw("a"), Options{}); err != nil || !res.Audited || !res.AuditOutcomeRecorded {
		t.Fatalf("%+v %v", res, err)
	}
	ran.Store(0)
	sink.down.Store(true)
	// Log down: an ordinary mutation is refused and does not run...
	if _, err := r.Invoke(ctx, actor, "t.grant", raw("a"), Options{}); !errors.Is(err, audit.ErrAuditUnavailable) || ran.Load() != 0 {
		t.Fatalf("grant with the log down: %v, ran %d", err, ran.Load())
	}
	// ...an access-removing one runs, and says the record is missing.
	res, err := r.Invoke(ctx, actor, "t.revoke", raw("a"), Options{})
	if !errors.Is(err, audit.ErrAuditOutcome) || res.Out != "revoked" || ran.Load() != 1 || res.Audited || res.AuditOutcomeRecorded {
		t.Fatalf("revoke with the log down: %+v %v ran %d", res, err, ran.Load())
	}
	// Policy still applies: a token with too little scope cannot revoke.
	if _, err := r.Invoke(ctx, TokenActor("t", ScopeRead), "t.revoke", raw("a"), Options{}); !errors.Is(err, ErrDenied) {
		t.Errorf("%v", err)
	}
	// RemovesAccess is for mutate or destructive actions.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("RemovesAccess on a read was accepted")
			}
		}()
		Register(r, Def[in, string]{Name: "t.readrev", Summary: "x", Risk: RiskRead, Scope: ScopeRead, RemovesAccess: true,
			Execute: func(context.Context, Call, in) (string, error) { return "", nil }})
	}()
}

// An execution error never carries a secret from the input.
func TestExecutionErrorsAreRedactedBeforeInvokeReturnsThem(t *testing.T) {
	e := newEnv(t)
	r := New(e.log, quiet, WithClock(e.clk.Now))
	sentinel := errors.New("sentinel")
	Register(r, Def[secretIn, string]{Name: "t.leaky", Summary: "x", Risk: RiskMutate, Scope: ScopeWrite,
		Target: func(i secretIn) string { return i.Name },
		Execute: func(_ context.Context, _ Call, i secretIn) (string, error) {
			return "", errors.Join(sentinel, errors.New("backend refused password "+i.Pass))
		}})
	Register(r, Def[secretIn, string]{Name: "t.leakyread", Summary: "x", Risk: RiskRead, Scope: ScopeRead,
		Execute: func(_ context.Context, _ Call, i secretIn) (string, error) {
			return "", errors.New("read failed for " + i.Pass)
		}})
	for _, name := range []string{"t.leaky", "t.leakyread"} {
		_, err := r.Invoke(ctx, UserActor("1", ScopeAdmin, e.fresh()), name, []byte(`{"name":"n","pass":"hunter2-sentinel"}`), Options{})
		if err == nil || strings.Contains(err.Error(), "hunter2-sentinel") {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "t.leaky" && !errors.Is(err, sentinel) {
			t.Error("the cause is no longer visible to errors.Is")
		}
	}
}
