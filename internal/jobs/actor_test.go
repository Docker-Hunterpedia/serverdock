package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
)

var ctx = context.Background()

func TestEnqueueRefusesTheSystemActorForUserTriggeredKinds(t *testing.T) {
	e := newEnv(t)
	svc := e.service(t, "web")
	for _, kind := range []string{"system", "System"} {
		n := job(LockKey(svc))
		n.Actor = Actor{Kind: kind, ID: "system", Blob: `{"v":1,"k":"system","id":"system"}`}
		if _, err := e.js.Enqueue(ctx, n); !errors.Is(err, ErrSystemActor) || !errors.Is(err, ErrInvalid) {
			t.Errorf("restart queued by %q: %v", kind, err)
		}
		d := deploy(svc)
		d.Actor = n.Actor
		if _, err := e.js.EnqueueDeploy(ctx, d); !errors.Is(err, ErrSystemActor) {
			t.Errorf("deploy queued by %q: %v", kind, err)
		}
		del := job(LockKey(svc))
		del.Kind, del.Actor = KindServiceDelete, n.Actor
		if _, err := e.js.Enqueue(ctx, del); !errors.Is(err, ErrSystemActor) {
			t.Errorf("delete queued by %q: %v", kind, err)
		}
	}
	// The system actor's own encoding is empty, so ActorOf can never build a valid one.
	sys := ActorOf(actions.Actor{})
	if err := sys.validate(); err == nil {
		t.Error("an empty actor validates")
	}
	var n int
	e.st.R.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows written", n)
	}
}

func TestActorOfRoundTripsThroughTheRowAndRebuild(t *testing.T) {
	e := newEnv(t)
	svc := e.service(t, "web")
	user := actions.UserSessionActor("7", "pub-7", actions.ScopeAdmin, time.Now().Add(time.Minute)).WithHostKind("name")
	n := job(LockKey(svc))
	n.Actor = ActorOf(user)
	id, err := e.js.Enqueue(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	row, err := e.js.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Actor.Kind != "user" || row.Actor.ID != "7" || row.Actor.Session != "pub-7" {
		t.Fatalf("columns: %+v", row.Actor)
	}
	inv := newInvoker(t)
	got, err := row.RebuildActor(inv)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind() != actions.KindUser || got.ID() != "7" || got.SessionID() != "pub-7" || got.HostKind() != "name" || got.Scope() != actions.ScopeAdmin {
		t.Errorf("rebuilt %+v", got)
	}
	// A blob that disagrees with the columns is refused.
	for name, mod := range map[string]func(*Job){
		"kind":    func(j *Job) { j.Actor.Kind = "token" },
		"id":      func(j *Job) { j.Actor.ID = "8" },
		"session": func(j *Job) { j.Actor.Session = "other" },
	} {
		j := row
		mod(&j)
		if _, err := j.RebuildActor(inv); !errors.Is(err, ErrActorMismatch) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A row whose blob says "system" never rebuilds.
	j := row
	j.Actor.Blob = `{"v":1,"k":"system","id":"7"}`
	if _, err := j.RebuildActor(inv); !errors.Is(err, actions.ErrSystemJobActor) {
		t.Errorf("system blob: %v", err)
	}
}

func TestCancelSessionJobsCancelsQueuedAndRunningJobsOfThoseSessionsOnly(t *testing.T) {
	e := newEnv(t)
	started := make(chan int64, 4)
	var sawCancel atomic.Bool
	s, _ := start(t, e, func(ctx context.Context, j Job) error {
		started <- j.ID
		<-ctx.Done()
		sawCancel.Store(true)
		return context.Cause(ctx)
	}, nil)
	s.OpenGate()
	mk := func(svc int64, session string) int64 {
		n := job(LockKey(svc))
		n.Actor.Session = session
		id, err := e.js.Enqueue(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b, other := e.service(t, "a"), e.service(t, "b"), e.service(t, "d")
	running := mk(a, "s1")
	s.Notify()
	if got := <-started; got != running {
		t.Fatalf("started %d", got)
	}
	queuedSame := mk(a, "s1") // waits behind the running job of service a
	queuedOther := mk(b, "s2")
	kept := mk(other, "s3")
	// The running job of s1, the queued one of s1 and the one of s2 are cancelled; s3's is not.
	n, err := s.CancelSessionJobs(ctx, []string{"s1", "s2"}, "the session that queued this job was revoked")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("cancelled %d jobs, want 3", n)
	}
	waitFor(t, "the running job to end", func() bool { return e.state(t, running) == StateCancelled })
	if !sawCancel.Load() {
		t.Error("the running job's context was not cancelled")
	}
	for _, id := range []int64{queuedSame, queuedOther} {
		if st := e.state(t, id); st != StateCancelled {
			t.Errorf("job %d is %s", id, st)
		}
	}
	var reason string
	e.st.R.QueryRow(`SELECT cancel_reason FROM jobs WHERE id = ?`, running).Scan(&reason)
	if reason != "the session that queued this job was revoked" {
		t.Errorf("reason %q", reason)
	}
	// s3's job was started (its own service is free), not cancelled.
	if st := e.state(t, kept); st == StateCancelled {
		t.Error("another session's job was cancelled")
	}
	if n, err := s.CancelSessionJobs(ctx, nil, "x"); n != 0 || err != nil {
		t.Errorf("no sessions: %d %v", n, err)
	}
}

// A cancel that lands while a job is being recorded must not leave an entry in the
// scheduler's maps, and the worker count returns to zero.
func TestLateCancelLeavesNoPendingEntry(t *testing.T) {
	e := newEnv(t)
	s, _ := start(t, e, func(context.Context, Job) error { return nil }, nil)
	s.OpenGate()
	svc := e.service(t, "web")
	for range 30 {
		id, err := e.js.Enqueue(ctx, job(LockKey(svc)))
		if err != nil {
			t.Fatal(err)
		}
		s.Notify()
		// Cancel races with the job's own start and finish; any terminal state is fine.
		for st := e.state(t, id); st != StateSucceeded && st != StateCancelled; st = e.state(t, id) {
			_ = s.Cancel(ctx, id, "late")
			time.Sleep(time.Millisecond)
		}
	}
	waitFor(t, "the workers to finish", func() bool { return s.Running() == 0 })
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) != 0 || len(s.running) != 0 {
		t.Errorf("pending=%v running=%d", s.pending, len(s.running))
	}
}

func newInvoker(t *testing.T) *actions.SystemInvoker {
	t.Helper()
	e := newEnv(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	al, err := audit.New(e.st, log)
	if err != nil {
		t.Fatal(err)
	}
	return actions.New(al, log).SystemInvoker()
}
