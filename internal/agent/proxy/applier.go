package proxy

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"
)

// ErrNotStarted is returned by Apply before Start (or after Stop): app routes
// are applied only on top of the already loaded panel.
var ErrNotStarted = errors.New("proxy: the panel config is not loaded")

// Applier owns the embedded Caddy's config in server mode. Start loads the
// panel alone (as it always has); Apply then adds the app routes on top.
//
// Since plan 0004 step 6b the Caddy config depends only on the SET of public
// ports (the listen list of the one app server); what each port answers lives
// in the table the serverdock_app handler reads (app.go). So Apply loads only
// when the port set changes (or a retry of failed ports is asked), with bind
// failures isolated to the port that caused them, and otherwise swaps the table
// with no reload. One Apply runs at a time. The table is package-global:
// exactly one Applier may exist per process (see table in app.go).
type Applier struct {
	base Config
	load func(Config) error
	stop func() error

	mu      sync.Mutex
	started bool
	applied []AppRoute   // what Caddy is serving now (the last successful load); the table's routes
	failed  map[int]bool // ports of the last Apply that could not be bound
}

// NewApplier returns an Applier over base. base.Apps is ignored: routes come
// from Apply only.
func NewApplier(base Config) *Applier {
	base.Apps = nil
	return &Applier{base: base, load: Load, stop: Stop}
}

// Start loads the config without any app route. Boot order: the panel is up
// before an app route can exist.
func (a *Applier) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.load(a.base); err != nil {
		return err
	}
	a.started = true
	a.commit(nil, nil)
	return nil
}

// Stop shuts Caddy down.
func (a *Applier) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.started = false
	// The apps keep serving until Caddy has stopped, and a failed stop leaves the
	// table as it was instead of making every app answer "not running".
	if err := a.stop(); err != nil {
		return err
	}
	a.commit(nil, nil)
	return nil
}

// commit records a successful load (or a table-only swap): what Caddy serves
// now and the ports that could not be bound, and stores the table for exactly
// those routes. It is the only place the table is written, so the table and the
// running config cannot diverge, whatever point a load sequence stops at.
func (a *Applier) commit(applied []AppRoute, failed []int) {
	a.applied = applied
	a.failed = map[int]bool{}
	for _, p := range failed {
		a.failed[p] = true
	}
	if len(applied) == 0 {
		table.Store(nil)
		return
	}
	table.Store(newTable(applied))
}

// try loads routes' port set.
func (a *Applier) try(routes []AppRoute) error {
	cfg := a.base
	cfg.Apps = routes
	return a.load(cfg)
}

// tryOrSkip is try, except that a port set equal to the one already loaded is
// the identical config: nothing is loaded and it counts as success (caddy.Load
// with force would reload the very config that is running).
func (a *Applier) tryOrSkip(routes []AppRoute) error {
	if samePorts(routes, a.applied) {
		return nil
	}
	return a.try(routes)
}

func samePorts(x, y []AppRoute) bool {
	if len(x) != len(y) {
		return false
	}
	set := make(map[int]bool, len(x))
	for _, r := range x {
		set[r.Port] = true
	}
	for _, r := range y {
		if !set[r.Port] {
			return false
		}
	}
	return true
}

func without(routes []AppRoute, drop ...int) []AppRoute {
	skip := map[int]bool{}
	for _, p := range drop {
		skip[p] = true
	}
	out := make([]AppRoute, 0, len(routes))
	for _, r := range routes {
		if !skip[r.Port] {
			out = append(out, r)
		}
	}
	return out
}

