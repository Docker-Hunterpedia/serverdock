package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// jobRig runs real jobs (a jobs.Scheduler over the test store) whose run function
// does what the deploy runner will: rebuild the queuing actor, refresh it, and
// invoke a JobOnly action for it through SystemInvoker.InvokeAs.
type jobRig struct {
	a     *aenv
	inv   *actions.SystemInvoker
	js    *jobs.Store
	sched *jobs.Scheduler

	mu      sync.Mutex
	results map[int64]error
	lock    int
	runs    atomic.Int32 // executions of t.job_run
	destroy atomic.Int32 // executions of t.job_destroy
}

func newJobRig(t *testing.T) *jobRig {
	t.Helper()
	r := &jobRig{results: map[int64]error{}}
	defer func() { r.a.clk.Add(10 * time.Second) }() // the setup's own credential time must be before any job
	r.a = newAEnvJobs(t, func(e *env) JobCanceller {
		r.js = jobs.NewStore(e.st, e.clk.Now)
		return jobCancellerFunc(func(ctx context.Context, tx *sql.Tx, sessions []string, reason string) (func(), error) {
			return r.sched.CancelSessionJobsTx(ctx, tx, sessions, reason)
		})
	})
	reg := r.a.reg
	actions.Register(reg, actions.Def[actions.None, string]{Name: "t.job_run", Summary: "x", Risk: actions.RiskMutate, Scope: actions.ScopeWrite, JobOnly: true,
		Target:  func(actions.None) string { return "svc" },
		Execute: func(context.Context, actions.Call, actions.None) (string, error) { r.runs.Add(1); return "ok", nil }})
	actions.Register(reg, actions.Def[actions.None, string]{Name: "t.job_destroy", Summary: "x", Risk: actions.RiskDestructive, Scope: actions.ScopeAdmin, JobOnly: true,
		Target: func(actions.None) string { return "svc" },
		Execute: func(context.Context, actions.Call, actions.None) (string, error) {
			r.destroy.Add(1)
			return "gone", nil
		}})
	r.inv = reg.SystemInvoker()
	sched, err := jobs.NewScheduler(jobs.Config{Store: r.js, Now: r.a.clk.Now, Run: r.run})
	if err != nil {
		t.Fatal(err)
	}
	r.sched = sched
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sched.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

type jobCancellerFunc func(context.Context, *sql.Tx, []string, string) (func(), error)

func (f jobCancellerFunc) CancelSessionJobsTx(ctx context.Context, tx *sql.Tx, s []string, reason string) (func(), error) {
	return f(ctx, tx, s, reason)
}

func (r *jobRig) run(ctx context.Context, j jobs.Job) (err error) {
	defer func() { r.mu.Lock(); r.results[j.ID] = err; r.mu.Unlock() }()
	var p struct{ Action string }
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	actor, err := j.RebuildActor(r.inv)
	if err != nil {
		return err
	}
	actor, err = r.a.s.RefreshJobActor(ctx, actor, j.EnqueuedAt)
	if err != nil {
		return err
	}
	_, err = r.inv.InvokeAs(ctx, actor, p.Action, json.RawMessage(`{}`), actions.Options{Confirm: j.Confirm, RequestID: jobs.RequestID(j.ID, j.Attempt)})
	return err
}

// queue stores a job as actor (the gate is still closed, so nothing runs yet).
func (r *jobRig) queue(actor actions.Actor, action, confirm string) int64 {
	r.a.t.Helper()
	r.lock++
	id, err := r.js.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindRestart, LockKey: jobs.LockKey(int64(r.lock)), Actor: jobs.ActorOf(actor), Confirm: confirm,
		Payload: json.RawMessage(fmt.Sprintf(`{"Action":%q}`, action)), TimeoutS: 60})
	if err != nil {
		r.a.t.Fatal(err)
	}
	return id
}

