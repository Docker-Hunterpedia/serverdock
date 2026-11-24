package deploy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

type fakeRefresh struct {
	mu    sync.Mutex
	calls []time.Time
	err   error
	scope actions.Scope // when set, the refreshed actor's scope (a narrowed one)
}

func (f *fakeRefresh) RefreshJobActor(_ context.Context, a actions.Actor, enq time.Time) (actions.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, enq)
	if f.err != nil {
		return actions.Actor{}, f.err
	}
	if f.scope != 0 {
		return actions.UserSessionActor(a.ID(), a.SessionID(), f.scope, a.MFAFreshUntil()), nil
	}
	return a, nil
}

func (f *fakeRefresh) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

type hookRig struct {
	r     *Runner
	ref   *fakeRefresh
	reg   *actions.Registry
	execs []execCall
	mu    sync.Mutex
}

type execCall struct {
	job   int64
	actor actions.Actor
	req   string
}

func newHookRig(t *testing.T) *hookRig {
	t.Helper()
	st := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	al, err := audit.New(st, log)
	if err != nil {
		t.Fatal(err)
	}
	h := &hookRig{ref: &fakeRefresh{}, reg: actions.New(al, log)}
	h.r = NewRunner(RunnerConfig{DB: st, Jobs: jobs.NewStore(st, nil), Inv: h.reg.SystemInvoker(), Refresh: h.ref, Locks: &ServiceLocks{}, Log: log})
	h.r.exec = func(_ context.Context, c actions.Call, job int64) (RunDeployOut, error) {
		h.mu.Lock()
		h.execs = append(h.execs, execCall{job, c.Actor, c.RequestID})
		h.mu.Unlock()
		return RunDeployOut{Status: "ok"}, nil
	}
	RegisterJobActions(h.reg, h.r)
	return h
}

func (h *hookRig) n() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.execs) }

func queuedBy(a actions.Actor, attempt int) jobs.Job {
	return jobs.Job{ID: 7, Kind: jobs.KindDeploy, Attempt: attempt, EnqueuedAt: time.Unix(1000, 0), Actor: jobs.ActorOf(a), Payload: []byte(`{"deploy":1}`)}
}

// Plan 0004 Revision 7: the actor is rebuilt and refreshed before EVERY run and
// every retry attempt, and the action then runs as the REFRESHED actor, with the
// attempt's own request id.
func TestRunnerRefreshesTheActorBeforeEveryRunAndAttempt(t *testing.T) {
	h := newHookRig(t)
	h.ref.scope = actions.ScopeWrite // the user's scope was narrowed after the job was queued
	admin := actions.UserSessionActor("1", "sess-a", actions.ScopeAdmin, time.Now().Add(time.Hour))
	for attempt := 1; attempt <= 3; attempt++ {
		if err := h.r.Run(context.Background(), queuedBy(admin, attempt)); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if h.ref.n() != attempt || h.n() != attempt {
			t.Fatalf("attempt %d: refreshed %d times, ran %d times", attempt, h.ref.n(), h.n())
		}
	}
	for i, e := range h.execs {
		if e.actor.Scope() != actions.ScopeWrite || e.actor.ID() != "1" || e.actor.SessionID() != "sess-a" {
			t.Errorf("run %d as %v %s %s: not the refreshed actor", i, e.actor.Scope(), e.actor.ID(), e.actor.SessionID())
		}
		if want := jobs.RequestID(7, i+1); e.req != want || e.job != 7 {
			t.Errorf("run %d: request id %q job %d, want %q", i, e.req, e.job, want)
		}
	}
	for _, at := range h.ref.calls {
		if !at.Equal(time.Unix(1000, 0)) {
			t.Errorf("refreshed against %v, not the enqueue time", at)
		}
	}
}

