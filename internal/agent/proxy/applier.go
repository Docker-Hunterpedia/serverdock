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
	a.commit(nil, nil)
	return a.stop()
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
// caddy.Load, so a deploy switch never reloads, even while another service's
// port is taken. Otherwise (a different port set, or retryFailed) it loads;
// when caddy.Load fails, Caddy keeps the previous config running, and Apply
// retries without each newly added port in turn, so one port held by another
// program cannot take the other apps down. If several new ports fail, it starts
// from none of them and adds them back one at a time. A load whose port set
// equals the served one is skipped. After every successful load the table is
// stored for the ports then served. If no load succeeds, the previous table,
// served routes and failed set are kept and the error is returned. A cancelled
// ctx stops the retries between loads.
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
	// in use"): when it is one of the new ports, try without it first. That is
	// one extra reload in the usual case instead of up to N. When what is left
	// is exactly what is served, there is nothing to load: the port just stays
	// failed (an explicit retry of a still-taken port costs one failed load).
	if p, ok := bindErrorPort(loadErr); ok && slices.Contains(added, p) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rest := without(routes, p)
		if a.tryOrSkip(rest) == nil {
			a.commit(rest, []int{p})
			return []int{p}, nil
		}
	}
	// One port at a time: usually exactly one is the culprit.
	for _, p := range added {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rest := without(routes, p)
		if a.try(rest) == nil {
			a.commit(rest, []int{p})
			return []int{p}, nil
		}
	}
	if len(added) == 1 {
		return nil, loadErr // the new port was not the problem
	}
	// Several failed (or the problem is elsewhere): the new ports one by one on
	// top of everything else.
	keep := without(routes, added...)
	if err := a.tryOrSkip(keep); err != nil {
		return nil, err
	}
	a.commit(keep, nil)
	for _, p := range added {
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
