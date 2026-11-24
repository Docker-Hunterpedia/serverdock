package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// RunFunc runs one job. It is the deploy package's entry point (step 8a). ctx is
// cancelled when the job is cancelled or superseded (context.Cause says why) and
// when the server shuts down (ErrShutdown). A nil return is success; an error
// wrapped with Retryable is retried; any other error fails the job.
type RunFunc func(ctx context.Context, j Job) error

// ErrShutdown is the cancel cause of a job interrupted by Shutdown. Such a job is
// left "running" on purpose: the next boot's pass resolves it (plan 0004,
// Reconciler), because only it can tell whether the deploy had committed.
var ErrShutdown = errors.New("jobs: server is shutting down")

// CancelCause is the cancel cause of a job cancelled by Cancel or superseded.
type CancelCause struct{ Reason string }

func (c CancelCause) Error() string { return "jobs: cancelled: " + c.Reason }

// Config configures a Scheduler.
type Config struct {
	Store *Store
	Run   RunFunc
	Log   *slog.Logger
	// Workers is the most jobs running at once: 1 or 2 (default 2).
	Workers int
	// Now replaces the clock in tests; nil uses the Store's own clock, which is what
	// production does. A non-nil Now MUST be the same clock as the Store's
	// (jobs.NewStore's now argument): arm subtracts this one from a time the store
	// wrote, so two different clocks would make a retry fire early or late.
	Now func() time.Time
	// RetryDelay replaces the package RetryDelay in tests.
	RetryDelay func(failedAttempt int) time.Duration

	// Test hooks (zero in production).
	afterPick  func(Job)     // runs between Pick and the worker's registration
	storeRetry time.Duration // wait before retrying after a store error (default 5 s)
}

// Scheduler is one goroutine and one timer. It sleeps on {timer, notify, ctx}.
// After each pass it arms the timer for the earliest not-yet-due queued job, or
// stops it when there is none, so an idle server has no timer at all. A due job
// that is blocked (its lock key is running, or both workers are busy) is not
// timed: the end of the running job wakes the scheduler.
type Scheduler struct {
	cfg     Config
	bootID  string
	notify  chan struct{} // capacity 1: wake-ups coalesce
	gate    chan struct{} // closed by OpenGate
	gateOne sync.Once

	mu       sync.Mutex
	paused   bool
	stopping bool
	running  map[int64]context.CancelCauseFunc
	active   int              // workers started and not finished (running is removed earlier, before the result is recorded)
	pending  map[int64]string // cancels that arrived during a pick (picking), before the picked job was registered; emptied when that pick ends
	picking  bool             // dispatch is between Pick and registering its worker (one pick at a time)
	wake     time.Time        // when the timer fires; zero when it is stopped
	wg       sync.WaitGroup
}

// NewScheduler validates cfg. It starts nothing: call Run.
func NewScheduler(cfg Config) (*Scheduler, error) {
	if cfg.Store == nil || cfg.Run == nil {
		return nil, errors.New("jobs: a Store and a Run function are required")
	}
	if cfg.Workers == 0 {
		cfg.Workers = MaxWorkers
	}
	if cfg.Workers < 1 || cfg.Workers > MaxWorkers {
		return nil, errors.New("jobs: Workers must be 1 or 2")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = cfg.Store.now // one clock: the store's (see Config.Now)
	}
	if cfg.RetryDelay == nil {
		cfg.RetryDelay = RetryDelay
	}
	if cfg.storeRetry == 0 {
		cfg.storeRetry = 5 * time.Second
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return &Scheduler{
		cfg: cfg, bootID: hex.EncodeToString(b[:]),
		notify: make(chan struct{}, 1), gate: make(chan struct{}),
		running: map[int64]context.CancelCauseFunc{}, pending: map[int64]string{},
	}, nil
}

// BootID identifies this scheduler's run; it is stored on every job it starts.
func (s *Scheduler) BootID() string { return s.bootID }

// Notify wakes the scheduler (a job was enqueued or finished). It never blocks.
func (s *Scheduler) Notify() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// OpenGate is the start gate: the scheduler dispatches nothing until the
// reconciler's first successful resync calls it, so no job runs against a state
// map that has not been filled yet. It is idempotent.
func (s *Scheduler) OpenGate() {
	s.gateOne.Do(func() { close(s.gate) })
	s.Notify()
}

func (s *Scheduler) gateOpen() bool {
	select {
	case <-s.gate:
		return true
	default:
		return false
	}
}

// Pause stops dispatching; running jobs continue. M7b pauses before its "no job
// running" check, and shutdown pauses first.
func (s *Scheduler) Pause() {
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
}

// Resume undoes Pause.
func (s *Scheduler) Resume() {
	s.mu.Lock()
	s.paused = false
	s.mu.Unlock()
	s.Notify()
}

// Running is the number of jobs running now.
func (s *Scheduler) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// NextWake reports when the timer is set to fire; ok is false when it is stopped.
func (s *Scheduler) NextWake() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wake, !s.wake.IsZero()
}

