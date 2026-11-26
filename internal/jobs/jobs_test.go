package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

var testActor = Actor{Kind: "user", ID: "1", Session: "sess1", Blob: `{"k":"user"}`}

type env struct {
	st *store.Store
	js *Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, js: NewStore(st, nil)}
	e.exec(t, `INSERT INTO users(id, username, created_at) VALUES (1, 'admin', 1)`)
	for _, pub := range []string{"sess1", "s1", "s2", "s3", "pub-7"} {
		e.session(t, pub)
	}
	return e
}

// session inserts a live sessions row with that public id (enqueue refuses an
// actor whose session does not exist).
func (e *env) session(t *testing.T, public string) {
	t.Helper()
	e.exec(t, `INSERT INTO sessions(id_hash, public_id, user_id, created_at, last_seen_at, idle_expires_at, abs_expires_at, mfa_fresh_until, ip_key, user_agent)
		VALUES (?, ?, 1, 1, 1, 9999999999, 9999999999, 0, 'ip', 'ua')`, []byte(public), public)
}

func (e *env) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.st.W.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (e *env) service(t *testing.T, name string) int64 {
	t.Helper()
	e.exec(t, `INSERT OR IGNORE INTO projects(id, name, created_at) VALUES (1, 'p', ?)`, FormatTime(time.Now()))
	res, err := e.st.W.Exec(`INSERT INTO services(project_id, name, image_ref, health_kind, health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, created_at, updated_at)
		VALUES (1, ?, 'nginx:1', 'tcp', 60, 3, 'blue_green', 10, 10, 'x', 'x')`, name)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *env) state(t *testing.T, id int64) string {
	t.Helper()
	var s string
	if err := e.st.R.QueryRow(`SELECT state FROM jobs WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func job(lock string) NewJob {
	return NewJob{Kind: KindRestart, LockKey: lock, Actor: testActor, Payload: json.RawMessage(`{}`), TimeoutS: 60}
}

func settings() Settings {
	return Settings{Strategy: StrategyBlueGreen, HealthTimeoutS: 60, DrainS: 10, StopGraceS: 10}
}

func deploy(svc int64) NewDeploy {
	return NewDeploy{ServiceID: svc, Kind: "deploy", Snapshot: json.RawMessage(`{"v":1}`), Settings: settings(), Actor: testActor}
}

// start runs a scheduler and returns a stop function that waits for Run to return.
func start(t *testing.T, e *env, run RunFunc, mod func(*Config)) (*Scheduler, func()) {
	t.Helper()
	cfg := Config{Store: e.js, Run: run, RetryDelay: func(int) time.Duration { return 30 * time.Millisecond }}
	if mod != nil {
		mod(&cfg)
	}
	s, err := NewScheduler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return s, stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A restart's budget: the stop of the container and its health gate.
func TestRestartTimeoutIsTheStopPlusTheHealthGate(t *testing.T) {
	if got := RestartTimeoutSeconds(300, 600); got != (300+15)+(600+30) {
		t.Errorf("%d", got)
	}
	if got := RestartTimeoutSeconds(0, 5); got != 15+35 {
		t.Errorf("%d", got)
	}
}

func TestTimeoutDerivedFromSettings(t *testing.T) {
	max := Settings{Strategy: StrategyBlueGreen, HealthTimeoutS: 600, DrainS: 300, StopGraceS: 300}
	if got, err := TimeoutSeconds(max); err != nil || got != 1945 {
		t.Errorf("blue-green at the maximum settings: %d %v, want 1945", got, err)
	}
	max.Strategy = StrategyStopFirst
	if got, err := TimeoutSeconds(max); err != nil || got != 1945+945 {
		t.Errorf("stop-first at the maximum settings: %d %v, want %d", got, err, 1945+945)
	}
	if got, _ := TimeoutSeconds(settings()); got != 600+60+30+10+10+(10+15+30)+60 {
		t.Errorf("default settings: %d", got)
	}
	for _, bad := range []Settings{
		{Strategy: "x", HealthTimeoutS: 60, DrainS: 10, StopGraceS: 10},
		{Strategy: StrategyBlueGreen, HealthTimeoutS: 4, DrainS: 10, StopGraceS: 10},
		{Strategy: StrategyBlueGreen, HealthTimeoutS: 601, DrainS: 10, StopGraceS: 10},
		{Strategy: StrategyBlueGreen, HealthTimeoutS: 60, DrainS: 9, StopGraceS: 10},
		{Strategy: StrategyBlueGreen, HealthTimeoutS: 60, DrainS: 10, StopGraceS: 301},
		{Strategy: StrategyBlueGreen, HealthTimeoutS: 60, DrainS: 10, StopGraceS: -1},
	} {
		if _, err := TimeoutSeconds(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	// No allowed setting exceeds the derived value for either strategy: the sum of
	// the phase bounds is what TimeoutSeconds returns, and it is stored on the job.
	e := newEnv(t)
	svc := e.service(t, "web")
	d := deploy(svc)
	d.Settings = Settings{Strategy: StrategyStopFirst, HealthTimeoutS: 120, DrainS: 20, StopGraceS: 30}
	out, err := e.js.EnqueueDeploy(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := TimeoutSeconds(d.Settings)
	var got, attempts int
	e.st.R.QueryRow(`SELECT timeout_s, max_attempts FROM jobs WHERE id = ?`, out.JobID).Scan(&got, &attempts)
	if got != want || attempts != DeployAttempts {
		t.Errorf("stored timeout_s %d (want %d), max_attempts %d", got, want, attempts)
	}
}

func TestEnqueueValidatesAtTheBoundary(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	bad := map[string]func(*NewJob){
		"kind":        func(n *NewJob) { n.Kind = "cron" },
		"lock key":    func(n *NewJob) { n.LockKey = "service:1; DROP" },
		"actor":       func(n *NewJob) { n.Actor.Kind = "" },
		"blob":        func(n *NewJob) { n.Actor.Blob = "" },
		"payload":     func(n *NewJob) { n.Payload = json.RawMessage(`{`) },
		"big payload": func(n *NewJob) { n.Payload = json.RawMessage(`"` + strings.Repeat("a", MaxPayload) + `"`) },
		"timeout":     func(n *NewJob) { n.TimeoutS = 0 },
		"attempts":    func(n *NewJob) { n.MaxAttempts = 11 },
	}
	for name, mod := range bad {
		n := job("service:1")
		mod(&n)
		if _, err := e.js.Enqueue(ctx, n); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	d := deploy(1)
	d.Kind = "rollback" // needs a source deploy
	if _, err := e.js.EnqueueDeploy(ctx, d); !errors.Is(err, ErrInvalid) {
		t.Errorf("rollback without a source: %v", err)
	}
	if _, err := e.js.Enqueue(ctx, job("service:1")); err != nil {
		t.Fatal(err)
	}
}

func TestNoTimerWhenNothingIsDue(t *testing.T) {
	e := newEnv(t)
	var ran atomic.Int32
	s, _ := start(t, e, func(context.Context, Job) error { ran.Add(1); return nil }, nil)
	s.OpenGate()
	time.Sleep(50 * time.Millisecond)
	if at, ok := s.NextWake(); ok {
		t.Fatalf("an idle scheduler has a timer set for %v", at)
	}
	// A job that is due now runs and leaves no timer behind.
	id, err := s.Enqueue(context.Background(), job("service:1"))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to succeed", func() bool { return e.state(t, id) == StateSucceeded })
	time.Sleep(20 * time.Millisecond)
	if at, ok := s.NextWake(); ok {
		t.Fatalf("a timer is set after the queue drained: %v", at)
	}
}

// A due job blocked behind a running one of the same key needs no timer: the
// running job's end wakes the scheduler.
func TestBlockedDueJobNeedsNoTimer(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	s, _ := start(t, e, func(ctx context.Context, j Job) error {
		if j.Attempt == 1 && j.ID == 1 {
			<-release
		}
		return nil
	}, nil)
	s.OpenGate()
	first, _ := s.Enqueue(context.Background(), job("service:1"))
	waitFor(t, "the first job to run", func() bool { return e.state(t, first) == StateRunning })
	second, _ := s.Enqueue(context.Background(), job("service:1"))
	time.Sleep(80 * time.Millisecond)
	if e.state(t, second) != StateQueued {
		t.Fatal("the second job of the key ran beside the first")
	}
	if at, ok := s.NextWake(); ok {
		t.Fatalf("a blocked job armed a timer for %v", at)
	}
	close(release)
	waitFor(t, "the second job after the first ended", func() bool { return e.state(t, second) == StateSucceeded })
}

func TestTimerSetToEarliestDueJob(t *testing.T) {
	e := newEnv(t)
	s, _ := start(t, e, func(context.Context, Job) error { return nil }, nil)
	s.OpenGate()
	now := time.Now()
	late, early := now.Add(time.Hour), now.Add(30*time.Minute)
	for i, at := range []time.Time{late, early} {
		n := job(fmt.Sprintf("service:%d", i+1))
		n.RunAfter = at
		if _, err := s.Enqueue(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "the timer to be set to the earlier job", func() bool {
		at, ok := s.NextWake()
		return ok && at.Sub(early.Truncate(time.Millisecond)).Abs() <= time.Millisecond
	})
	// A new, earlier job moves the timer.
	earlier := now.Add(10 * time.Minute)
	n := job("service:3")
	n.RunAfter = earlier
	if _, err := s.Enqueue(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the timer to move", func() bool {
		at, _ := s.NextWake()
		return at.Sub(earlier.Truncate(time.Millisecond)).Abs() <= time.Millisecond
	})
}

func TestTimerFiresForALaterJob(t *testing.T) {
	e := newEnv(t)
	var ran atomic.Int32
	s, _ := start(t, e, func(context.Context, Job) error { ran.Add(1); return nil }, nil)
	s.OpenGate()
	n := job("service:1")
	n.RunAfter = time.Now().Add(150 * time.Millisecond)
	id, _ := e.js.Enqueue(context.Background(), n) // no Notify: only the timer (armed by the next pass) can run it
	s.Notify()
	if e.state(t, id) != StateQueued {
		t.Fatal("not queued")
	}
	waitFor(t, "the timer to run the job", func() bool { return e.state(t, id) == StateSucceeded })
}

func TestSchedulerWakesOnNotify(t *testing.T) {
	e := newEnv(t)
	ran := make(chan int64, 4)
	s, _ := start(t, e, func(_ context.Context, j Job) error { ran <- j.ID; return nil }, nil)
	s.OpenGate()
	time.Sleep(30 * time.Millisecond) // the loop is asleep with no timer
	id, err := s.Enqueue(context.Background(), job("service:1"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ran:
		if got != id {
			t.Fatalf("ran %d, want %d", got, id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the scheduler did not wake on Notify")
	}
}

func TestSchedulerWaitsForTheStartGate(t *testing.T) {
	e := newEnv(t)
	ran := make(chan int64, 4)
	s, _ := start(t, e, func(_ context.Context, j Job) error { ran <- j.ID; return nil }, nil)
	id, err := s.Enqueue(context.Background(), job("service:1"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran:
		t.Fatal("a job ran before the start gate opened")
	case <-time.After(150 * time.Millisecond):
	}
	if e.state(t, id) != StateQueued {
		t.Fatalf("state %s before the gate", e.state(t, id))
	}
	if _, ok := s.NextWake(); ok {
		t.Error("a timer is armed while the gate is closed")
	}
	s.OpenGate()
	s.OpenGate() // idempotent
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("the job did not run after the gate opened")
	}
}

func TestOneRunningJobPerLockKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := e.js.Enqueue(ctx, job("service:1"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	other, _ := e.js.Enqueue(ctx, job("service:2"))
	// Two pickers racing: exactly one job of service:1 and the one of service:2.
	var wg sync.WaitGroup
	var picked sync.Map
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if j, ok, err := e.js.Pick(ctx, "boot"); err != nil {
				t.Error(err)
			} else if ok {
				picked.Store(j.ID, j.LockKey)
			}
		}()
	}
	wg.Wait()
	counts := map[string]int{}
	picked.Range(func(_, v any) bool { counts[v.(string)]++; return true })
	if counts["service:1"] != 1 || counts["service:2"] != 1 {
		t.Fatalf("picked %v, want one job per lock key", counts)
	}
	_ = other
	// The same through the scheduler: never two jobs of one key at once.
	e2 := newEnv(t)
	var cur, peak atomic.Int32
	s, _ := start(t, e2, func(context.Context, Job) error {
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(30 * time.Millisecond)
		cur.Add(-1)
		return nil
	}, nil)
	var last int64
	for i := 0; i < 4; i++ {
		last, _ = s.Enqueue(ctx, job("service:1"))
	}
	s.OpenGate()
	waitFor(t, "all four to finish", func() bool { return e2.state(t, last) == StateSucceeded })
	if p := peak.Load(); p != 1 {
		t.Errorf("%d jobs of one lock key ran at once", p)
	}
}

func TestWorkerLimitTwo(t *testing.T) {
	e := newEnv(t)
	var cur, peak atomic.Int32
	release := make(chan struct{})
	s, _ := start(t, e, func(ctx context.Context, _ Job) error {
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		cur.Add(-1)
		return nil
	}, nil)
	s.OpenGate()
	var ids []int64
	for i := 1; i <= 5; i++ {
		id, err := s.Enqueue(context.Background(), job(fmt.Sprintf("service:%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	waitFor(t, "two workers", func() bool { return s.Running() == 2 })
	time.Sleep(80 * time.Millisecond)
	if s.Running() != 2 || peak.Load() != 2 {
		t.Fatalf("running %d, peak %d, want 2", s.Running(), peak.Load())
	}
	close(release)
	waitFor(t, "all five to finish", func() bool { return e.state(t, ids[4]) == StateSucceeded })
	if peak.Load() > 2 {
		t.Errorf("peak %d workers", peak.Load())
	}
	if _, err := NewScheduler(Config{Store: e.js, Run: func(context.Context, Job) error { return nil }, Workers: 3}); err == nil {
		t.Error("3 workers accepted")
	}
}

func TestSupersedeCancelsQueuedInTheSameTransaction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	svc := e.service(t, "web")
	first, err := e.js.EnqueueDeploy(ctx, deploy(svc))
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.js.EnqueueDeploy(ctx, deploy(svc))
	if err != nil {
		t.Fatal(err)
	}
	if first.Number != 1 || second.Number != 2 {
		t.Errorf("numbers %d %d", first.Number, second.Number)
	}
	if len(second.Superseded) != 1 || second.Superseded[0] != first.JobID {
		t.Fatalf("superseded %v", second.Superseded)
	}
	var status string
	var by int64
	e.st.R.QueryRow(`SELECT status, superseded_by FROM deploys WHERE id = ?`, first.DeployID).Scan(&status, &by)
	if status != DeploySuperseded || by != second.DeployID {
		t.Errorf("first deploy: %s superseded_by %d", status, by)
	}
	if e.state(t, first.JobID) != StateCancelled || e.state(t, second.JobID) != StateQueued {
		t.Errorf("job states %s %s", e.state(t, first.JobID), e.state(t, second.JobID))
	}
	// The new deploy points at its job and the job at the deploy.
	var jobID int64
	e.st.R.QueryRow(`SELECT job_id FROM deploys WHERE id = ?`, second.DeployID).Scan(&jobID)
	if jobID != second.JobID {
		t.Errorf("deploys.job_id %d", jobID)
	}
	// Another service's queued deploy is untouched.
	other := e.service(t, "db")
	o, _ := e.js.EnqueueDeploy(ctx, deploy(other))
	if _, err := e.js.EnqueueDeploy(ctx, deploy(svc)); err != nil {
		t.Fatal(err)
	}
	if e.state(t, o.JobID) != StateQueued {
		t.Error("a deploy of another service was superseded")
	}

	// All or nothing: when the supersede step fails, the new deploy and job are
	// not left behind and the older queued job stays queued.
	e.exec(t, `CREATE TRIGGER fail_supersede BEFORE UPDATE OF status ON deploys WHEN NEW.status = 'superseded' BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	var jobsBefore, deploysBefore int
	e.st.R.QueryRow(`SELECT count(*) FROM jobs`).Scan(&jobsBefore)
	e.st.R.QueryRow(`SELECT count(*) FROM deploys`).Scan(&deploysBefore)
	if _, err := e.js.EnqueueDeploy(ctx, deploy(svc)); err == nil {
		t.Fatal("the failing supersede was not reported")
	}
	var jobsAfter, deploysAfter int
	e.st.R.QueryRow(`SELECT count(*) FROM jobs`).Scan(&jobsAfter)
	e.st.R.QueryRow(`SELECT count(*) FROM deploys`).Scan(&deploysAfter)
	if jobsAfter != jobsBefore || deploysAfter != deploysBefore {
		t.Errorf("a failed enqueue left rows behind: jobs %d->%d, deploys %d->%d", jobsBefore, jobsAfter, deploysBefore, deploysAfter)
	}
}

func TestSupersedeFlagsARunningJob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	svc := e.service(t, "web")
	started := make(chan struct{})
	var cause atomic.Value
	s, _ := start(t, e, func(ctx context.Context, j Job) error {
		close(started)
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return ctx.Err()
	}, nil)
	s.OpenGate()
	first, err := s.EnqueueDeploy(ctx, deploy(svc))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := s.EnqueueDeploy(ctx, deploy(svc))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.CancelRunning) != 1 || second.CancelRunning[0] != first.JobID {
		t.Fatalf("CancelRunning %v", second.CancelRunning)
	}
	waitFor(t, "the superseded job to end", func() bool { return e.state(t, first.JobID) == StateCancelled })
	var cc CancelCause
	if c, _ := cause.Load().(error); !errors.As(c, &cc) || cc.Reason != "superseded" {
		t.Errorf("cancel cause %v", c)
	}
	var flag int
	var reason string
	e.st.R.QueryRow(`SELECT cancel_requested, cancel_reason FROM jobs WHERE id = ?`, first.JobID).Scan(&flag, &reason)
	if flag != 1 || reason != "superseded" {
		t.Errorf("cancel flag %d reason %q", flag, reason)
	}
	// While it runs the deploy is not marked superseded (it may still finish); when
	// the job ends cancelled and the runner (here: a stub that ends no row) left it
	// live, the queue's backstop ends it, naming the deploy that replaced it.
	var by sql.NullInt64
	var status, code, safe string
	e.st.R.QueryRow(`SELECT superseded_by, status, reason_code, COALESCE(app_safe, '') FROM deploys WHERE id = ?`, first.DeployID).Scan(&by, &status, &code, &safe)
	if status != "superseded" || code != "superseded" || !by.Valid || by.Int64 != second.DeployID || safe != "unknown" {
		t.Errorf("deploy after its job ended: %s %s by %v app_safe %q", status, code, by, safe)
	}
}