// Apply makes routes the apps' routes. It returns the ports that could not be
// served (usually "address already in use"); every other route is live.
//
// If the set of ports is the one already served plus the ports that failed to
// bind last time, and retryFailed is false, only the table is swapped: no
// caddy.Load, so a deploy switch (same port set) never reloads, even while
// another service's port is taken. A change of the port set loads even without
// retryFailed, and a still-wanted failed port is part of that load (one failed
// load, then the isolating one). Otherwise (a different port set, or retryFailed) it loads;
// when caddy.Load fails, Caddy keeps the previous config running. While the
// bind error names a newly added port, Apply drops that port and loads again,
// so k ports held by other programs cost k failed loads plus one that works
// (none when what is left is already served). A bind error that names a port
// already served, or one already dropped, is returned at once with the previous
// routes still running. Only a load error that names no port at all makes Apply
// probe the new ports one at a time. A load whose port set equals the served one
// is skipped. Nothing is committed until a load works; then the table is stored
// for the ports served and the dropped ports become the failed set. If no load
// succeeds, the previous table, served routes and failed set are kept and the
// error is returned. A cancelled ctx stops the retries between loads.
func (a *Applier) Apply(ctx context.Context, routes []AppRoute, retryFailed bool) (failed []int, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.started {
		return nil, ErrNotStarted
	}
	// Validate before touching Caddy: a bad route list is the caller's bug.
	cfg := a.base
	cfg.Apps = routes
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	routes = append([]AppRoute(nil), routes...)
	sort.Slice(routes, func(i, j int) bool { return routes[i].Port < routes[j].Port })

	have := map[int]bool{}
	for _, r := range a.applied {
		have[r.Port] = true
	}
	// "Same ports": desired = applied + failed (a failed port is never in
	// a.applied, but the control plane keeps sending it).
	same := !retryFailed && len(routes) == len(have)+len(a.failed)
	var added []int
	for _, r := range routes {
		if !have[r.Port] {
			added = append(added, r.Port)
			if !a.failed[r.Port] {
				same = false
			}
		}
	}
	if same {
		var live []AppRoute
		for _, r := range routes {
			if have[r.Port] {
				live = append(live, r)
			}
		}
		fl := a.failedList()
		a.commit(live, fl)
		return fl, nil
	}
	loadErr := a.tryOrSkip(routes)
	if loadErr == nil {
		a.commit(routes, nil)
		return nil, nil
	}
	if len(added) == 0 {
		return nil, loadErr // nothing new to bind: not a bind failure
	}
	// Caddy's bind error names the port ("listen tcp :9001: bind: address already
	// in use"). While it names a NEW port, drop that port and load again: k taken
	// ports cost k failed loads and one that works. When what is left is exactly
	// what is served, there is nothing to load: the ports just stay failed (an
	// explicit retry of a still-taken port costs one failed load). Nothing is
	// committed until a load works, so a failure part-way keeps the previous state.
	cur := routes
	var dropped []int
	for {
		p, ok := bindErrorPort(loadErr)
		if !ok {
			break
		}
		if !slices.Contains(added, p) || slices.Contains(dropped, p) {
			// A bind conflict that dropping new ports cannot fix (a served port, or
			// a port already dropped): not isolated by probing, the error is returned
			// with the previous routes still running.
			return nil, loadErr
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dropped = append(dropped, p)
		cur = without(cur, p)
		if loadErr = a.tryOrSkip(cur); loadErr == nil {
			sort.Ints(dropped)
			a.commit(cur, dropped)
			return dropped, nil
		}
	}
	// The error names no port (not a bind conflict, so never reached for one):
	// find the culprit by probing the remaining new ports one at a time.
	var left []int
	for _, p := range added {
		if !slices.Contains(dropped, p) {
			left = append(left, p)
		}
	}
	if len(left) == 0 {
		return nil, loadErr
	}
	for _, p := range left {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rest := without(cur, p)
		if a.try(rest) == nil {
			sort.Ints(dropped)
			fl := append(append([]int(nil), dropped...), p)
			sort.Ints(fl)
			a.commit(rest, fl)
			return fl, nil
		}
	}
	if len(left) == 1 && len(dropped) == 0 {
		return nil, loadErr // the new port was not the problem
	}
	// Several failed (or the problem is elsewhere): the new ports one by one on
	// top of everything else.
	keep := without(cur, left...)
	if err := a.tryOrSkip(keep); err != nil {
		return nil, err
	}
	failed = append(failed, dropped...)
	a.commit(keep, failed)
	for _, p := range left {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var route AppRoute
		for _, r := range routes {
			if r.Port == p {
				route = r
			}
		}
		cand := append(append([]AppRoute(nil), a.applied...), route)
		sort.Slice(cand, func(i, j int) bool { return cand[i].Port < cand[j].Port })
		if a.try(cand) == nil {
			a.commit(cand, failed)
		} else {
			failed = append(failed, p)
			a.commit(a.applied, failed)
		}
	}
	sort.Ints(failed)
	return failed, nil
}

// failedList is the failed ports, sorted.
func (a *Applier) failedList() []int {
	out := make([]int, 0, len(a.failed))
	for p := range a.failed {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

var bindErrRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`listen tcp [^ ]*:([0-9]{1,5}): [^ ]*\s*(?:bind: )?address already in use`)
})

// bindErrorPort extracts the port from a Caddy "address already in use" error.
func bindErrorPort(err error) (int, bool) {
	m := bindErrRe().FindStringSubmatch(err.Error())
	if m == nil {
		return 0, false
	}
	p, err2 := strconv.Atoi(m[1])
	return p, err2 == nil
}

// Applied returns the ports Caddy is serving app routes on.
func (a *Applier) Applied() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]int, len(a.applied))
	for i, r := range a.applied {
		out[i] = r.Port
	}
	return out
}
