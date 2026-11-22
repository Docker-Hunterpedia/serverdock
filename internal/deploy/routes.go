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

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// This file is the control plane's side of the app routes (plan 0004, "App
// routes in the embedded Caddy"): the minimal in-memory state map, the route
// overrides, and Routes.ApplyRoutes, the only code that sends proxy_apply.
//
// Where the later steps hook in:
//   - step 7 (services.set_public_port, services.delete): the DB write is the
//     mutate function of ApplyRoutes (or a Refresh after it);
//   - step 8b/9 (runner switch, commit, failure re-apply, stop-first
//     maintenance): SetOverride and ClearOverride, then the commit's own DB
//     transaction as a mutate function, always through ApplyRoutes;
//   - step 10 (watcher and resync): Sync after the events stream is open
//     (that opens the boot gate), then SetContainer and RemoveContainer per
//     event, and Refresh after a resync. The watcher takes only the route lock.
//
// Lock order (plan 0004): service lock, then allocation lock, then the route
// lock; the route lock is a leaf here (nothing else is taken inside it except
// the state map's own short mutex and the database).

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
// State comes from State.Restarting and State.Running, never from event order. A
// restarting container has no live network address, so the configured address
// is used; if both are present and differ, the address is left empty (an
// upstream is then never emitted for it).
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
	case st.Running:
		c.State = StateRunning
	}
	switch {
	case st.IP != "" && st.ConfiguredIP != "" && st.IP != st.ConfiguredIP:
		c.IP = ""
	case st.ConfiguredIP != "":
		c.IP = st.ConfiguredIP
	default:
		c.IP = st.IP
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
	ProxyApply(ctx context.Context, routes []agentops.Route) (agentops.ApplyResult, error)
}

// BootSource is what Sync reads from Docker: *agentops.Client.
type BootSource interface {
	ContainerList(ctx context.Context) (agentops.ListResult, error)
	ContainerInspect(ctx context.Context, id string) (agentops.ContainerState, error)
}

// BindMessage is what a service whose port could not be bound shows.
func BindMessage(port int) string {
	return fmt.Sprintf("port %d is in use by another program on this server", port)
}

// Routes owns the app routes. All of its state is guarded by the one global
// route lock.
type Routes struct {
	db  *store.Store
	px  RouteApplier
	log *slog.Logger

	state StateMap

	mu        sync.Mutex // the global route lock
	overrides map[int64]Override
	lastHash  string
	failed    map[int]bool // ports of the last apply that could not be bound

	statusMu sync.Mutex // guards failed for readers that do not hold the route lock
}

// NewRoutes returns a Routes. Nothing is applied until Sync (or Replace-style
// loading by step 10) has filled the state map: the panel runs alone until then.
func NewRoutes(db *store.Store, px RouteApplier, log *slog.Logger) *Routes {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Routes{db: db, px: px, log: log, overrides: map[int64]Override{}, lastHash: hashRoutes(nil), failed: map[int]bool{}}
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

// ApplyRoutes is the only way routes change. Under the one global route lock it
//  1. runs mutate (set or clear an override, update the state map or write the
//     database: nil for a plain refresh);
//  2. when the boot gate is open: reads the services and active deploys from the
//     database, the override map and the state map, and builds the sorted route
//     list;
//  3. compares its hash with the last applied one and calls proxy_apply only if
//     it changed;
//  4. records the hash and the per-port bind results.
//
// Because the whole sequence is under the lock, two concurrent changes cannot
// each apply a list that lacks the other's. A failed proxy_apply leaves the
// recorded hash alone, so the next call retries. Before the boot gate opens,
// mutate still runs but nothing is sent.
func (r *Routes) ApplyRoutes(ctx context.Context, mutate func()) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mutate != nil {
		mutate()
	}
	if !r.state.Ready() {
		return nil
	}
	routes, err := r.build(ctx)
	if err != nil {
		return err
	}
	h := hashRoutes(routes)
	if h == r.lastHash {
		return nil
	}
	res, err := r.px.ProxyApply(ctx, routes)
	if err != nil {
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
		r.log.Warn("some app ports could not be bound", "ports", res.FailedPorts)
	}
	return nil
}

// Refresh re-reads everything and applies if the routes changed.
func (r *Routes) Refresh(ctx context.Context) error { return r.ApplyRoutes(ctx, nil) }

// SetOverride sets the service's override and applies.
func (r *Routes) SetOverride(ctx context.Context, service int64, o Override) error {
	return r.ApplyRoutes(ctx, func() { r.overrides[service] = o })
}

// ClearOverride removes the service's override and applies.
func (r *Routes) ClearOverride(ctx context.Context, service int64) error {
	return r.ApplyRoutes(ctx, func() { delete(r.overrides, service) })
}

// SetContainer records a container (from an inspect) and applies. A change that
// leaves the routes as they were (a crash-looping container flipping between
// running and restarting) sends nothing.
func (r *Routes) SetContainer(ctx context.Context, c Container) error {
	return r.ApplyRoutes(ctx, func() { r.state.set(c) })
}

// RemoveContainer forgets a container (a destroy event) and applies.
func (r *Routes) RemoveContainer(ctx context.Context, id string) error {
	return r.ApplyRoutes(ctx, func() { r.state.remove(id) })
}

// Sync is the boot hook: list the containers, inspect each of ours, load the
// result into the state map (which opens the boot gate) and apply the routes,
// all in one critical section. Step 10's watcher calls it after its events
// stream is open (so no event is missed), then re-inspects the ids that had
// events during the list with SetContainer. A container that disappears between
// the list and its inspect is skipped; any other error leaves the gate closed
// and the panel alone, and the caller retries.
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
	return r.ApplyRoutes(ctx, func() { r.state.replace(all) })
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
		if o, ok := r.overrides[x.service]; ok {
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
