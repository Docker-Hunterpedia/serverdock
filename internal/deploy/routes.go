package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// This file is the control plane's side of the app routes (plan 0004, "App
// routes in the embedded Caddy"): the minimal in-memory state map, the route
// overrides, and Routes.ApplyRoutes, the only code that sends proxy_apply.
//
// Where the later steps hook in:
//   - step 7 (services.set_public_port, services.create) and step 9
//     (services.delete): the DB write is the mutate function of ApplyRoutes;
//   - step 8b/9 (runner switch, commit, failure re-apply, stop-first
//     maintenance): SetOverride and ClearOverride, then the commit's own DB
//     transaction as a mutate function, always through ApplyRoutes;
//   - step 10 (watcher and resync): Sync after the events stream is open
//     (that opens the boot gate), then SetContainer and RemoveContainer per
//     event, and Refresh after a resync. The watcher takes only the route lock.
//
// The route lock is a channel semaphore, taken in a select against the caller's
// ctx (step 8b): a caller whose context ends while another apply is in flight
// gives up with an error that says its mutate did not run. A mutate function must
// commit anything it writes before returning, must not be called with a SQLite
// write transaction open, and never takes a service or allocation lock.
//
// Lock order (plan 0004): service lock, then allocation lock, then the route
// lock; the route lock is a leaf here (nothing else is taken inside it except
// the state map's own short mutex and the database). The deploy runner holds its
// service lock for the whole job and takes the allocation lock only at `check`,
// never inside a route call, so the order is never reversed.

// Container states that keep an upstream. A restarting container stays routed
// so that a crash loop does not flip the config on every restart; Caddy answers
// 502 while the app is down.
const (
	StateRunning    = "running"
	StateRestarting = "restarting"
)

// Container is one entry of the state map: what the control plane knows about a
// container of this install. The address is the one the container is bound to
// (its configured static address), never a guess.
type Container struct {
	ID      string
	Service int64
	Deploy  int64
	State   string // running, restarting, or Docker's word (exited, created, ...)
	IP      string // "" when it cannot be trusted
	ImageID string
}

// ContainerFromInspect turns a container_inspect result into a state map entry.
// It reports false for a container that is not ours (not owned) or whose
// service and deploy labels are not ids: such a container is never an upstream.
// State comes from State.Restarting and State.Running, never from event order,
// and a paused container is not routable. The address is the configured static
// address (a restarting container has no live one); with no configured address,
// or a live one that differs, it is left empty and an upstream is never emitted.
func ContainerFromInspect(st agentops.ContainerState) (Container, bool) {
	if !st.Owned {
		return Container{}, false
	}
	svc, err1 := strconv.ParseInt(st.Labels[agentops.LabelService], 10, 64)
	dep, err2 := strconv.ParseInt(st.Labels[agentops.LabelDeploy], 10, 64)
	if err1 != nil || err2 != nil || svc < 1 || dep < 1 {
		return Container{}, false
	}
	c := Container{ID: st.ID, Service: svc, Deploy: dep, State: st.State, ImageID: st.ImageID}
	switch {
	case st.Restarting:
		c.State = StateRestarting
	case st.Running && st.State != "paused": // a paused container reports Running too, and serves nothing
		c.State = StateRunning
	}
	// Only the configured static address is trusted, never the live one on its
	// own; if a live address exists and differs, nothing is trusted.
	if st.ConfiguredIP != "" && (st.IP == "" || st.IP == st.ConfiguredIP) {
		c.IP = st.ConfiguredIP
	}
	return c, true
}

// StateMap is the in-memory container id to Container map. It is not ready until
// the first list and inspect has filled it (Replace): until then no app route is
// applied. It has its own short mutex; routes are built from a snapshot.
type StateMap struct {
	mu    sync.Mutex
	by    map[string]Container
	ready bool
	// unknown holds the services whose last Docker operation ended in a docker or
	// timeout error: their state is not known until an operation (or, from step 10,
	// an event) for them succeeds. At most one entry per service (100); no timer.
	unknown map[int64]struct{}
}

