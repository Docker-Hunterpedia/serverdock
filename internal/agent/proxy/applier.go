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
// panel alone (as it always has); Apply then adds the app routes on top, one
// reload per call, with bind failures isolated to the port that caused them.
// One Apply runs at a time.
type Applier struct {
	base Config
	load func(Config) error
	stop func() error

	mu      sync.Mutex
	started bool
	applied []AppRoute // what Caddy is serving now (the last successful load)
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
	a.started, a.applied = true, nil
	return nil
}

// Stop shuts Caddy down.
func (a *Applier) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.started, a.applied = false, nil
	return a.stop()
}

func (a *Applier) try(routes []AppRoute) error {
	cfg := a.base
	cfg.Apps = routes
	return a.load(cfg)
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

// Apply makes routes the apps' config. It returns the ports that could not be
// served (usually "address already in use"); every other route is live. When
// caddy.Load fails, Caddy keeps the previous config running; Apply then retries
// without each newly added port in turn, so one port held by another program
// cannot take the other apps down. If several new ports fail, it starts from
// none of them and adds them back one at a time. An error means nothing
// beyond what was last loaded is guaranteed (Applied reports that), and a
// cancelled ctx stops the retries between reloads.
func (a *Applier) Apply(ctx context.Context, routes []AppRoute) (failed []int, err error) {
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
	var added []int
	for _, r := range routes {
		if !have[r.Port] {
			added = append(added, r.Port)
		}
	}
	loadErr := a.try(routes)
	if loadErr == nil {
		a.applied = routes
		return nil, nil
	}
	if len(added) == 0 {
		return nil, loadErr // nothing new to bind: not a bind failure
	}
	// Caddy's bind error names the port ("listen tcp :9001: bind: address already
	// in use"): when it is one of the new ports, try without it first. That is
	// one extra reload in the usual case instead of up to N.
	if p, ok := bindErrorPort(loadErr); ok && slices.Contains(added, p) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rest := without(routes, p)
		if a.try(rest) == nil {
			a.applied = rest
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
			a.applied = rest
			return []int{p}, nil
		}
	}
	if len(added) == 1 {
		return nil, loadErr // the new port was not the problem
	}
	// Several failed (or the problem is elsewhere): the new ports one by one on
	// top of everything else.
	keep := without(routes, added...)
	if err := a.try(keep); err != nil {
		return nil, err
	}
	a.applied = keep
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
			a.applied = cand
		} else {
			failed = append(failed, p)
		}
	}
	return failed, nil
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