// Enqueue adds a job and wakes the scheduler.
func (s *Scheduler) Enqueue(ctx context.Context, n NewJob) (int64, error) {
	id, err := s.cfg.Store.Enqueue(ctx, n)
	if err == nil {
		s.Notify()
	}
	return id, err
}

// EnqueueDeploy queues a deploy, closes the in-memory cancel of the running
// deploy jobs it superseded, and wakes the scheduler.
func (s *Scheduler) EnqueueDeploy(ctx context.Context, n NewDeploy) (Enqueued, error) {
	out, err := s.cfg.Store.EnqueueDeploy(ctx, n)
	if err != nil {
		return out, err
	}
	for _, id := range out.CancelRunning {
		s.cancelRunning(id, "superseded")
	}
	s.Notify()
	return out, nil
}

// Cancel cancels a job: a queued one at once, a running one by flagging it and
// closing its context (the runner stops at its next cancellable point).
func (s *Scheduler) Cancel(ctx context.Context, id int64, reason string) error {
	state, err := s.cfg.Store.cancel(ctx, id, reason)
	if err != nil {
		return err
	}
	if state == StateRunning {
		s.cancelRunning(id, reason)
	}
	s.Notify()
	return nil
}

// MaxSessionsPerCancel bounds one CancelSessionJobs call.
const MaxSessionsPerCancel = 1000

// CancelSessionJobsTx cancels the queued and running jobs queued by the given
// sessions (public ids) inside the caller's write transaction. The returned
// function closes the in-memory cancel of the running ones and wakes the
// scheduler; the caller runs it only after the transaction committed.
func (s *Scheduler) CancelSessionJobsTx(ctx context.Context, tx *sql.Tx, sessions []string, reason string) (afterCommit func(), err error) {
	if len(sessions) > MaxSessionsPerCancel {
		return nil, invalid("too many sessions")
	}
	running, n, err := s.cfg.Store.CancelSessionJobsTx(ctx, tx, sessions, reason)
	if err != nil {
		return nil, err
	}
	return func() {
		for _, id := range running {
			s.cancelRunning(id, reason)
		}
		if n > 0 {
			s.Notify()
		}
	}, nil
}