// MarkUnknown records that an operation for the service ended in a docker or
// timeout error: its state is unknown until ClearUnknown.
func (s *StateMap) MarkUnknown(service int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unknown == nil {
		s.unknown = map[int64]struct{}{}
	}
	s.unknown[service] = struct{}{}
}

// ClearUnknown forgets MarkUnknown after an operation for the service succeeded.
func (s *StateMap) ClearUnknown(service int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.unknown, service)
}

// Unknown reports whether the service's state is unknown (see MarkUnknown).
func (s *StateMap) Unknown(service int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.unknown[service]
	return ok
}

// Ready reports whether the first list and inspect has been loaded.
func (s *StateMap) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// Get returns the entry for id.
func (s *StateMap) Get(id string) (Container, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.by[id]
	return c, ok
}

func (s *StateMap) replace(all []Container) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.by = make(map[string]Container, len(all))
	for _, c := range all {
		s.by[c.ID] = c
	}
	s.ready = true
}

func (s *StateMap) set(c Container) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]Container{}
	}
	s.by[c.ID] = c
}

func (s *StateMap) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.by, id)
}

func (s *StateMap) snapshot() []Container {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Container, 0, len(s.by))
	for _, c := range s.by {
		out = append(out, c)
	}
	return out
}

// OverrideKind says how a service's route is overridden.
type OverrideKind int

const (
	// OverrideUpstream points the route at a container other than the active
	// deploy's: the candidate during a switch (before the commit), or the old
	// container when a failed switch is re-applied.
	OverrideUpstream OverrideKind = iota + 1
	// OverrideMaintenance serves the "Deploying, back in a moment" page: the
	// stop-first gap.
	OverrideMaintenance
)

// Override replaces what the database says for one service until it is cleared.
// An upstream override still needs its container in the state map, at Address,
// in state running or restarting: otherwise the route serves the "not running"
// page like any other.
type Override struct {
	Kind   OverrideKind
	Deploy int64  // OverrideUpstream: the deploy whose container is the target
	IP     string // OverrideUpstream: its static address
	Port   int    // OverrideUpstream: its container port (from the deploy's snapshot)
}

// RouteApplier sends the route list to the agent: *agentops.Client.
type RouteApplier interface {
	ProxyApply(ctx context.Context, routes []agentops.Route, retryFailed bool) (agentops.ApplyResult, error)
}

// BootSource is what Sync reads from Docker: *agentops.Client.
type BootSource interface {
	ContainerList(ctx context.Context) (agentops.ListResult, error)
	ContainerInspect(ctx context.Context, id string) (agentops.ContainerState, error)
}

// BindMessage is what a service whose port the proxy could not open shows. The
// agent reports the port, not the reason (a bind failure is the usual one), so
// the wording does not claim to know it.
func BindMessage(port int) string {
	return fmt.Sprintf("port %d could not be opened, most likely because another program on this server uses it", port)
}

// StaleMessage is what services.get shows while a committed change has not been
// applied by the proxy (Routes.Stale).
const StaleMessage = "the proxy has not applied the latest change; Try again"

// ErrRoutesNotReady is returned by ApplyRoutes (after running its mutation)
// while the state map has not been filled by the first list and inspect: nothing
// was sent, so a caller must not report the routes as applied.
var ErrRoutesNotReady = errors.New("deploy: the app routes are not applied yet (the first container list has not finished)")

// Routes owns the app routes. All of its state is guarded by the one global
// route lock.
type Routes struct {
	db  *store.Store
	px  RouteApplier
	log *slog.Logger

	state StateMap

	lock      chan struct{} // the global route lock: a token in it means held (capacity 1)
	ovMu      sync.Mutex    // guards overrides; never held across anything else, so clearing needs no route lock
	overrides map[int64]Override
	lastHash  string
	failed    map[int]bool // ports of the last apply that could not be bound
	stale     bool         // a committed change was not applied (guarded by statusMu)

	statusMu sync.Mutex // guards failed and stale for readers that do not hold the route lock
}

