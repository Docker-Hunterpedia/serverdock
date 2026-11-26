package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// This file is the reconciler's watcher (plan 0004 step 10, "Reconciler, events
// and interrupts"): one `events` op on the local session, kept open; the state map
// it keeps current; and the reconnect loop. The rules that touch containers (the
// resync) are in resync.go; the two system-only actions they run are in
// reconcileactions.go.
//
// One goroutine runs the whole reconciler (Run), plus the goroutine that holds the
// events call open while a connection lasts. Everything that handles events or
// resyncs runs on the first one, one thing at a time, so event handling is
// serialised behind a resync by construction. The events callback (which runs on
// the hub session's reader) only records the container id: it never calls Docker,
// never takes a lock that anything else holds across a call.
//
// Lock order: the watcher takes only the route lock (through Routes). The resync
// takes service locks with TryLock, one at a time, and releases each before it
// applies routes.

// ReconcileOps is what the reconciler asks of the agent: *agentops.Client.
type ReconcileOps interface {
	ContainerList(ctx context.Context) (agentops.ListResult, error)
	ContainerInspect(ctx context.Context, id string) (agentops.ContainerState, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerStop(ctx context.Context, id string, grace int) error
	ContainerRemove(ctx context.Context, id string) error
	HealthCheck(ctx context.Context, a agentops.HealthArgs) (agentops.HealthResult, error)
	Events(ctx context.Context, onReady func(), fn func(agentops.Event) error) error
}

// ActionRunner runs a reconciler action as the system actor: *actions.SystemInvoker.
// Each run is audited (intent, then outcome); a pass that changes nothing runs none.
type ActionRunner interface {
	Invoke(ctx context.Context, name string, raw json.RawMessage, opts actions.Options) (actions.Result, error)
}

// ReconcilerConfig is what the reconciler needs.
type ReconcilerConfig struct {
	DB     *store.Store
	Ops    ReconcileOps
	Routes *Routes
	Locks  *ServiceLocks
	// Act runs reconciler.remove_container and reconciler.finish_deploy.
	Act ActionRunner
	// Proxy is false when the embedded proxy is off: the state map is kept, no route
	// is ever applied and the boot gate of Routes stays closed.
	Proxy bool
	// OpenGate is the scheduler's start gate (jobs.Scheduler.OpenGate): called once,
	// after the first resync has finished.
	OpenGate func()
	// PanelUp, if set, is closed once the local agent has loaded the panel; the first
	// connect waits for it (a proxy_apply before the proxy starts would fail).
	PanelUp <-chan struct{}
	// KeepLog is the deploy-log hook of step 11 (nil until then): it is called with
	// the deploy and the container id before an orphan is stopped.
	KeepLog func(ctx context.Context, deploy int64, container string) error
	Log     *slog.Logger

	// Tunables (zero means the default; tests shorten them).
	BackoffMin time.Duration // 1 s
	BackoffMax time.Duration // 30 s
	Rand       func() float64
	// Since is when this process started: raiseAppSafe only raises deploys that ended
	// after it. Zero means no limit (tests).
	Since time.Time
	// Audit, if set, receives the corrected outcome of a deploy whose commit turned out
	// to have happened (nil: no audit row).
	Audit    audit.Appender
	NewTimer func(d time.Duration) (c <-chan time.Time, stop func() bool)
	// MaxQueued bounds the distinct container ids buffered between two passes (the
	// ones beyond it make the next pass a full resync).
	MaxQueued int
}

// Reconciler is the watcher, the state map's keeper and the resync.
type Reconciler struct {
	cfg ReconcilerConfig

	up      atomic.Bool // the events stream is open and the resync after it has finished
	synced  atomic.Bool // the first resync has finished (never goes back)
	first   chan struct{}
	firstMu sync.Once

	q    *eventQueue
	kick chan struct{} // capacity 1: reconnect now (a service action found the routes not ready)

	svcMu   sync.Mutex
	svcWant map[int64]struct{} // services whose per-service resync is requested
	svcWake chan struct{}      // capacity 1
}

// NewReconciler returns a Reconciler; it panics on a missing dependency (a wiring bug).
func NewReconciler(cfg ReconcilerConfig) *Reconciler {
	if cfg.DB == nil || cfg.Ops == nil || cfg.Routes == nil || cfg.Locks == nil || cfg.Act == nil {
		panic("deploy: NewReconciler needs DB, Ops, Routes, Locks and Act")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.BackoffMin == 0 {
		cfg.BackoffMin = time.Second
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = 30 * time.Second
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}
	if cfg.MaxQueued == 0 {
		cfg.MaxQueued = 4096
	}
	if cfg.NewTimer == nil {
		cfg.NewTimer = func(d time.Duration) (<-chan time.Time, func() bool) {
			t := time.NewTimer(d)
			return t.C, t.Stop
		}
	}
	return &Reconciler{cfg: cfg, first: make(chan struct{}), q: newEventQueue(cfg.MaxQueued), kick: make(chan struct{}, 1),
		svcWant: map[int64]struct{}{}, svcWake: make(chan struct{}, 1)}
}

// Synced is closed once the first resync has finished.
func (rc *Reconciler) Synced() <-chan struct{} { return rc.first }

// Up reports whether the events stream is open and its resync has finished.
func (rc *Reconciler) Up() bool { return rc.up.Load() }

// EnsureSynced asks for a reconnect now (when the first resync has not finished,
// for instance because Docker was down at start) and waits for it, or for ctx. It
// is the Booter's Sync: the Booter no longer lists containers itself.
func (rc *Reconciler) EnsureSynced(ctx context.Context) error {
	if rc.synced.Load() {
		return nil
	}
	select {
	case rc.kick <- struct{}{}:
	default:
	}
	select {
	case <-rc.first:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the container list has not finished: %w", context.Cause(ctx))
	}
}

// ResyncService asks for the per-service resync of service (a job for it finished).
// It never blocks and never waits for anything; the reconciler's own goroutine runs
// it, after whatever it is doing, and a request made during a resync or while the
// stream is down is kept (the set is bounded) and runs once the connection is up
// again: the full resync does not make it redundant, because the job may have ended
// after that resync listed the containers.
func (rc *Reconciler) ResyncService(service int64) {
	if service < 1 {
		return
	}
	rc.svcMu.Lock()
	if len(rc.svcWant) < 4096 {
		rc.svcWant[service] = struct{}{}
	}
	rc.svcMu.Unlock()
	select {
	case rc.svcWake <- struct{}{}:
	default:
	}
}

func (rc *Reconciler) takeServices() []int64 {
	rc.svcMu.Lock()
	defer rc.svcMu.Unlock()
	out := make([]int64, 0, len(rc.svcWant))
	for s := range rc.svcWant {
		out = append(out, s)
	}
	clear(rc.svcWant)
	return out
}

// The states a service shows (derived on read; plan 0004 "In-memory state").
const (
	ServiceRunning    = "running"    // ●
	ServiceRestarting = "restarting" // ▲
	ServiceExited     = "exited"     // ▲
	ServiceMissing    = "missing"    // ◆ the active container is gone
	ServiceUnknown    = "unknown"    // ◌
	ServiceNone       = ""           // no live version
)

// ServiceState is the state of a service whose active deploy is activeDeploy,
// derived from the state map. It is unknown (◌) while the events stream is down,
// until the first resync has finished, and while an operation for the service ended
// in a docker or timeout error (until the next successful operation or event for
// it; no timer).
func (rc *Reconciler) ServiceState(service, activeDeploy int64) string {
	if !rc.up.Load() || !rc.synced.Load() || rc.cfg.Routes.State().Unknown(service) {
		return ServiceUnknown
	}
	if activeDeploy < 1 {
		return ServiceNone
	}
	state := ServiceMissing
	for _, c := range rc.cfg.Routes.State().snapshot() {
		if c.Service != service || c.Deploy != activeDeploy {
			continue
		}
		switch c.State {
		case StateRunning:
			return ServiceRunning
		case StateRestarting:
			state = ServiceRestarting
		default:
			if state == ServiceMissing {
				state = ServiceExited
			}
		}
	}
	return state
}

// ServiceOfJob is the service a job works on (its lock key is "service:<id>"), 0 if
// it has none.
func ServiceOfJob(j jobs.Job) int64 {
	var id int64
	if _, err := fmt.Sscanf(j.LockKey, "service:%d", &id); err != nil || id < 1 {
		return 0
	}
	return id
}

// ---- the event queue ----

// eventQueue holds the container ids that had an event since the last pass. It is
// the only thing the events callback touches: a short mutex and a non-blocking
// wake-up, so the hub session's reader is never held up by Docker.
type eventQueue struct {
	mu       sync.Mutex
	ids      map[string]int64 // container id -> service from the event's label (0 if unknown)
	order    []string
	overflow bool // more distinct ids than the bound: the next pass is a full resync
	max      int
	wake     chan struct{}
}

func newEventQueue(max int) *eventQueue {
	return &eventQueue{ids: map[string]int64{}, max: max, wake: make(chan struct{}, 1)}
}

func (q *eventQueue) add(e agentops.Event) error {
	svc, _ := strconv.ParseInt(e.Service, 10, 64)
	q.mu.Lock()
	if _, ok := q.ids[e.ID]; ok {
		if svc > 0 {
			q.ids[e.ID] = svc
		}
	} else if len(q.ids) >= q.max {
		q.overflow = true
	} else {
		q.ids[e.ID] = svc
		q.order = append(q.order, e.ID)
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}

type queued struct {
	id  string
	svc int64
}

// take returns the buffered ids in arrival order and empties the queue.
func (q *eventQueue) take() (ids []queued, overflow bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range q.order {
		ids = append(ids, queued{id, q.ids[id]})
	}
	overflow = q.overflow
	q.order, q.overflow = nil, false
	if len(q.ids) > 256 {
		q.ids = map[string]int64{} // a map that grew keeps its buckets: start small again
	} else {
		clear(q.ids)
	}
	return ids, overflow
}

// markOverflow makes the next take report an overflow (a resync that could not look at
// what was buffered asks for a full one).
func (q *eventQueue) markOverflow() {
	q.mu.Lock()
	q.overflow = true
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) reset() { q.take() }

// ---- the loop ----

// Run is the watcher: connect, resync, handle events, and when the stream ends
// reconnect after a jittered back-off (1 s up to 30 s). The back-off timer exists
// only while the stream is down. It returns when ctx ends, after the events call
// has been joined.
func (rc *Reconciler) Run(ctx context.Context) {
	if rc.cfg.PanelUp != nil {
		select {
		case <-rc.cfg.PanelUp:
		case <-ctx.Done():
			return
		}
	}
	delay := rc.cfg.BackoffMin
	for ctx.Err() == nil {
		reached, err := rc.session(ctx)
		rc.up.Store(false)
		if ctx.Err() != nil {
			return
		}
		if reached {
			delay = rc.cfg.BackoffMin // the stream was up and resynced: the climb starts again
		}
		if err != nil {
			rc.cfg.Log.Warn("the Docker event stream ended; reconnecting", "err", err.Error(), "in", delay.String())
		}
		wait := time.Duration(float64(delay) * (0.5 + 0.5*rc.cfg.Rand())) // jitter: half to all of the step
		delay = min(delay*2, rc.cfg.BackoffMax)
		c, stop := rc.cfg.NewTimer(wait)
		select {
		case <-c:
		case <-rc.kick:
			delay = rc.cfg.BackoffMin
			stop()
		case <-ctx.Done():
			stop()
			return
		}
	}
}

// session is one connection: open the events stream, resync, then handle events and
// per-service requests until the stream ends or ctx does.
func (rc *Reconciler) session(ctx context.Context) (reached bool, err error) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rc.q.reset()
	opened := make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	go func() {
		done <- rc.cfg.Ops.Events(cctx, func() { once.Do(func() { close(opened) }) }, rc.q.add)
	}()
	join := func() { cancel(); <-done }

	// Events first, then the list: whatever happens during the list is buffered by
	// container id and looked at again afterwards.
	select {
	case <-opened:
	case err := <-done:
		if err == nil {
			err = errors.New("the event stream closed before it opened")
		}
		return false, err
	case <-ctx.Done():
		join()
		return false, ctx.Err()
	}
	first := !rc.synced.Load()
	markSynced := func() {
		rc.synced.Store(true)
		rc.firstMu.Do(func() {
			close(rc.first)
			if rc.cfg.OpenGate != nil {
				rc.cfg.OpenGate()
			}
		})
	}
	// The start gate opens as soon as the state is loaded and the routes are applied;
	// the slow half of the resync (removals, starts) follows and delays nothing.
	applied, err := rc.fullResync(cctx, first, markSynced)
	if err != nil {
		join()
		return false, err
	}
	if !applied {
		// The routes were not applied (or the buffered events overflowed): end the
		// connection so that the back-off retries (it climbs: never a hot loop).
		join()
		return false, errors.New("the resync did not finish: the routes were not applied")
	}
	// Events buffered during the list and the rules: re-inspect them one by one.
	if err := rc.drain(cctx); err != nil {
		join()
		return false, err
	}
	rc.up.Store(true)
	for {
		select {
		case <-rc.q.wake:
			if err := rc.drain(cctx); err != nil {
				join()
				return true, err
			}
		case <-rc.svcWake:
			for _, s := range rc.takeServices() {
				if err := rc.serviceResync(cctx, s); err != nil {
					join()
					return true, err
				}
			}
		case err := <-done:
			if err == nil {
				err = errors.New("the event stream ended")
			}
			return true, err
		case <-ctx.Done():
			join()
			return true, ctx.Err()
		}
	}
}

// drain handles every buffered id, serially. More than the queue's bound means a
// full resync instead. An error other than a container that is gone ends the
// connection (the state is then unknown until the reconnect's resync).
func (rc *Reconciler) drain(ctx context.Context) error {
	for ctx.Err() == nil {
		ids, overflow := rc.q.take()
		if overflow {
			if _, err := rc.fullResync(ctx, false, nil); err != nil {
				return err
			}
			continue
		}
		if len(ids) == 0 {
			return nil
		}
		for _, e := range ids {
			if err := rc.handle(ctx, e); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// drainIDs handles the buffered ids (not a full resync): the part of drain that a
// resync runs between its apply and its removals. It reports an overflow instead of
// acting on it.
func (rc *Reconciler) drainIDs(ctx context.Context) (overflow bool, err error) {
	ids, overflow := rc.q.take()
	if overflow {
		return true, nil
	}
	for _, e := range ids {
		if err := rc.handle(ctx, e); err != nil {
			return false, err
		}
	}
	return false, nil
}

// handle is the event handler: inspect the container (never trust event order) and
// update the state map and the routes. It takes no service lock, only the route
// lock (inside Routes); TestWatcherTakesOnlyTheRouteLock keeps it that way.
func (rc *Reconciler) handle(ctx context.Context, e queued) error {
	st, err := rc.cfg.Ops.ContainerInspect(ctx, e.id)
	switch {
	case agentops.IsCode(err, agent.CodeNotFound):
		rc.routeErr(rc.cfg.Routes.RemoveContainer(ctx, e.id))
		if e.svc > 0 {
			rc.cfg.Routes.State().ClearUnknown(e.svc)
		}
		return nil
	case err != nil:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.svc > 0 {
			rc.cfg.Routes.State().MarkUnknown(e.svc)
		}
		return fmt.Errorf("inspecting a container after an event: %w", err)
	}
	c, ok := ContainerFromInspect(st)
	if !ok { // not ours to route to
		rc.routeErr(rc.cfg.Routes.RemoveContainer(ctx, e.id))
		return nil
	}
	rc.routeErr(rc.cfg.Routes.SetContainer(ctx, c))
	rc.cfg.Routes.State().ClearUnknown(c.Service)
	return nil
}

// routeErr logs a route apply error that is not "not yet": Routes marks itself stale
// and the next apply sends again, so an event never needs to retry by itself.
func (rc *Reconciler) routeErr(err error) {
	if err != nil && !errors.Is(err, ErrRoutesNotReady) {
		rc.cfg.Log.Warn("the app routes could not be applied after a container event", "err", err.Error())
	}
}