func TestCancelReachesARunningJob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	started := make(chan struct{})
	var cause atomic.Value
	s, _ := start(t, e, func(ctx context.Context, j Job) error {
		close(started)
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return ctx.Err()
	}, nil)
	s.OpenGate()
	id, _ := s.Enqueue(ctx, job("service:1"))
	<-started
	if err := s.Cancel(ctx, id, "cancelled by the user"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to end", func() bool { return e.state(t, id) == StateCancelled })
	j, _ := e.js.Get(ctx, id)
	if !j.CancelRequested || j.CancelReason != "cancelled by the user" {
		t.Errorf("job %+v", j)
	}
	var cc CancelCause
	if c, _ := cause.Load().(error); !errors.As(c, &cc) {
		t.Errorf("cause %v", c)
	}
	// A queued job is cancelled at once; a finished one cannot be cancelled.
	// Not due for an hour, so the scheduler (which runs another pass once the
	// first job ends) cannot pick it before the cancel: it is still queued.
	later := job("service:2")
	later.RunAfter = time.Now().Add(time.Hour)
	q, _ := e.js.Enqueue(ctx, later)
	if err := e.js.cancelForTest(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, id, "again"); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("cancelling a finished job: %v", err)
	}
	if err := s.Cancel(ctx, 9999, "x"); err == nil {
		t.Error("cancelling a missing job succeeded")
	}
}