// NewRoutes returns a Routes. Nothing is applied until Sync (or Replace-style
// loading by step 10) has filled the state map: the panel runs alone until then.
func NewRoutes(db *store.Store, px RouteApplier, log *slog.Logger) *Routes {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Routes{db: db, px: px, log: log, lock: make(chan struct{}, 1), overrides: map[int64]Override{}, lastHash: hashRoutes(nil), failed: map[int]bool{}}
}

// State returns the state map (read access for service state and tests).
func (r *Routes) State() *StateMap { return &r.state }

// Ready reports whether the boot gate is open.
func (r *Routes) Ready() bool { return r.state.Ready() }

// BindStatus returns "" or, when the last apply could not bind port, the
// message services.get shows ("port 8080 is in use by another program on this
// server"). It never blocks on an apply in progress.
func (r *Routes) BindStatus(port int) string {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if r.failed[port] {
		return BindMessage(port)
	}
	return ""
}

// ApplyBound bounds the build and send that follow a committed mutation. They
// run on a context that the caller's cancellation cannot reach (the change is
// already saved: abandoning the apply would leave it saved but unapplied), so
// they need a bound of their own.
const ApplyBound = 30 * time.Second

// MutateError is the error of an ApplyRoutes whose mutate function failed, or
// that gave up waiting for the route lock (its context ended first), so the
// mutate function never ran. Either way nothing was written through it and the
// routes were not built or sent: a refused request never reloads the proxy.
// (ClearOverride is the exception in one respect: it deletes the override before
// it waits for the lock, so when its wait gives up the override is already gone.)
type MutateError struct{ Err error }

func (e *MutateError) Error() string { return e.Err.Error() }
func (e *MutateError) Unwrap() error { return e.Err }

// ApplyRoutes is the only way routes change. Under the one global route lock it
//  1. runs mutate (write the database, or nil for a plain refresh). If mutate
//     fails, nothing else happens and the error comes back as a *MutateError
//     (the DB error stays apart from apply errors, and a refused request never
//     reloads Caddy);
//  2. when the boot gate is open: reads the services and active deploys from the
//     database, the override map and the state map, and builds the sorted route
//     list;
//  3. compares its hash with the last applied one and calls proxy_apply only if
//     it changed;
//  4. records the hash and the per-port bind results.
//
// After a mutate that returned nil the change is committed, so steps 2 to 4 run
// on a context detached from ctx's cancellation, bounded by ApplyBound; if they
// fail, the routes are marked stale (Stale) until an apply succeeds (any failed
// send marks them, committed change or not). mutate must
// commit anything it writes before returning, must not be called with a SQLite
// write transaction open, and never takes a service or allocation lock.
//
// Because the whole sequence is under the lock, two concurrent changes cannot
// each apply a list that lacks the other's. A failed proxy_apply leaves the
// recorded hash cleared, so the next call retries. Before the boot gate opens,
// mutate still runs but nothing is sent (ErrRoutesNotReady).
func (r *Routes) ApplyRoutes(ctx context.Context, mutate func() error) error {
	// An explicit caller (a DB mutation such as saving the same public port)
	// must not be short-circuited by the hash while a port is failed.
	return r.apply(ctx, mutate, mutate != nil, mutate != nil)
}

// ApplyRoutesNoRetry is ApplyRoutes that never asks the agent to retry failed
// ports: a mutation that leaves the route list unchanged sends nothing, and a
// switch (same set of public ports) never reloads because of another service's
// taken port. A mutation that changes the port set still loads (including the
// still-wanted failed ports: one failed load plus one load). The deploy commit
// (step 8b) uses it, so a deploy never retries someone else's failed port.
func (r *Routes) ApplyRoutesNoRetry(ctx context.Context, mutate func() error) error {
	return r.apply(ctx, mutate, false, mutate != nil)
}