// A job is never run on an error from the rebuild or the refresh.
func TestRunnerNeverRunsAJobOnARebuildOrRefreshError(t *testing.T) {
	admin := actions.UserSessionActor("1", "sess-a", actions.ScopeAdmin, time.Now().Add(time.Hour))

	h := newHookRig(t)
	h.ref.err = &actions.DeniedError{Reason: "the API token that queued this job was revoked or has expired"}
	err := h.r.Run(context.Background(), queuedBy(admin, 1))
	if !errors.Is(err, actions.ErrDenied) || h.n() != 0 {
		t.Fatalf("refresh error: %v, ran %d times", err, h.n())
	}
	// The same job, refreshed fine on its next attempt, runs.
	h.ref.err = nil
	if err := h.r.Run(context.Background(), queuedBy(admin, 2)); err != nil || h.n() != 1 {
		t.Fatalf("%v ran %d", err, h.n())
	}

	for name, mod := range map[string]func(*jobs.Job){
		"garbage blob":  func(j *jobs.Job) { j.Actor.Blob = "{nope" },
		"system kind":   func(j *jobs.Job) { j.Actor.Blob = `{"v":1,"k":"system","id":"x"}` },
		"id mismatch":   func(j *jobs.Job) { j.Actor.ID = "2" },
		"kind mismatch": func(j *jobs.Job) { j.Actor.Kind = "token" },
		"session swap":  func(j *jobs.Job) { j.Actor.Session = "sess-other" },
		"empty blob":    func(j *jobs.Job) { j.Actor.Blob = "" },
	} {
		h := newHookRig(t)
		j := queuedBy(admin, 1)
		mod(&j)
		err := h.r.Run(context.Background(), j)
		if err == nil || h.n() != 0 || h.ref.n() != 0 {
			t.Errorf("%s: err %v, refreshed %d, ran %d (the job must not run, nor be refreshed)", name, err, h.ref.n(), h.n())
		}
		if err != nil && strings.Contains(err.Error(), "{nope") {
			t.Errorf("%s: the blob leaked into the error: %v", name, err)
		}
	}

	// A kind that has no runner yet does not run (and is not refreshed for nothing).
	h = newHookRig(t)
	j := queuedBy(admin, 1)
	j.Kind = jobs.KindRestart
	if err := h.r.Run(context.Background(), j); err == nil || h.n() != 0 {
		t.Errorf("restart: %v ran %d", err, h.n())
	}
}

// jobs.run_deploy is JobOnly: nobody but the runner can invoke it.
func TestRunDeployIsJobOnly(t *testing.T) {
	h := newHookRig(t)
	admin := actions.UserSessionActor("1", "sess-a", actions.ScopeAdmin, time.Now().Add(time.Hour))
	_, err := h.reg.Invoke(context.Background(), admin, "jobs.run_deploy", []byte(`{"job":1}`), actions.Options{})
	if !errors.Is(err, actions.ErrDenied) && !errors.Is(err, actions.ErrUnknownAction) {
		t.Fatalf("a direct call: %v", err)
	}
	if h.n() != 0 {
		t.Error("ran")
	}
}

func TestServiceLocks(t *testing.T) {
	var l ServiceLocks
	un, err := l.Lock(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.TryLock(1); ok {
		t.Fatal("TryLock took a held lock")
	}
	other, ok := l.TryLock(2)
	if !ok {
		t.Fatal("another service's lock is not free")
	}
	// A waiter gives up with its context and leaves no entry behind.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := l.Lock(ctx, 1); err == nil {
		t.Fatal("acquired a held lock")
	}
	got := make(chan func(), 1)
	go func() {
		u, err := l.Lock(context.Background(), 1)
		if err != nil {
			t.Error(err)
		}
		got <- u
	}()
	time.Sleep(20 * time.Millisecond)
	un()
	un() // idempotent
	u2 := <-got
	u2()
	other()
	if l.Len() != 0 {
		t.Errorf("%d entries left", l.Len())
	}
	if u, ok := l.TryLock(1); !ok {
		t.Error("not free after unlock")
	} else {
		u()
	}
}