func (s *Store) cancelForTest(ctx context.Context, id int64) error {
	st, err := s.cancel(ctx, id, "queued cancel")
	if err == nil && st != StateCancelled {
		return fmt.Errorf("state %s", st)
	}
	return err
}

func TestPullRetriesThenSucceeds(t *testing.T) {
	e := newEnv(t)
	var attempts []int
	var mu sync.Mutex
	s, _ := start(t, e, func(_ context.Context, j Job) error {
		mu.Lock()
		attempts = append(attempts, j.Attempt)
		mu.Unlock()
		if j.RequestID != RequestID(j.ID, j.Attempt) {
			t.Errorf("request id %q", j.RequestID)
		}
		if j.Attempt < 3 {
			return Retryable(errors.New("registry 503"))
		}
		return nil
	}, nil)
	s.OpenGate()
	n := job("service:1")
	n.MaxAttempts = DeployAttempts
	id, _ := s.Enqueue(context.Background(), n)
	waitFor(t, "success on the third attempt", func() bool { return e.state(t, id) == StateSucceeded })
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(attempts) != "[1 2 3]" {
		t.Errorf("attempts %v", attempts)
	}
}

func TestDiskFullAndOtherPermanentErrorsAreNotRetried(t *testing.T) {
	e := newEnv(t)
	var runs atomic.Int32
	s, _ := start(t, e, func(context.Context, Job) error {
		runs.Add(1)
		return errors.New("disk_full: no space left on device") // not wrapped in Retryable
	}, nil)
	s.OpenGate()
	n := job("service:1")
	n.MaxAttempts = DeployAttempts
	id, _ := s.Enqueue(context.Background(), n)
	waitFor(t, "the job to fail", func() bool { return e.state(t, id) == StateFailed })
	time.Sleep(100 * time.Millisecond)
	if runs.Load() != 1 {
		t.Errorf("ran %d times", runs.Load())
	}
	j, _ := e.js.Get(context.Background(), id)
	var errText string
	e.st.R.QueryRow(`SELECT error FROM jobs WHERE id = ?`, j.ID).Scan(&errText)
	if !strings.Contains(errText, "disk_full") {
		t.Errorf("error %q", errText)
	}
}