// CancelSessionJobs is CancelSessionJobsTx in its own transaction. It returns
// how many jobs were cancelled.
func (s *Scheduler) CancelSessionJobs(ctx context.Context, sessions []string, reason string) (int, error) {
	if len(sessions) == 0 {
		return 0, nil
	}
	var after func()
	var count int
	err := s.cfg.Store.st.WriteTx(ctx, func(tx *sql.Tx) error {
		running, n, err := s.cfg.Store.CancelSessionJobsTx(ctx, tx, sessions, reason)
		if err != nil {
			return err
		}
		count = n
		after = func() {
			for _, id := range running {
				s.cancelRunning(id, reason)
			}
			s.Notify()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	after()
	return count, nil
}

func (s *Scheduler) cancelRunning(id int64, reason string) {
	s.mu.Lock()
	cancel := s.running[id]
	if cancel == nil && s.picking {
		// Possibly picked an instant ago and not registered yet: dispatch checks
		// this set when it registers the worker, so the cancel is never lost.
		// Outside a pick, an unregistered running job has already finished (its
		// worker left the map before recording), so there is nothing to cancel.
		s.pending[id] = reason
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel(CancelCause{Reason: reason})
	}
}

// Run is the scheduler loop. It returns after ctx is cancelled and every worker
// has returned (running jobs get a context cancelled with ErrShutdown).
func (s *Scheduler) Run(ctx context.Context) {
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
		s.stop()
		s.wg.Wait()
	}()
	for {
		timerC = nil
		if s.gateOpen() {
			failed, since := s.dispatch(ctx)
			timerC = s.arm(ctx, &timer, failed, since)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.notify:
		case <-timerC:
		}
	}
}

// arm sets the one timer to the earliest job that was not due when dispatch last
// looked (since), or stops it. It must not use a later clock reading: a job that
// became due between dispatch's last pick and this query would be neither picked
// nor timed, and would sit queued until some unrelated wake-up.
func (s *Scheduler) arm(ctx context.Context, timer **time.Timer, storeFailed bool, since time.Time) <-chan time.Time {
	s.mu.Lock()
	paused := s.paused || s.stopping
	s.mu.Unlock()
	var at time.Time
	var ok bool
	if !paused {
		var err error
		if at, ok, err = s.cfg.Store.NextDue(ctx, since); err != nil {
			s.cfg.Log.Error("jobs: next due", "err", err)
			storeFailed = true
		}
		if storeFailed {
			// A store error must not strand a due job: look again shortly.
			at, ok = s.cfg.Now().Add(s.cfg.storeRetry), true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ok {
		s.wake = time.Time{}
		if *timer != nil {
			(*timer).Stop()
		}
		return nil
	}
	// at is a store-clock time and Now is the scheduler's clock: the same clock by
	// contract (see Config.Now and Store), so the difference is the real wait.
	d := at.Sub(s.cfg.Now())
	if d < 0 {
		d = 0
	}
	s.wake = at
	if *timer == nil {
		*timer = time.NewTimer(d)
	} else {
		(*timer).Reset(d)
	}
	return (*timer).C
}

// dispatch starts due jobs while a worker is free. since is the store clock's
// reading taken before the last look at the queue: every job due at or before it
// was considered (picked, or blocked and left to the wake-up of the job it waits
// for), so arm times only the jobs due after it.
func (s *Scheduler) dispatch(ctx context.Context) (storeFailed bool, since time.Time) {
	since = s.cfg.Store.now()
	for ctx.Err() == nil {
		s.mu.Lock()
		blocked := s.paused || s.stopping || s.active >= s.cfg.Workers
		s.picking = !blocked
		s.mu.Unlock()
		if blocked {
			return false, since
		}
		since = s.cfg.Store.now() // before Pick reads its own, never after
		j, ok, err := s.cfg.Store.Pick(ctx, s.bootID)
		if err != nil || !ok {
			s.endPick()
		}
		if err != nil {
			if ctx.Err() == nil {
				s.cfg.Log.Error("jobs: pick", "err", err)
				return true, since
			}
			return false, since
		}
		if !ok {
			return false, since
		}
		if s.cfg.afterPick != nil {
			s.cfg.afterPick(j)
		}
		jctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
		s.mu.Lock()
		if s.stopping {
			// Picked in the instant of shutdown: no worker starts, and the row stays
			// "running" for the next boot's pass (see ErrShutdown).
			s.picking = false
			clear(s.pending)
			s.mu.Unlock()
			cancel(ErrShutdown)
			return false, since
		}
		s.running[j.ID] = cancel
		s.active++
		if reason, ok := s.pending[j.ID]; ok {
			cancel(CancelCause{Reason: reason}) // a cancel that raced the pick: the worker starts already cancelled
		} else if j.CancelRequested {
			cancel(CancelCause{Reason: j.CancelReason})
		}
		// One pick at a time: any other entry is a cancel for a job that already
		// finished, so the whole set is dropped with this pick.
		s.picking = false
		clear(s.pending)
		s.wg.Add(1) // under mu: stop() cannot run between the check and the Add
		s.mu.Unlock()
		go s.work(jctx, cancel, j)
	}
	return false, since
}

// endPick ends a pick that registered no worker.
func (s *Scheduler) endPick() {
	s.mu.Lock()
	s.picking = false
	clear(s.pending)
	s.mu.Unlock()
}

func (s *Scheduler) work(ctx context.Context, cancel context.CancelCauseFunc, j Job) {
	defer s.wg.Done()
	err := s.safeRun(ctx, j)
	cause := context.Cause(ctx)
	cancel(nil)
	// Leave the cancel map before the result is recorded: a cancel that arrives
	// from here on only flags the row (the outcome is already decided by cause).
	// It adds a pending entry only while a pick is in progress, and that pick's
	// end drops it.
	s.mu.Lock()
	delete(s.running, j.ID)
	s.mu.Unlock()
	// The bookkeeping must survive the job's own cancellation.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer bcancel()
	s.record(bctx, j, err, cause)
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	s.Notify()
}

func (s *Scheduler) safeRun(ctx context.Context, j Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			s.cfg.Log.Error("jobs: a job panicked", "job", j.ID, "kind", j.Kind, "panic", p)
			err = errors.New("the job panicked")
		}
	}()
	return s.cfg.Run(ctx, j)
}

func (s *Scheduler) record(ctx context.Context, j Job, runErr, cause error) {
	st := s.cfg.Store
	var cc CancelCause
	cancelled := errors.As(cause, &cc)
	// A failed write is tried once more. If it fails again the row stays
	// "running" and the next boot's pass resolves it (plan 0004, Reconciler).
	try := func(f func() error) error {
		if err := f(); err != nil {
			return f()
		}
		return nil
	}
	var err error
	switch {
	case runErr == nil:
		err = try(func() error { return st.Finish(ctx, j.ID, StateSucceeded, "") })
	case errors.Is(cause, ErrShutdown):
		// Left running for the boot pass (see ErrShutdown).
		s.cfg.Log.Info("jobs: a job was interrupted by shutdown", "job", j.ID)
		return
	case cancelled:
		err = try(func() error { return st.Finish(ctx, j.ID, StateCancelled, cc.Reason) })
	case IsRetryable(runErr) && j.Attempt < j.MaxAttempts:
		err = try(func() error {
			ok, err := st.Requeue(ctx, j.ID, s.cfg.RetryDelay(j.Attempt), runErr.Error())
			if err == nil && !ok {
				// A cancel was requested during the attempt: it wins over the retry,
				// and the job ends with the reason it was cancelled for.
				reason := "cancelled"
				if cur, gerr := st.Get(ctx, j.ID); gerr == nil && cur.CancelReason != "" {
					reason = cur.CancelReason
				}
				return st.Finish(ctx, j.ID, StateCancelled, reason)
			}
			return err
		})
	default:
		err = try(func() error { return st.Finish(ctx, j.ID, StateFailed, runErr.Error()) })
	}
	if err != nil {
		s.cfg.Log.Error("jobs: record result (the boot pass will resolve it)", "job", j.ID, "err", err)
	}
}

// stop pauses dispatching and cancels every running job's context.
func (s *Scheduler) stop() {
	s.mu.Lock()
	s.stopping = true
	cancels := make([]context.CancelCauseFunc, 0, len(s.running))
	for _, c := range s.running {
		cancels = append(cancels, c)
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c(ErrShutdown)
	}
}

// Shutdown pauses the scheduler, cancels running jobs (cause ErrShutdown) and
// waits for their workers until ctx ends. Run does the same when its context is
// cancelled; Shutdown is for a caller that needs the wait bounded.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.stop()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