// CommitRoutes is ApplyRoutesNoRetry for a deploy's commit (step 8b): commit, the
// database transaction that makes the new deploy the service's active one, is the
// mutate function, and when it has succeeded the service's override is cleared in
// the same step, under the same hold of the route lock (the database now says what
// the override said, so the routes normally do not change and nothing is sent).
// Like every ApplyRoutesNoRetry it never retries another service's failed port.
// Its errors: *MutateError (commit failed, or the lock wait ended first: nothing
// was committed), ErrRoutesNotReady (committed, nothing sent), any other error
// (committed, the send failed and the routes are marked stale).
func (r *Routes) CommitRoutes(ctx context.Context, service int64, commit func() error) error {
	return r.ApplyRoutesNoRetry(ctx, func() error {
		if err := commit(); err != nil {
			return err
		}
		r.delOverride(service)
		return nil
	})
}

// DeleteRoutes is ApplyRoutesNoRetry for a service delete (step 9): the database
// delete is the mutate, so the service's route goes with its rows under one hold of
// the route lock. Deleting an exposed service changes the set of public ports, so
// the apply reloads Caddy (a documented reload case); another service's failed port
// is not retried.
func (r *Routes) DeleteRoutes(ctx context.Context, mutate func() error) error {
	return r.ApplyRoutesNoRetry(ctx, mutate)
}

// hasFailed reports whether the last successful apply left ports unopened.
func (r *Routes) hasFailed() bool {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	return len(r.failed) > 0
}

// Stale reports that the proxy may not match the desired route list: the last
// send after the gate opened failed or timed out (after a committed change, the
// boot Sync, an override re-apply). A successful send clears it. Not set for
// ErrRoutesNotReady: that is "not yet", not "failed".
func (r *Routes) Stale() bool {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	return r.stale
}

func (r *Routes) setStale(v bool) {
	r.statusMu.Lock()
	r.stale = v
	r.statusMu.Unlock()
}

// apply is ApplyRoutes. retry is true for an explicit caller: with failed
// ports it sends even when the route list is unchanged. State-map and override
// paths pass false, so a crash loop never reloads, failed port or not. detach
// is true when mutate wrote the database: the send then outlives ctx.
func (r *Routes) apply(ctx context.Context, mutate func() error, retry, detach bool) error {
	if err := r.acquire(ctx); err != nil {
		return &MutateError{Err: fmt.Errorf("waiting for the route lock: %w", err)}
	}
	defer r.release()
	// A panic past this point (in mutate, the build or the send) leaves the outcome
	// unknown: forget the hash so the next apply always sends, then let it go on.
	defer func() {
		if p := recover(); p != nil {
			r.lastHash = ""
			r.setStale(true)
			panic(p)
		}
	}()
	committed := false
	if mutate != nil {
		if err := mutate(); err != nil {
			return &MutateError{Err: err}
		}
		committed = detach
	}
	if !r.state.Ready() {
		return ErrRoutesNotReady
	}
	if committed {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), ApplyBound)
		defer cancel()
	}
	err := r.send(ctx, retry)
	// Stale means "the proxy may not match the desired list": any send that fails
	// once the gate is open (a committed change, the boot Sync, an override
	// re-apply) sets it, and a successful send clears it.
	r.setStale(err != nil)
	return err
}