func TestRetriesStopAtMaxAttempts(t *testing.T) {
	e := newEnv(t)
	var runs atomic.Int32
	s, _ := start(t, e, func(context.Context, Job) error { runs.Add(1); return Retryable(errors.New("timeout")) }, nil)
	s.OpenGate()
	n := job("service:1")
	n.MaxAttempts = 2
	id, _ := s.Enqueue(context.Background(), n)
	waitFor(t, "the job to fail", func() bool { return e.state(t, id) == StateFailed })
	if runs.Load() != 2 {
		t.Errorf("ran %d times, want 2", runs.Load())
	}
	if d := RetryDelay(1); d != 15*time.Second {
		t.Errorf("first delay %v", d)
	}
	if d := RetryDelay(2); d != time.Minute {
		t.Errorf("second delay %v", d)
	}
}

func TestRetryArmsTheTimerAndNothingElse(t *testing.T) {
	e := newEnv(t)
	var runs atomic.Int32
	s, _ := start(t, e, func(_ context.Context, j Job) error {
		if runs.Add(1) == 1 {
			return Retryable(errors.New("503"))
		}
		return nil
	}, func(c *Config) { c.RetryDelay = func(int) time.Duration { return 300 * time.Millisecond } })
	s.OpenGate()
	n := job("service:1")
	n.MaxAttempts = 2
	id, _ := s.Enqueue(context.Background(), n)
	// The timer is also armed, for "now", when the job is first enqueued, and it keeps
	// that wake until the job is picked: wait for the RETRY's wake (about 300 ms
	// ahead), which is set only after the requeue was recorded.
	waitFor(t, "the retry timer", func() bool {
		at, ok := s.NextWake()
		return ok && time.Until(at) > 100*time.Millisecond
	})
	if e.state(t, id) != StateQueued {
		t.Errorf("state %s while waiting to retry", e.state(t, id))
	}
	waitFor(t, "the retry to succeed", func() bool { return e.state(t, id) == StateSucceeded })
	waitFor(t, "the timer to stop", func() bool { _, ok := s.NextWake(); return !ok })
}

func TestPauseAndResume(t *testing.T) {
	e := newEnv(t)
	var ran atomic.Int32
	s, _ := start(t, e, func(context.Context, Job) error { ran.Add(1); return nil }, nil)
	s.OpenGate()
	s.Pause()
	id, _ := s.Enqueue(context.Background(), job("service:1"))
	time.Sleep(100 * time.Millisecond)
	if ran.Load() != 0 {
		t.Fatal("a paused scheduler ran a job")
	}
	s.Resume()
	waitFor(t, "the job after Resume", func() bool { return e.state(t, id) == StateSucceeded })
}

func TestShutdownCancelsRunningJobsAndLeavesThemRunningForTheBootPass(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{})
	var cause atomic.Value
	s, stop := start(t, e, func(ctx context.Context, j Job) error {
		close(started)
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return ctx.Err()
	}, nil)
	s.OpenGate()
	id, _ := s.Enqueue(context.Background(), job("service:1"))
	<-started
	stop() // cancels the Run context and waits for the worker
	if c, _ := cause.Load().(error); !errors.Is(c, ErrShutdown) {
		t.Errorf("cause %v", c)
	}
	if got := e.state(t, id); got != StateRunning {
		t.Errorf("state %s after shutdown: the boot pass resolves it, the scheduler must not guess", got)
	}
	if err := s.Shutdown(context.Background()); err != nil { // idempotent, and bounded
		t.Fatal(err)
	}
	e.st.R.QueryRow(`SELECT 1`).Scan(new(int))
}

func TestAPanickingJobFailsAndTheSchedulerSurvives(t *testing.T) {
	e := newEnv(t)
	s, _ := start(t, e, func(_ context.Context, j Job) error {
		if j.LockKey == "service:1" {
			panic("boom")
		}
		return nil
	}, nil)
	s.OpenGate()
	a, _ := s.Enqueue(context.Background(), job("service:1"))
	waitFor(t, "the panicking job to fail", func() bool { return e.state(t, a) == StateFailed })
	b, _ := s.Enqueue(context.Background(), job("service:2"))
	waitFor(t, "the next job to run", func() bool { return e.state(t, b) == StateSucceeded })
}

// OnDone runs after the result is recorded (the deploy package asks for the
// per-service resync there), for a success and a failure alike, and a panic in it
// does not stop the scheduler.
func TestOnDoneRunsAfterTheResultIsRecorded(t *testing.T) {
	e := newEnv(t)
	type seen struct {
		lock, state string
	}
	var mu sync.Mutex
	var got []seen
	s, _ := start(t, e, func(_ context.Context, j Job) error {
		if j.LockKey == "service:2" {
			return errors.New("fails")
		}
		return nil
	}, func(c *Config) {
		c.OnDone = func(j Job) {
			var state string
			_ = e.st.R.QueryRow(`SELECT state FROM jobs WHERE id = ?`, j.ID).Scan(&state)
			mu.Lock()
			got = append(got, seen{j.LockKey, state})
			mu.Unlock()
			if j.LockKey == "service:3" {
				panic("a bad hook")
			}
		}
	})
	s.OpenGate()
	for _, k := range []string{"service:1", "service:2", "service:3"} {
		if _, err := s.Enqueue(context.Background(), job(k)); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "three results", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 3 })
	mu.Lock()
	defer mu.Unlock()
	for _, g := range got {
		want := StateSucceeded
		if g.lock == "service:2" {
			want = StateFailed
		}
		if g.state != want {
			t.Errorf("%s: the hook saw state %s, want %s (the result must be recorded first)", g.lock, g.state, want)
		}
	}
	d, _ := s.Enqueue(context.Background(), job("service:4"))
	waitFor(t, "the scheduler to survive the hook's panic", func() bool { return e.state(t, d) == StateSucceeded })
}