func TestResolveInterruptedSettlesJobsOfAnEarlierBoot(t *testing.T) {
	st := openStore(t)
	svc := addService(t, st, "web")
	other := addService(t, st, "api")
	mkJob := func(svc int64, state, boot string) int64 {
		var b any
		if boot != "" {
			b = boot
		}
		res, err := st.W.Exec(`INSERT INTO jobs(kind, lock_key, state, actor_kind, actor_id, actor_blob, enqueued_at, payload_json, timeout_s, boot_id, run_after, created_at)
			VALUES ('deploy', ?, ?, 'user', '1', '{}', 'x', '{}', 60, ?, 'x', 'x')`, jobs.LockKey(svc), state, b)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	link := func(dep, job int64) {
		mustExec(t, st, `UPDATE deploys SET job_id = ?, status = 'running' WHERE id = ?`, job, dep)
	}

	uncommitted := addDeploy(t, st, svc, 1, "running", nil)
	jobA := mkJob(svc, "running", "old-boot")
	link(uncommitted, jobA)
	mustExec(t, st, `INSERT INTO deploy_stages(deploy_id, stage, state) VALUES (?, 'pull', 'ok'), (?, 'start', 'running'), (?, 'health', 'pending')`, uncommitted, uncommitted, uncommitted)

	committed := addDeploy(t, st, other, 1, "running", nil)
	jobB := mkJob(other, "running", "") // a NULL boot id is an earlier boot too
	link(committed, jobB)
	mustExec(t, st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, committed, other)

	third := addService(t, st, "third")
	jobNow := mkJob(third, "running", "this-boot") // this boot's own job is left alone
	queued := mkJob(addService(t, st, "fourth"), "queued", "")

	n, err := ResolveInterrupted(context.Background(), st, "this-boot", time.Now())
	if err != nil || n != 2 {
		t.Fatalf("%d settled, %v", n, err)
	}
	get := func(q string, id int64) (a, b string) {
		var x, y *string
		if err := st.R.QueryRow(q, id).Scan(&x, &y); err != nil {
			t.Fatal(err)
		}
		if x != nil {
			a = *x
		}
		if y != nil {
			b = *y
		}
		return
	}
	if s, e := get(`SELECT state, error FROM jobs WHERE id = ?`, jobA); s != "failed" || e != "interrupted by restart" {
		t.Errorf("uncommitted job %s %q", s, e)
	}
	if s, c := get(`SELECT status, reason_code || '/' || app_safe FROM deploys WHERE id = ?`, uncommitted); s != "failed" || c != "interrupted/unknown" {
		t.Errorf("uncommitted deploy %s %s", s, c)
	}
	if s, c := get(`SELECT state, error FROM jobs WHERE id = ?`, jobB); s != "succeeded" || c != "" {
		t.Errorf("committed job %s %q", s, c)
	}
	if s, _ := get(`SELECT status, reason FROM deploys WHERE id = ?`, committed); s != "succeeded" {
		t.Errorf("committed deploy %s", s)
	}
	for stage, want := range map[string]string{"pull": "ok", "start": "failed", "health": "skipped"} {
		var got string
		if err := st.R.QueryRow(`SELECT state FROM deploy_stages WHERE deploy_id = ? AND stage = ?`, uncommitted, stage).Scan(&got); err != nil || got != want {
			t.Errorf("stage %s: %q %v, want %s", stage, got, err, want)
		}
	}
	if s, _ := get(`SELECT state, error FROM jobs WHERE id = ?`, jobNow); s != "running" {
		t.Errorf("this boot's job was touched: %s", s)
	}
	if s, _ := get(`SELECT state, error FROM jobs WHERE id = ?`, queued); s != "queued" {
		t.Errorf("a queued job was touched: %s", s)
	}
	// Nothing left to do: a second pass settles nothing.
	if n, err := ResolveInterrupted(context.Background(), st, "this-boot", time.Now()); err != nil || n != 0 {
		t.Errorf("second pass: %d %v", n, err)
	}
}