// finish opens the gate and waits for the job's recorded result.
func (r *jobRig) finish(id int64) error {
	r.a.t.Helper()
	r.sched.OpenGate()
	r.sched.Notify()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		err, ok := r.results[id]
		r.mu.Unlock()
		if ok {
			return err
		}
		if time.Now().After(deadline) {
			r.a.t.Fatalf("job %d did not run", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *jobRig) state(id int64) string {
	r.a.t.Helper()
	var s string
	if err := r.a.st.R.QueryRow(`SELECT state FROM jobs WHERE id = ?`, id).Scan(&s); err != nil {
		r.a.t.Fatal(err)
	}
	return s
}

func (r *jobRig) waitState(id int64, want string) {
	r.a.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.state(id) != want {
		if time.Now().After(deadline) {
			r.a.t.Fatalf("job %d is %s, want %s", id, r.state(id), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func deniedWith(t *testing.T, err error, sub string) {
	t.Helper()
	if !errors.Is(err, actions.ErrDenied) || !strings.Contains(err.Error(), sub) {
		t.Fatalf("got %v, want a denial containing %q", err, sub)
	}
}

func tokenActor(tok TokenCreateOut, scope actions.Scope) actions.Actor {
	return actions.TokenActor(strconv.FormatInt(tok.ID, 10), scope)
}

// Plan 0002 "must": a job queued by a write token reaches the runner, whose
// InvokeAs of a destructive action is denied for that token, whatever the job
// kind. The denied row names the token with executor = system; nothing ran.
func TestJobQueuedByTokenCannotReachADeniedAction(t *testing.T) {
	r := newJobRig(t)
	tok := r.a.createToken("write", 30)
	actor := tokenActor(tok, actions.ScopeWrite)
	// Enqueue time: the destructive action is refused to the token directly too.
	if err := r.a.reg.Authorize(ctx, actor, "t.job_destroy", json.RawMessage(`{}`), actions.Options{Confirm: "svc"}); !errors.Is(err, actions.ErrDenied) {
		t.Fatalf("authorize: %v", err)
	}
	id := r.queue(actor, "t.job_destroy", "svc")
	deniedWith(t, r.finish(id), "")
	if r.destroy.Load() != 0 {
		t.Fatal("the destructive action ran")
	}
	r.waitState(id, jobs.StateFailed)
	var kind, aid, executor, phase string
	if err := r.a.st.R.QueryRow(`SELECT actor_kind, actor_id, COALESCE(executor, ''), phase FROM audit_log WHERE action = 't.job_destroy' ORDER BY id DESC LIMIT 1`).
		Scan(&kind, &aid, &executor, &phase); err != nil {
		t.Fatal(err)
	}
	if kind != "token" || aid != strconv.FormatInt(tok.ID, 10) || executor != "system" || phase != "denied" {
		t.Errorf("denied row: %s/%s executor=%q phase=%s", kind, aid, executor, phase)
	}
	// The token still runs what its own scope allows, as itself.
	ok := r.queue(actor, "t.job_run", "")
	if err := r.finish(ok); err != nil || r.runs.Load() != 1 {
		t.Fatalf("a write job: %v runs=%d", err, r.runs.Load())
	}
}

func TestJobAuditRowsCarryTheActorWithExecutorSystemForARealQueuedJob(t *testing.T) {
	r := newJobRig(t)
	id := r.queue(r.a.user(), "t.job_run", "")
	if err := r.finish(id); err != nil {
		t.Fatal(err)
	}
	rows, err := r.a.st.R.Query(`SELECT actor_kind, actor_id, COALESCE(executor, ''), request_id FROM audit_log WHERE action = 't.job_run'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var k, aid, ex, req string
		rows.Scan(&k, &aid, &ex, &req)
		n++
		if k != "user" || aid != strconv.FormatInt(r.a.uid, 10) || ex != "system" || req != jobs.RequestID(id, 1) {
			t.Errorf("row %s/%s executor=%q request=%q", k, aid, ex, req)
		}
	}
	if n != 2 {
		t.Errorf("%d rows, want intent and outcome", n)
	}
}

func TestRevokedTokenJobDeniedAtRunTime(t *testing.T) {
	r := newJobRig(t)
	tok := r.a.createToken("write", 30)
	id := r.queue(tokenActor(tok, actions.ScopeWrite), "t.job_run", "")
	r.a.mustInv(r.a.user(), "tokens.revoke", fmt.Sprintf(`{"id":%d}`, tok.ID))
	deniedWith(t, r.finish(id), "revoked or has expired")
	if r.runs.Load() != 0 {
		t.Error("the job ran")
	}
}

func TestExpiredTokenJobDeniedAtRunTime(t *testing.T) {
	r := newJobRig(t)
	tok := r.a.createToken("write", 1)
	id := r.queue(tokenActor(tok, actions.ScopeWrite), "t.job_run", "")
	r.a.clk.Add(25 * time.Hour)
	deniedWith(t, r.finish(id), "revoked or has expired")
	if r.runs.Load() != 0 {
		t.Error("the job ran")
	}
}

// The scope comes from the database at run time and is never wider than the one
// the job was queued with.
func TestTokenJobScopeComesFromTheDatabase(t *testing.T) {
	r := newJobRig(t)
	tok := r.a.createToken("read", 30)
	// A blob claiming "write" for a read token is cut down to read: denied.
	id := r.queue(tokenActor(tok, actions.ScopeWrite), "t.job_run", "")
	deniedWith(t, r.finish(id), "scope")
	if r.runs.Load() != 0 {
		t.Error("the job ran")
	}
}

func TestCredentialChangeDeniesAQueuedUserJob(t *testing.T) {
	change := map[string]func(r *jobRig){
		"password change": func(r *jobRig) {
			r.a.mustInv(r.a.user(), "auth.password_change", fmt.Sprintf(`{"current_password":%q,"new_password":"a brand new passphrase"}`, goodPassword))
		},
		"totp re-enrolment": func(r *jobRig) {
			begin := r.a.mustInv(r.a.user(), "auth.totp_reenroll_begin", "").Out.(ReenrollBeginOut)
			seed, err := b32().DecodeString(begin.Secret)
			if err != nil {
				r.a.t.Fatal(err)
			}
			r.a.clk.Add(31 * time.Second)
			actor := actions.UserSessionActor(strconv.FormatInt(r.a.uid, 10), r.a.public, actions.ScopeAdmin, r.a.clk.Now().Add(time.Minute))
			r.a.mustInv(actor, "auth.totp_reenroll_confirm", fmt.Sprintf(`{"enrollment_id":%q,"code":%q}`, begin.EnrollmentID, TOTPCode(seed, TOTPCounter(r.a.clk.Now()))))
		},
		"admin reset": func(r *jobRig) {
			if _, err := r.a.s.AdminReset(ctx, false); err != nil {
				r.a.t.Fatal(err)
			}
		},
	}
	for name, do := range change {
		t.Run(name, func(t *testing.T) {
			r := newJobRig(t)
			actor := r.a.user()
			id := r.queue(actor, "t.job_run", "")
			r.a.clk.Add(10 * time.Second)
			do(r)
			r.a.clk.Add(10 * time.Second)
			if err := r.finish(id); !errors.Is(err, actions.ErrDenied) {
				t.Fatalf("got %v, want a denial", err)
			}
			if r.runs.Load() != 0 {
				t.Error("the job ran")
			}
		})
	}
	t.Run("nothing changed", func(t *testing.T) {
		r := newJobRig(t)
		id := r.queue(r.a.user(), "t.job_run", "")
		r.a.clk.Add(10 * time.Second)
		if err := r.finish(id); err != nil || r.runs.Load() != 1 {
			t.Fatalf("%v runs=%d", err, r.runs.Load())
		}
	})
}

func TestDeletedUserJobIsDenied(t *testing.T) {
	r := newJobRig(t)
	id := r.queue(r.a.user(), "t.job_run", "")
	r.a.clk.Add(time.Second)
	if _, err := r.a.st.W.Exec(`DELETE FROM sessions`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.st.W.Exec(`DELETE FROM users WHERE id = ?`, r.a.uid); err != nil {
		t.Fatal(err)
	}
	deniedWith(t, r.finish(id), "no longer exists")
}

// A destructive job keeps the fresh-2FA window it was queued with: past it, the
// registry says the user must confirm again, and nothing is deleted. A wrong or
// missing Confirm is refused the same way.
func TestDestructiveJobPastTheFreshWindowIsRefusedAtRunTime(t *testing.T) {
	r := newJobRig(t)
	id := r.queue(r.a.user(), "t.job_destroy", "svc")
	r.a.clk.Add(6 * time.Minute) // the window is 5 minutes
	err := r.finish(id)
	var de *actions.DeniedError
	if !errors.As(err, &de) || de.Code != "mfa_required" {
		t.Fatalf("got %v, want the mfa_required denial", err)
	}
	if r.destroy.Load() != 0 {
		t.Fatal("the destructive action ran")
	}
	// Inside the window, with the exact Confirm, it runs; without it, it does not.
	r2 := newJobRig(t)
	if err := r2.finish(r2.queue(r2.a.user(), "t.job_destroy", "wrong")); !errors.As(err, &de) || de.Code != "confirm" {
		t.Fatalf("wrong confirm: %v", err)
	}
	r3 := newJobRig(t)
	if err := r3.finish(r3.queue(r3.a.user(), "t.job_destroy", "svc")); err != nil || r3.destroy.Load() != 1 {
		t.Fatalf("%v destroy=%d", err, r3.destroy.Load())
	}
}

func TestSignOutDoesNotCancelAQueuedJob(t *testing.T) {
	r := newJobRig(t)
	id := r.queue(r.a.user(), "t.job_run", "")
	if err := r.a.s.RevokeSession(ctx, r.a.secret); err != nil { // sign-out
		t.Fatal(err)
	}
	if _, err := r.a.s.LookupSession(ctx, r.a.secret); err == nil {
		t.Fatal("the session survived sign-out")
	}
	if st := r.state(id); st != jobs.StateQueued {
		t.Fatalf("sign-out changed the job to %s", st)
	}
	if err := r.finish(id); err != nil || r.runs.Load() != 1 {
		t.Fatalf("%v runs=%d", err, r.runs.Load())
	}
	// Session expiry is no different: the session row is gone and the job still runs.
	r2 := newJobRig(t)
	id2 := r2.queue(r2.a.user(), "t.job_run", "")
	r2.a.clk.Add(8 * 24 * time.Hour)
	if err := r2.finish(id2); err != nil || r2.runs.Load() != 1 {
		t.Fatalf("after expiry: %v runs=%d", err, r2.runs.Load())
	}
}

func TestExplicitSessionRevokeCancelsThatSessionsJobs(t *testing.T) {
	t.Run("one session", func(t *testing.T) {
		r := newJobRig(t)
		_, p1 := r.a.newSessionFor(r.a.uid)
		_, p2 := r.a.newSessionFor(r.a.uid)
		mk := func(public string) actions.Actor {
			return actions.UserSessionActor(strconv.FormatInt(r.a.uid, 10), public, actions.ScopeAdmin, r.a.clk.Now().Add(time.Minute))
		}
		revoked, other, mine := r.queue(mk(p1), "t.job_run", ""), r.queue(mk(p2), "t.job_run", ""), r.queue(r.a.user(), "t.job_run", "")
		r.a.mustInv(r.a.user(), "auth.sessions_revoke", fmt.Sprintf(`{"public_id":%q}`, p1))
		if st := r.state(revoked); st != jobs.StateCancelled {
			t.Fatalf("the revoked session's job is %s", st)
		}
		var reason string
		r.a.st.R.QueryRow(`SELECT cancel_reason FROM jobs WHERE id = ?`, revoked).Scan(&reason)
		if reason != SessionRevokedJobReason {
			t.Errorf("reason %q", reason)
		}
		if r.state(other) != jobs.StateQueued || r.state(mine) != jobs.StateQueued {
			t.Error("another session's job was cancelled")
		}
		if err := r.finish(other); err != nil {
			t.Fatal(err)
		}
		if err := r.finish(mine); err != nil {
			t.Fatal(err)
		}
		if r.runs.Load() != 2 {
			t.Errorf("runs=%d, want 2 (the revoked session's job never runs)", r.runs.Load())
		}
	})
	t.Run("all others", func(t *testing.T) {
		r := newJobRig(t)
		_, p1 := r.a.newSessionFor(r.a.uid)
		_, p2 := r.a.newSessionFor(r.a.uid)
		mk := func(public string) actions.Actor {
			return actions.UserSessionActor(strconv.FormatInt(r.a.uid, 10), public, actions.ScopeAdmin, r.a.clk.Now().Add(time.Minute))
		}
		a1, a2, mine := r.queue(mk(p1), "t.job_run", ""), r.queue(mk(p2), "t.job_run", ""), r.queue(r.a.user(), "t.job_run", "")
		res := r.a.mustInv(r.a.user(), "auth.sessions_revoke", `{"all_others":true}`).Out.(RevokedOut)
		if res.Revoked != 2 {
			t.Fatalf("revoked %d", res.Revoked)
		}
		for _, id := range []int64{a1, a2} {
			if st := r.state(id); st != jobs.StateCancelled {
				t.Errorf("job %d is %s", id, st)
			}
		}
		if st := r.state(mine); st != jobs.StateQueued {
			t.Errorf("the caller's own job is %s", st)
		}
	})
	t.Run("a running job is flagged and stops", func(t *testing.T) {
		r := newJobRig(t)
		_, p1 := r.a.newSessionFor(r.a.uid)
		started := make(chan struct{})
		actions.Register(r.a.reg, actions.Def[actions.None, string]{Name: "t.job_block", Summary: "x", Risk: actions.RiskMutate, Scope: actions.ScopeWrite, JobOnly: true,
			Target: func(actions.None) string { return "svc" },
			Execute: func(ctx context.Context, _ actions.Call, _ actions.None) (string, error) {
				close(started)
				<-ctx.Done()
				return "", context.Cause(ctx)
			}})
		id := r.queue(actions.UserSessionActor(strconv.FormatInt(r.a.uid, 10), p1, actions.ScopeAdmin, r.a.clk.Now().Add(time.Minute)), "t.job_block", "")
		r.sched.OpenGate()
		r.sched.Notify()
		<-started
		r.a.mustInv(r.a.user(), "auth.sessions_revoke", fmt.Sprintf(`{"public_id":%q}`, p1))
		r.waitState(id, jobs.StateCancelled)
	})
}

// The session delete and the jobs cancel are one transaction: when the cancel
// fails, the session is still there and the caller gets the typed error with no
// internal detail.
func TestSessionRevokeIsRolledBackWhenTheJobCancelFails(t *testing.T) {
	a := newAEnvJobs(t, func(*env) JobCanceller {
		return jobCancellerFunc(func(context.Context, *sql.Tx, []string, string) (func(), error) {
			return nil, errors.New("db is down")
		})
	})
	_, p := a.newSessionFor(a.uid)
	_, err := a.inv(a.user(), "auth.sessions_revoke", fmt.Sprintf(`{"public_id":%q}`, p))
	if !errors.Is(err, ErrRevokeNotApplied) || strings.Contains(err.Error(), "db is down") {
		t.Fatalf("got %v", err)
	}
	if _, err := a.inv(a.user(), "auth.sessions_revoke", `{"all_others":true}`); !errors.Is(err, ErrRevokeNotApplied) {
		t.Fatalf("all_others: %v", err)
	}
	var n int
	a.st.R.QueryRow(`SELECT count(*) FROM sessions WHERE public_id = ?`, p).Scan(&n)
	if n != 1 {
		t.Error("the session was revoked although its jobs were not cancelled")
	}
}

// Revoke between authorisation and enqueue: the enqueue is refused, so no job can
// be left behind by a session that has just been revoked.
func TestJobQueuedByARevokedSessionIsRefusedAtEnqueue(t *testing.T) {
	r := newJobRig(t)
	_, p := r.a.newSessionFor(r.a.uid)
	actor := actions.UserSessionActor(strconv.FormatInt(r.a.uid, 10), p, actions.ScopeAdmin, r.a.clk.Now().Add(time.Minute))
	r.a.mustInv(r.a.user(), "auth.sessions_revoke", fmt.Sprintf(`{"public_id":%q}`, p)) // revoked after the caller was authorised
	_, err := r.js.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindRestart, LockKey: jobs.LockKey(1), Actor: jobs.ActorOf(actor), Payload: json.RawMessage(`{}`), TimeoutS: 60})
	if !errors.Is(err, jobs.ErrActorSessionGone) {
		t.Fatalf("got %v", err)
	}
}

func TestRefreshJobActorRefusesKindsThatCannotOwnAJob(t *testing.T) {
	a := newAEnv(t)
	for name, actor := range map[string]actions.Actor{
		"mcp": actions.MCPActor("1"), "webhook": actions.WebhookActor("1", "x"), "empty": {},
	} {
		if _, err := a.s.RefreshJobActor(ctx, actor, a.clk.Now()); !errors.Is(err, actions.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A database failure while refreshing returns a fixed message (it lands in the
// job's error column), not the driver's text.
func TestRefreshJobActorDatabaseErrorIsFixedText(t *testing.T) {
	a := newAEnv(t)
	dead, cancel := context.WithCancel(ctx)
	cancel()
	for name, actor := range map[string]actions.Actor{"user": a.user(), "token": actions.TokenActor("1", actions.ScopeWrite)} {
		_, err := a.s.RefreshJobActor(dead, actor, a.clk.Now())
		if err == nil || err.Error() != "could not check the job's actor" {
			t.Errorf("%s: %v", name, err)
		}
	}
}