// acquire takes the route lock or gives up when ctx ends first. A free lock is
// always taken, even by a caller whose context has already ended (the old mutex
// did too); only a wait is bounded by ctx.
func (r *Routes) acquire(ctx context.Context) error {
	select {
	case r.lock <- struct{}{}:
		return nil
	default:
	}
	select {
	case r.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (r *Routes) release() { <-r.lock }

// send builds the list and sends it if it changed. The caller holds the route lock.
func (r *Routes) send(ctx context.Context, retry bool) error {
	routes, err := r.build(ctx)
	if err != nil {
		return err
	}
	h := hashRoutes(routes)
	if h == r.lastHash && !(retry && r.hasFailed()) {
		return nil
	}
	res, err := r.px.ProxyApply(ctx, routes, retry && r.hasFailed())
	if err != nil {
		// The outcome is unknown (the agent may have applied the list before the
		// call timed out or the reply was lost): forget the hash so the next call
		// always sends, even a revert to the previous list.
		r.lastHash = ""
		return fmt.Errorf("applying the app routes: %w", err)
	}
	r.lastHash = h
	failed := make(map[int]bool, len(res.FailedPorts))
	for _, p := range res.FailedPorts {
		failed[p] = true
	}
	r.statusMu.Lock()
	r.failed = failed
	r.statusMu.Unlock()
	if len(failed) > 0 {
		r.log.Warn("some app ports could not be opened", "ports", res.FailedPorts)
	}
	return nil
}

// Refresh re-reads everything and applies if the routes changed. With failed
// ports it sends again (the other program let go, or the user pressed "Try
// again"); after an apply that failed with an unknown outcome it sends again
// too (the recorded hash was cleared); otherwise it does nothing. Before the
// boot gate opens it returns ErrRoutesNotReady, so a caller never reports
// "applied" for routes that were not. Step 7's "Try again" action and a public
// port saved again call it; step 10's resync calls it after each (re)connect.
func (r *Routes) Refresh(ctx context.Context) error { return r.apply(ctx, nil, true, false) }

// SetOverride sets the service's override and applies.
func (r *Routes) SetOverride(ctx context.Context, service int64, o Override) error {
	return r.apply(ctx, func() error { r.setOverride(service, o); return nil }, false, false)
}

// ClearOverride removes the service's override and applies. The removal does not
// wait for the route lock: it happens first, under its own short mutex, so a
// repair whose bound runs out while another apply holds the lock still leaves no
// override behind (an override that outlived its deploy would keep routing the
// port to a container that is about to be removed). Only the apply that follows
// waits for the lock, within ctx.
func (r *Routes) ClearOverride(ctx context.Context, service int64) error {
	r.delOverride(service)
	return r.apply(ctx, nil, false, false)
}

func (r *Routes) setOverride(service int64, o Override) {
	r.ovMu.Lock()
	r.overrides[service] = o
	r.ovMu.Unlock()
}

func (r *Routes) delOverride(service int64) {
	r.ovMu.Lock()
	delete(r.overrides, service)
	r.ovMu.Unlock()
}

func (r *Routes) overrideOf(service int64) (Override, bool) {
	r.ovMu.Lock()
	defer r.ovMu.Unlock()
	o, ok := r.overrides[service]
	return o, ok
}

// SetContainer records a container (from an inspect) and applies. A change that
// leaves the routes as they were (a crash-looping container flipping between
// running and restarting) sends nothing.
func (r *Routes) SetContainer(ctx context.Context, c Container) error {
	return r.apply(ctx, func() error { r.state.set(c); return nil }, false, false)
}

// Track records a container in the state map WITHOUT applying routes, and
// Untrack forgets one the same way. The deploy runner uses them for its candidate
// (plan 0004 step 8a: it does not wait for the watcher to learn the address). No
// route refers to a candidate until the switch (step 8b: an override), so nothing
// changes in the config and nothing may be sent: step 8a never calls proxy_apply.
func (r *Routes) Track(c Container) { r.state.set(c) }

// Untrack is Track's inverse.
func (r *Routes) Untrack(id string) { r.state.remove(id) }

// RemoveContainer forgets a container (a destroy event) and applies.
func (r *Routes) RemoveContainer(ctx context.Context, id string) error {
	return r.apply(ctx, func() error { r.state.remove(id); return nil }, false, false)
}

// Sync is the boot hook: list the containers and inspect each of ours (outside
// the route lock: these are Docker calls), then, under the route lock, load the
// result into the state map, which opens the boot gate, and apply the routes. An
// apply failure after the load leaves the gate open (the state is known; the
// next apply retries). Step 10's watcher calls it after its events stream is
// open (so no event is missed) and serialises event handling behind it, then
// re-inspects the ids that had events during the list, one by one, with
// SetContainer. A container that disappears between the list and its inspect is
// skipped; any other error leaves the gate closed and the panel alone, and the
// caller retries.
func (r *Routes) Sync(ctx context.Context, src BootSource) error {
	list, err := src.ContainerList(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	var all []Container
	for _, e := range list.Containers {
		if e.Unowned {
			continue // not ours to route to; the allocator still counts its address
		}
		st, err := src.ContainerInspect(ctx, e.ID)
		if agentops.IsCode(err, agent.CodeNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspecting a container: %w", err)
		}
		if c, ok := ContainerFromInspect(st); ok {
			all = append(all, c)
		}
	}
	return r.apply(ctx, func() error { r.state.replace(all); return nil }, true, false)
}

// routeRow is a service with a public port and its active deploy.
type routeRow struct {
	service       int64
	port          int
	containerPort int
	deploy        int64 // the active deploy, 0 if none
	ip            string
}

func (r *Routes) rows(ctx context.Context) ([]routeRow, error) {
	// The container port comes from the active deploy's snapshot (what that
	// container was built to listen on), falling back to the service's setting.
	rs, err := r.db.R.QueryContext(ctx, `
		SELECT s.id, s.public_port, COALESCE(CASE WHEN json_valid(d.snapshot_json) THEN json_extract(d.snapshot_json, '$.container_port') END, s.container_port, 0),
		       COALESCE(s.active_deploy_id, 0), COALESCE(d.ip, '')
		FROM services s LEFT JOIN deploys d ON d.id = s.active_deploy_id
		WHERE s.public_port IS NOT NULL ORDER BY s.public_port`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []routeRow
	for rs.Next() {
		var x routeRow
		if err := rs.Scan(&x.service, &x.port, &x.containerPort, &x.deploy, &x.ip); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rs.Err()
}

// build makes the sorted route list. The caller holds the route lock.
func (r *Routes) build(ctx context.Context) ([]agentops.Route, error) {
	rows, err := r.rows(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the services: %w", err)
	}
	if len(rows) > agentops.MaxRoutes {
		return nil, errors.New("more exposed services than the route limit")
	}
	containers := r.state.snapshot()
	out := make([]agentops.Route, 0, len(rows))
	for _, x := range rows {
		deploy, ip, port := x.deploy, x.ip, x.containerPort
		if o, ok := r.overrideOf(x.service); ok {
			switch o.Kind {
			case OverrideMaintenance:
				out = append(out, agentops.Route{Port: x.port, Page: agentops.PageMaintenance})
				continue
			case OverrideUpstream:
				deploy, ip = o.Deploy, o.IP
				if o.Port > 0 {
					port = o.Port
				}
			}
		}
		if deploy > 0 && port > 0 && ownRunningContainer(containers, x.service, deploy, ip) {
			out = append(out, agentops.Route{Port: x.port, Upstream: ip + ":" + strconv.Itoa(port)})
		} else {
			out = append(out, agentops.Route{Port: x.port, Page: agentops.PageNotRunning})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

// ownRunningContainer is the upstream rule: the state map must hold a container
// with this service's id and the target deploy's id, bound to exactly this
// address, in state running or restarting. Another service's container at the
// address, a stopped one, a missing one, or a different address all fail it, so
// an address collision can never route one service's traffic to another's
// container.
func ownRunningContainer(containers []Container, service, deploy int64, ip string) bool {
	if ip == "" {
		return false
	}
	for _, c := range containers {
		if c.Service == service && c.Deploy == deploy && c.IP == ip && (c.State == StateRunning || c.State == StateRestarting) {
			return true
		}
	}
	return false
}

func hashRoutes(routes []agentops.Route) string {
	var b strings.Builder
	for _, r := range routes {
		fmt.Fprintf(&b, "%d|%s|%s\n", r.Port, r.Upstream, r.Page)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
