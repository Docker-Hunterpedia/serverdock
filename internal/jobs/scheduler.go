package jobs

import (
	"context"
	"crypto/rand"
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
	// Now replaces the clock in tests.
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
	pending  map[int64]string // cancels that arrived between Pick and registration
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
		cfg.Now = time.Now
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

// CancelSessionJobs cancels the queued and running jobs queued by the given
// sessions (public ids), for an explicit session revoke. A running job stops at
// its next cancellable point. It returns how many jobs were cancelled.
func (s *Scheduler) CancelSessionJobs(ctx context.Context, sessions []string, reason string) (int, error) {
	if len(sessions) == 0 {
		return 0, nil
	}
	if len(sessions) > MaxSessionsPerCancel {
		return 0, invalid("too many sessions")
	}
	running, n, err := s.cfg.Store.cancelBySession(ctx, sessions, reason)
	if err != nil {
		return 0, err
	}
	for _, id := range running {
		s.cancelRunning(id, reason)
	}
	if n > 0 {
		s.Notify()
	}
	return n, nil
}

func (s *Scheduler) cancelRunning(id int64, reason string) {
	s.mu.Lock()
	cancel := s.running[id]
	if cancel == nil {
		// Not registered yet: the job was picked an instant ago. dispatch checks
		// this set when it registers the worker, so the cancel is never lost.
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
			failed := s.dispatch(ctx)
			timerC = s.arm(ctx, &timer, failed)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.notify:
		case <-timerC:
		}
	}
}

// arm sets the one timer to the earliest not-yet-due job, or stops it.
func (s *Scheduler) arm(ctx context.Context, timer **time.Timer, storeFailed bool) <-chan time.Time {
	s.mu.Lock()
	paused := s.paused || s.stopping
	s.mu.Unlock()
	var at time.Time
	var ok bool
	if !paused {
		var err error
		if at, ok, err = s.cfg.Store.NextDue(ctx); err != nil {
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

// dispatch starts due jobs while a worker is free.
func (s *Scheduler) dispatch(ctx context.Context) (storeFailed bool) {
	for ctx.Err() == nil {
		s.mu.Lock()
		blocked := s.paused || s.stopping || s.active >= s.cfg.Workers
		s.mu.Unlock()
		if blocked {
			return false
		}
		j, ok, err := s.cfg.Store.Pick(ctx, s.bootID)
		if err != nil {
			if ctx.Err() == nil {
				s.cfg.Log.Error("jobs: pick", "err", err)
				return true
			}
			return false
		}
		if !ok {
			return false
		}
		if s.cfg.afterPick != nil {
			s.cfg.afterPick(j)
		}
		jctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
		s.mu.Lock()
		if s.stopping {
			// Picked in the instant of shutdown: no worker starts, and the row stays
			// "running" for the next boot's pass (see ErrShutdown).
			s.mu.Unlock()
			cancel(ErrShutdown)
			return false
		}
		s.running[j.ID] = cancel
		s.active++
		if reason, ok := s.pending[j.ID]; ok {
			cancel(CancelCause{Reason: reason}) // a cancel that raced the pick: the worker starts already cancelled
		} else if j.CancelRequested {
			cancel(CancelCause{Reason: j.CancelReason})
		}
		delete(s.pending, j.ID)
		s.wg.Add(1) // under mu: stop() cannot run between the check and the Add
		s.mu.Unlock()
		go s.work(jctx, cancel, j)
	}
	return false
}

func (s *Scheduler) work(ctx context.Context, cancel context.CancelCauseFunc, j Job) {
	defer s.wg.Done()
	err := s.safeRun(ctx, j)
	cause := context.Cause(ctx)
	cancel(nil)
	// Leave the cancel map before the result is recorded: a cancel that arrives
	// from here on only flags the row (the outcome is already decided by cause)
	// and cannot leave a pending entry behind for a finished job.
	s.mu.Lock()
	delete(s.running, j.ID)
	s.mu.Unlock()
	// The bookkeeping must survive the job's own cancellation.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer bcancel()
	s.record(bctx, j, err, cause)
	s.mu.Lock()
	delete(s.pending, j.ID)
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