func TestFinishedJobsPrunedOnInsert(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	svc := e.service(t, "web")
	old := FormatTime(time.Now().Add(-31 * 24 * time.Hour))
	recent := FormatTime(time.Now().Add(-29 * 24 * time.Hour))
	insert := func(state, finished string) int64 {
		res, err := e.st.W.Exec(`INSERT INTO jobs(kind, lock_key, state, actor_kind, actor_id, actor_blob, enqueued_at, payload_json, run_after, timeout_s, created_at, finished_at)
			VALUES ('deploy', 'service:1', ?, 'user', '1', '{}', ?, '{}', ?, 1, ?, ?)`, state, old, old, old, nullable(finished))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	var oldIDs []int64
	for i := 0; i < 130; i++ {
		oldIDs = append(oldIDs, insert([]string{StateSucceeded, StateFailed, StateCancelled}[i%3], old))
	}
	keepRecent := insert(StateSucceeded, recent)
	keepQueued := insert(StateQueued, "")
	// A deploy references the oldest job: pruning must not hit a foreign key error.
	e.exec(t, `INSERT INTO deploys(service_id, number, job_id, kind, snapshot_json, status, actor_kind, actor_id, created_at)
		VALUES (?, 1, ?, 'deploy', '{}', 'succeeded', 'user', '1', ?)`, svc, oldIDs[0], old)

	count := func() int {
		var n int
		e.st.R.QueryRow(`SELECT count(*) FROM jobs WHERE id IN (` + idList(oldIDs) + `)`).Scan(&n)
		return n
	}
	if _, err := e.js.Enqueue(ctx, job("service:9")); err != nil {
		t.Fatalf("enqueue failed (a prune hit a foreign key?): %v", err)
	}
	if got := count(); got != 130-PruneBatch {
		t.Fatalf("%d old jobs left after one insert, want %d (a bounded batch)", got, 130-PruneBatch)
	}
	var jobID sql.NullInt64
	if err := e.st.R.QueryRow(`SELECT job_id FROM deploys WHERE service_id = ? AND number = 1`, svc).Scan(&jobID); err != nil || jobID.Valid {
		t.Errorf("the deploy of a pruned job: job_id %v err %v, want NULL", jobID, err)
	}
	if _, err := e.js.Enqueue(ctx, job("service:9")); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 0 {
		t.Errorf("%d old jobs left after the second insert", got)
	}
	for _, id := range []int64{keepRecent, keepQueued} {
		var n int
		e.st.R.QueryRow(`SELECT count(*) FROM jobs WHERE id = ?`, id).Scan(&n)
		if n != 1 {
			t.Errorf("job %d (recent or unfinished) was pruned", id)
		}
	}
	// A deploy enqueue prunes too.
	e2 := newEnv(t)
	s2 := e2.service(t, "web")
	e2.exec(t, `INSERT INTO jobs(kind, lock_key, state, actor_kind, actor_id, actor_blob, enqueued_at, payload_json, run_after, timeout_s, created_at, finished_at)
		VALUES ('deploy', 'service:1', 'failed', 'user', '1', '{}', ?, '{}', ?, 1, ?, ?)`, old, old, old, old)
	if _, err := e2.js.EnqueueDeploy(ctx, deploy(s2)); err != nil {
		t.Fatal(err)
	}
	var n int
	e2.st.R.QueryRow(`SELECT count(*) FROM jobs WHERE state = 'failed'`).Scan(&n)
	if n != 0 {
		t.Error("EnqueueDeploy did not prune")
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func idList(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return strings.Join(parts, ",")
}

func TestFinishClipsTheErrorAtARuneBoundary(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id, _ := e.js.Enqueue(ctx, job("service:1"))
	if _, ok, err := e.js.Pick(ctx, "b"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	long := strings.Repeat("é", 400) // 800 bytes
	if err := e.js.Finish(ctx, id, StateFailed, long); err != nil {
		t.Fatal(err)
	}
	var got string
	e.st.R.QueryRow(`SELECT error FROM jobs WHERE id = ?`, id).Scan(&got)
	if len(got) > MaxErrorText || strings.ToValidUTF8(got, "") != got {
		t.Errorf("error text: %d bytes, valid=%v", len(got), strings.ToValidUTF8(got, "") == got)
	}
	if err := e.js.Finish(ctx, id, "unknown", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("finish with the state unknown: %v", err)
	}
}

func startHooked(t *testing.T, e *env, run RunFunc, hook func(Job)) (*Scheduler, func()) {
	return start(t, e, run, func(c *Config) { c.afterPick = hook })
}

// A cancel or supersede that arrives after Pick but before the worker is
// registered is not lost: the worker starts already cancelled, runs once, and the
// job ends cancelled.
func TestCancelBetweenPickAndRegistrationIsNotLost(t *testing.T) {
	for _, how := range []string{"cancel", "supersede"} {
		t.Run(how, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			svc := e.service(t, "web")
			picked, release := make(chan struct{}), make(chan struct{})
			var runs atomic.Int32
			var sawCancelled atomic.Bool
			var once sync.Once
			s, _ := startHooked(t, e, func(ctx context.Context, j Job) error {
				if j.ID != 1 { // the superseding deploy's own job is not the one under test
					<-ctx.Done()
					return ctx.Err()
				}
				runs.Add(1)
				sawCancelled.Store(ctx.Err() != nil)
				<-ctx.Done()
				return ctx.Err()
			}, func(Job) { once.Do(func() { close(picked); <-release }) })
			s.OpenGate()
			first, err := s.EnqueueDeploy(ctx, deploy(svc))
			if err != nil {
				t.Fatal(err)
			}
			<-picked
			if how == "cancel" {
				if err := s.Cancel(ctx, first.JobID, "user"); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.EnqueueDeploy(ctx, deploy(svc)); err != nil {
				t.Fatal(err)
			}
			close(release)
			waitFor(t, "the job to end cancelled", func() bool { return e.state(t, first.JobID) == StateCancelled })
			if runs.Load() != 1 || !sawCancelled.Load() {
				t.Errorf("runs %d, started already cancelled: %v", runs.Load(), sawCancelled.Load())
			}
		})
	}
}

func TestRetryableErrorAfterCancelIsNotRequeued(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var runs atomic.Int32
	var id int64
	s, _ := start(t, e, func(context.Context, Job) error {
		runs.Add(1)
		// The flag is set in the database only (as a supersede from another
		// process would); the runner then fails with a transient error.
		if _, err := e.js.cancel(ctx, id, "superseded"); err != nil {
			t.Error(err)
		}
		return Retryable(errors.New("503"))
	}, nil)
	s.OpenGate()
	n := job("service:1")
	n.MaxAttempts = DeployAttempts
	id, _ = e.js.Enqueue(ctx, n)
	s.Notify()
	waitFor(t, "the job to end", func() bool { return e.state(t, id) == StateCancelled })
	time.Sleep(100 * time.Millisecond)
	if runs.Load() != 1 {
		t.Errorf("ran %d times: the retry undid the cancel", runs.Load())
	}
	// The job ends with the reason it was cancelled for, not a generic "cancelled".
	var reason, errText string
	if err := e.st.R.QueryRow(`SELECT COALESCE(cancel_reason, ''), COALESCE(error, '') FROM jobs WHERE id = ?`, id).Scan(&reason, &errText); err != nil {
		t.Fatal(err)
	}
	if reason != "superseded" || errText != "superseded" {
		t.Errorf("cancel_reason %q, error %q: want superseded", reason, errText)
	}
	if ok, err := e.js.Requeue(ctx, id, 0, "x"); err != nil || ok {
		t.Errorf("Requeue of a finished job: %v %v", ok, err)
	}
}

func (e *env) deployRow(t *testing.T, id int64) (status, code string, finished sql.NullString, by sql.NullInt64) {
	t.Helper()
	var c sql.NullString
	if err := e.st.R.QueryRow(`SELECT status, reason_code, finished_at, superseded_by FROM deploys WHERE id = ?`, id).Scan(&status, &c, &finished, &by); err != nil {
		t.Fatal(err)
	}
	return status, c.String, finished, by
}

func TestCancelOfAQueuedDeployJobCancelsItsDeploy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	svc := e.service(t, "web")
	d, err := e.js.EnqueueDeploy(ctx, deploy(svc))
	if err != nil {
		t.Fatal(err)
	}
	if st, err := e.js.cancel(ctx, d.JobID, "cancelled by the user"); err != nil || st != StateCancelled {
		t.Fatal(st, err)
	}
	status, code, fin, _ := e.deployRow(t, d.DeployID)
	if status != "cancelled" || code != "cancelled" || !fin.Valid {
		t.Errorf("deploy %s %s finished=%v: left non-final", status, code, fin)
	}
}

// A job waiting to retry is queued while its deploy is running. A newer deploy
// ends both: the deploy must not be left 'running' forever.
func TestSupersedeOfARetryPendingJobFinishesItsRunningDeploy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	svc := e.service(t, "web")
	first, _ := e.js.EnqueueDeploy(ctx, deploy(svc))
	if _, ok, err := e.js.Pick(ctx, "b"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	e.exec(t, `UPDATE deploys SET status = 'running' WHERE id = ?`, first.DeployID)
	if ok, err := e.js.Requeue(ctx, first.JobID, time.Hour, "503"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	second, err := e.js.EnqueueDeploy(ctx, deploy(svc))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Superseded) != 1 || second.Superseded[0] != first.JobID {
		t.Fatalf("superseded %v", second.Superseded)
	}
	status, _, fin, by := e.deployRow(t, first.DeployID)
	if status != "superseded" || !fin.Valid || by.Int64 != second.DeployID {
		t.Errorf("deploy %s finished=%v by=%v", status, fin, by)
	}
	if e.state(t, first.JobID) != StateCancelled {
		t.Error("job not cancelled")
	}
	// Cancelling a retry-pending job finishes its running deploy too.
	third, _ := e.js.EnqueueDeploy(ctx, deploy(e.service(t, "db")))
	e.js.Pick(ctx, "b")
	e.exec(t, `UPDATE deploys SET status = 'running' WHERE id = ?`, third.DeployID)
	e.js.Requeue(ctx, third.JobID, time.Hour, "503")
	if _, err := e.js.cancel(ctx, third.JobID, "user"); err != nil {
		t.Fatal(err)
	}
	if status, _, _, _ := e.deployRow(t, third.DeployID); status != "cancelled" {
		t.Errorf("deploy %s after cancelling its retry-pending job", status)
	}
}

// A store error in Pick must not strand a due job: the scheduler looks again.
func TestStoreErrorDoesNotStrandADueJob(t *testing.T) {
	e := newEnv(t)
	e.exec(t, `CREATE TRIGGER no_run BEFORE UPDATE OF state ON jobs WHEN NEW.state = 'running' BEGIN SELECT RAISE(ABORT, 'store is unhappy'); END`)
	s, _ := start(t, e, func(context.Context, Job) error { return nil }, func(c *Config) { c.storeRetry = 50 * time.Millisecond })
	s.OpenGate()
	id, _ := s.Enqueue(context.Background(), job("service:1"))
	waitFor(t, "the retry timer", func() bool { _, ok := s.NextWake(); return ok })
	time.Sleep(120 * time.Millisecond)
	if e.state(t, id) != StateQueued {
		t.Fatal("ran despite the trigger")
	}
	e.exec(t, `DROP TRIGGER no_run`)
	waitFor(t, "the job to run once the store recovers", func() bool { return e.state(t, id) == StateSucceeded })
}

// A retry that becomes due between the scheduler's last Pick and its next-wake
// query must still run. The store clock here jumps 40 ms on every reading, so
// the requeued job (due 60 ms after the requeue) is not due at the Pick's
// reading and is due at any later one: before the fix the timer query used that
// later reading, found "no not-yet-due job", stopped the timer, and the job sat
// queued until some unrelated wake-up (an intermittent 5 s timeout of
// TestPullRetriesThenSucceeds under -race load).
func TestRetryDueBetweenPickAndTimerIsNotLost(t *testing.T) {
	e := newEnv(t)
	var tick atomic.Int64
	base := time.Now()
	e.js = NewStore(e.st, func() time.Time { return base.Add(time.Duration(tick.Add(1)) * 40 * time.Millisecond) })
	var attempts atomic.Int32
	s, _ := start(t, e, func(_ context.Context, j Job) error {
		attempts.Add(1)
		if j.Attempt < 2 {
			return Retryable(errors.New("registry 503"))
		}
		return nil
	}, func(c *Config) { c.RetryDelay = func(int) time.Duration { return 60 * time.Millisecond } })
	s.OpenGate()
	n := job("service:1")
	n.MaxAttempts = DeployAttempts
	id, err := s.Enqueue(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the retry to run", func() bool { return e.state(t, id) == StateSucceeded })
	if attempts.Load() != 2 {
		t.Errorf("%d attempts", attempts.Load())
	}
}

// The queue's backstop: whatever the runner did or did not do, a deploy job's end
// (failed or cancelled) ends its deploy row in the same transaction, frees its
// address and tidies its stages; a row the runner ended is not rewritten.
func TestFinishEndsTheDeployRowTheRunnerLeftLive(t *testing.T) {
	type tc struct {
		name, state, errText string
		status, code, reason string
		denied               bool
	}
	cases := []tc{
		{name: "denied", state: StateFailed, errText: "the API token that queued this job was revoked", denied: true, status: "failed", code: "denied", reason: DeniedReason},
		{name: "failed", state: StateFailed, errText: "the job panicked", status: "failed", code: "job_failed", reason: "the job panicked"},
		{name: "cancelled", state: StateCancelled, errText: "cancelled by the user", status: "cancelled", code: "cancelled", reason: "cancelled by the user"},
		{name: "superseded", state: StateCancelled, errText: "superseded", status: "superseded", code: "superseded", reason: "superseded by a newer deploy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			svc := e.service(t, "web")
			enq, err := e.js.EnqueueDeploy(ctx, deploy(svc))
			if err != nil {
				t.Fatal(err)
			}
			newer, err := e.js.EnqueueDeploy(ctx, deploy(svc)) // the deploy that replaced it (for "superseded")
			if err != nil {
				t.Fatal(err)
			}
			// Make the first one a running deploy that holds an address, mid-stage.
			e.exec(t, `UPDATE jobs SET state = 'running', boot_id = 'b' WHERE id = ?`, enq.JobID)
			e.exec(t, `UPDATE deploys SET status = 'running', ip = '10.250.0.7', stage = 'health' WHERE id = ?`, enq.DeployID)
			// The service has a live version: the backstop still says unknown (it cannot see it).
			e.exec(t, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, newer.DeployID, svc)
			e.exec(t, `INSERT INTO deploy_stages(deploy_id, stage, state) VALUES (?, 'pull', 'ok'), (?, 'health', 'running'), (?, 'switch', 'pending')`, enq.DeployID, enq.DeployID, enq.DeployID)
			if err := e.js.finish(ctx, enq.JobID, c.state, c.errText, c.denied); err != nil {
				t.Fatal(err)
			}
			var status, code, reason, safe string
			var by sql.NullInt64
			if err := e.st.R.QueryRow(`SELECT status, reason_code, reason, COALESCE(app_safe, ''), superseded_by FROM deploys WHERE id = ?`, enq.DeployID).Scan(&status, &code, &reason, &safe, &by); err != nil {
				t.Fatal(err)
			}
			if status != c.status || code != c.code || reason != c.reason || safe != "unknown" {
				t.Errorf("deploy: %s %s %q app_safe %q", status, code, reason, safe)
			}
			if c.name == "superseded" && (!by.Valid || by.Int64 != newer.DeployID) {
				t.Errorf("superseded_by %v, want %d", by, newer.DeployID)
			}
			stages := map[string]string{}
			rs, _ := e.st.R.Query(`SELECT stage, state FROM deploy_stages WHERE deploy_id = ?`, enq.DeployID)
			for rs.Next() {
				var a, b string
				rs.Scan(&a, &b)
				stages[a] = b
			}
			rs.Close()
			if stages["pull"] != "ok" || stages["health"] != "failed" || stages["switch"] != "skipped" {
				t.Errorf("stages %v", stages)
			}
			// The address is free again: the live-deploy unique index takes the same one.
			e.exec(t, `INSERT INTO deploys(service_id, number, kind, snapshot_json, ip, status, actor_kind, actor_id, created_at) VALUES (?, 99, 'deploy', '{}', '10.250.0.7', 'running', 'user', '1', 'x')`, svc)
		})
	}

	t.Run("a row the runner ended is not rewritten, and a succeeded job touches nothing", func(t *testing.T) {
		e := newEnv(t)
		svc := e.service(t, "web")
		enq, _ := e.js.EnqueueDeploy(ctx, deploy(svc))
		e.exec(t, `UPDATE jobs SET state = 'running', boot_id = 'b' WHERE id = ?`, enq.JobID)
		e.exec(t, `UPDATE deploys SET status = 'failed', reason_code = 'health_timeout', reason = 'precise', app_safe = 'yes' WHERE id = ?`, enq.DeployID)
		if err := e.js.Finish(ctx, enq.JobID, StateFailed, "the runner's own error"); err != nil {
			t.Fatal(err)
		}
		var code, reason, safe string
		e.st.R.QueryRow(`SELECT reason_code, reason, app_safe FROM deploys WHERE id = ?`, enq.DeployID).Scan(&code, &reason, &safe)
		if code != "health_timeout" || reason != "precise" || safe != "yes" {
			t.Errorf("rewritten: %s %q %s", code, reason, safe)
		}
		enq2, _ := e.js.EnqueueDeploy(ctx, deploy(svc))
		e.exec(t, `UPDATE jobs SET state = 'running', boot_id = 'b' WHERE id = ?`, enq2.JobID)
		e.exec(t, `UPDATE deploys SET status = 'running' WHERE id = ?`, enq2.DeployID)
		if err := e.js.Finish(ctx, enq2.JobID, StateSucceeded, ""); err != nil {
			t.Fatal(err)
		}
		var st string
		e.st.R.QueryRow(`SELECT status FROM deploys WHERE id = ?`, enq2.DeployID).Scan(&st)
		if st != "running" {
			t.Errorf("a succeeded job ended its deploy as %s: only the runner decides success", st)
		}
	})
}

// The succeeded side of the backstop (step 8b): a job that ends succeeded while its
// deploy row is still running AND committed (the runner could not write the end of a
// deploy that had committed) ends the row as succeeded, "record incomplete", with
// its stages tidied. A row that never committed is left alone.
func TestSucceededJobSettlesACommittedDeployRow(t *testing.T) {
	e := newEnv(t)
	svc := e.service(t, "web")
	enq, err := e.js.EnqueueDeploy(ctx, deploy(svc))
	if err != nil {
		t.Fatal(err)
	}
	e.exec(t, `UPDATE jobs SET state = 'running', boot_id = 'b' WHERE id = ?`, enq.JobID)
	e.exec(t, `UPDATE deploys SET status = 'running', ip = '10.250.0.7', stage = 'drain', committed_at = 'x' WHERE id = ?`, enq.DeployID)
	e.exec(t, `INSERT INTO deploy_stages(deploy_id, stage, state) VALUES (?, 'commit', 'ok'), (?, 'drain', 'running'), (?, 'stop_old', 'pending')`, enq.DeployID, enq.DeployID, enq.DeployID)
	if err := e.js.Finish(ctx, enq.JobID, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	var status, stage, code, reason string
	if err := e.st.R.QueryRow(`SELECT status, stage, reason_code, reason FROM deploys WHERE id = ?`, enq.DeployID).Scan(&status, &stage, &code, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || stage != "done" || code != "warning" || reason != RecordIncompleteReason {
		t.Errorf("deploy: %s %s %s %q", status, stage, code, reason)
	}
	got := map[string]string{}
	rs, _ := e.st.R.Query(`SELECT stage, state FROM deploy_stages WHERE deploy_id = ?`, enq.DeployID)
	for rs.Next() {
		var a, b string
		rs.Scan(&a, &b)
		got[a] = b
	}
	rs.Close()
	if got["commit"] != "ok" || got["drain"] != "ok" || got["stop_old"] != "skipped" {
		t.Errorf("stages %v", got)
	}
	// The address is free again.
	e.exec(t, `INSERT INTO deploys(service_id, number, kind, snapshot_json, ip, status, actor_kind, actor_id, created_at) VALUES (?, 99, 'deploy', '{}', '10.250.0.7', 'running', 'user', '1', 'x')`, svc)

	// The other half: a row that never committed is left alone by a succeeded job.
	svc2 := e.service(t, "api")
	enq2, _ := e.js.EnqueueDeploy(ctx, deploy(svc2))
	e.exec(t, `UPDATE jobs SET state = 'running', boot_id = 'b' WHERE id = ?`, enq2.JobID)
	e.exec(t, `UPDATE deploys SET status = 'running' WHERE id = ?`, enq2.DeployID)
	if err := e.js.Finish(ctx, enq2.JobID, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	var st2 string
	e.st.R.QueryRow(`SELECT status FROM deploys WHERE id = ?`, enq2.DeployID).Scan(&st2)
	if st2 != "running" {
		t.Errorf("a row that never committed became %s", st2)
	}

	// And a cancelled or failed job never records a committed deploy as such.
	for _, state := range []string{StateCancelled, StateFailed} {
		svc3 := e.service(t, "w"+state)
		enq3, _ := e.js.EnqueueDeploy(ctx, deploy(svc3))
		e.exec(t, `UPDATE jobs SET state = 'running', boot_id = 'b' WHERE id = ?`, enq3.JobID)
		e.exec(t, `UPDATE deploys SET status = 'running', stage = 'drain', committed_at = 'x' WHERE id = ?`, enq3.DeployID)
		if err := e.js.Finish(ctx, enq3.JobID, state, "superseded"); err != nil {
			t.Fatal(err)
		}
		var st3 string
		e.st.R.QueryRow(`SELECT status FROM deploys WHERE id = ?`, enq3.DeployID).Scan(&st3)
		if st3 != "succeeded" {
			t.Errorf("job %s: a committed deploy was recorded %s", state, st3)
		}
	}
}

// The cancel and the stage check are one transaction: a deploy in switch (or any
// later stage) cannot be cancelled, one before it can, and nothing is flagged when
// the cancel is refused.
func TestCancelDeployChecksTheStageAtomicallyWithTheFlag(t *testing.T) {
	e := newEnv(t)
	svc := e.service(t, "web")
	enq, _ := e.js.EnqueueDeploy(ctx, deploy(svc))
	e.exec(t, `UPDATE jobs SET state = 'running', boot_id = 'b' WHERE id = ?`, enq.JobID)
	e.exec(t, `UPDATE deploys SET status = 'running', stage = 'switch' WHERE id = ?`, enq.DeployID)
	forbidden := []string{"switch", "commit", "drain", "stop_old"}
	s, _ := start(t, e, func(ctx context.Context, j Job) error { return nil }, nil)
	if _, err := s.CancelDeploy(ctx, enq.JobID, "cancelled", forbidden); !errors.Is(err, ErrStageNotCancellable) {
		t.Fatalf("a cancel in switch: %v", err)
	}
	var flag int
	e.st.R.QueryRow(`SELECT cancel_requested FROM jobs WHERE id = ?`, enq.JobID).Scan(&flag)
	if flag != 0 {
		t.Error("a refused cancel still set the flag")
	}
	e.exec(t, `UPDATE deploys SET stage = 'health' WHERE id = ?`, enq.DeployID)
	state, err := s.CancelDeploy(ctx, enq.JobID, "cancelled", forbidden)
	if err != nil || state != StateRunning {
		t.Fatalf("%s %v", state, err)
	}
	e.st.R.QueryRow(`SELECT cancel_requested FROM jobs WHERE id = ?`, enq.JobID).Scan(&flag)
	if flag != 1 {
		t.Error("the cancel did not flag the job")
	}
	// A queued deploy is cancelled at once and reports it.
	q, _ := e.js.EnqueueDeploy(ctx, deploy(e.service(t, "api")))
	if state, err := s.CancelDeploy(ctx, q.JobID, "cancelled", forbidden); err != nil || state != StateCancelled {
		t.Fatalf("%s %v", state, err)
	}
}

// A deploy that has already ended (its job is about to) is never "cancelling".
func TestCancelDeployRefusesAnEndedDeploy(t *testing.T) {
	e := newEnv(t)
	svc := e.service(t, "web")
	enq, _ := e.js.EnqueueDeploy(ctx, deploy(svc))
	e.exec(t, `UPDATE jobs SET state = 'running', boot_id = 'b' WHERE id = ?`, enq.JobID)
	e.exec(t, `UPDATE deploys SET status = 'succeeded', stage = 'done' WHERE id = ?`, enq.DeployID)
	if _, err := e.js.cancelDeploy(ctx, enq.JobID, "cancelled", []string{"switch"}); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("err %v, want ErrNotCancellable", err)
	}
	var flag int
	e.st.R.QueryRow(`SELECT cancel_requested FROM jobs WHERE id = ?`, enq.JobID).Scan(&flag)
	if flag != 0 {
		t.Error("the cancel flag was set")
	}
}

// A queued delete cancels the service's queued deploys and restarts in the same
// transaction, flags the running job (cancel reason: deleted) and closes its
// drain-cut signal; later work for the service is refused, and another delete too.
func TestEnqueueDeleteCancelsQueuedFlagsRunningAndRefusesNewWork(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	svc, other := e.service(t, "web"), e.service(t, "other")
	started := make(chan int64, 1)
	var cause atomic.Value
	s, _ := start(t, e, func(ctx context.Context, j Job) error {
		if j.Kind == KindServiceDelete {
			return nil
		}
		started <- j.ID
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return ctx.Err()
	}, nil)
	s.OpenGate()
	running, err := s.EnqueueDeploy(ctx, deploy(svc))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	signal := s.DeleteSignal(running.JobID)
	if signal == nil {
		t.Fatal("no delete signal for a running job")
	}
	select {
	case <-signal:
		t.Fatal("the signal was closed before a delete")
	default:
	}
	// Queued behind the running one: a restart and, for another service, a deploy.
	restart, err := s.Enqueue(ctx, NewJob{Kind: KindRestart, LockKey: LockKey(svc), Actor: testActor, Payload: json.RawMessage(`{}`), TimeoutS: 60})
	if err != nil {
		t.Fatal(err)
	}
	otherDeploy, err := s.EnqueueDeploy(ctx, deploy(other))
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.EnqueueDelete(ctx, NewDelete{ServiceID: svc, Target: "p/web", Actor: testActor, StopGraceS: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Cancelled) != 1 || out.Cancelled[0] != restart || len(out.CancelRunning) != 1 || out.CancelRunning[0] != running.JobID {
		t.Fatalf("%+v", out)
	}
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("the drain-cut signal was not closed")
	}
	waitFor(t, "the flagged job to end", func() bool { return e.state(t, running.JobID) == StateCancelled })
	var cc CancelCause
	if c, _ := cause.Load().(error); !errors.As(c, &cc) || cc.Reason != ReasonDeleted {
		t.Errorf("cancel cause %v", c)
	}
	if got := e.state(t, restart); got != StateCancelled {
		t.Errorf("the queued restart is %s", got)
	}
	// The delete job: the target is its Confirm, the payload names the service.
	var kind, confirm, payload string
	var timeout int
	e.st.R.QueryRow(`SELECT kind, confirm, payload_json, timeout_s FROM jobs WHERE id = ?`, out.JobID).Scan(&kind, &confirm, &payload, &timeout)
	if kind != KindServiceDelete || confirm != "p/web" || timeout != DeleteTimeoutSeconds(10) || !strings.Contains(payload, `"service":`+fmt.Sprint(svc)) {
		t.Errorf("delete job: %s %q %d %s", kind, confirm, timeout, payload)
	}
	waitFor(t, "the delete job to run", func() bool { return e.state(t, out.JobID) == StateSucceeded })
	// The other service is untouched.
	_ = otherDeploy
	// A delete that is pending (or running) refuses new work and a second delete.
	e.exec(t, `UPDATE jobs SET state = 'queued', finished_at = NULL WHERE id = ?`, out.JobID)
	if _, err := s.EnqueueDeploy(ctx, deploy(svc)); !errors.Is(err, ErrServiceDeleting) {
		t.Errorf("deploy: %v", err)
	}
	if _, err := s.Enqueue(ctx, job(LockKey(svc))); !errors.Is(err, ErrServiceDeleting) {
		t.Errorf("restart: %v", err)
	}
	if _, err := s.EnqueueDelete(ctx, NewDelete{ServiceID: svc, Target: "p/web", Actor: testActor}); !errors.Is(err, ErrServiceDeleting) {
		t.Errorf("second delete: %v", err)
	}
	if _, err := s.EnqueueDeploy(ctx, deploy(other)); err != nil {
		t.Errorf("another service: %v", err)
	}
}

func TestEnqueueDeleteValidates(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []NewDelete{
		{ServiceID: 0, Target: "p/web", Actor: testActor},
		{ServiceID: 1, Target: "", Actor: testActor},
		{ServiceID: 1, Target: "p/web", Actor: testActor, StopGraceS: 301},
		{ServiceID: 1, Target: "p/web", Actor: Actor{Kind: "system", ID: "x", Blob: "x"}},
	} {
		if _, err := e.js.EnqueueDelete(context.Background(), bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}
