package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
)

// NetworkEnsurer is the agent's network_ensure op: *agentops.Client.
type NetworkEnsurer interface {
	NetworkEnsure(ctx context.Context, name, subnet string) (agentops.NetworkResult, error)
}

// BootConfig configures a Booter.
type BootConfig struct {
	// Instance is the install id (it names the app network).
	Instance string
	// AppNet is the recorded app subnet and its overlap check.
	AppNet SubnetSource
	Net    NetworkEnsurer
	Routes *Routes
	// Src is what Routes.Sync lists and inspects.
	Src BootSource
	// SyncRoutes is false when the embedded proxy is off: there is no route to
	// apply, so the boot gate stays closed and the panel (if any) runs alone.
	SyncRoutes bool
	// PanelUp is closed once the local agent has loaded the panel (the proxy is
	// started). Run waits for it, against its own context, before it starts or
	// joins a run: a lazy run before that would Sync into a proxy that is not
	// started and open the gate for nothing. Nil means already up.
	PanelUp <-chan struct{}
	// OnReady, if set, is called (from the goroutine that ran the boot) each time a
	// run ends with the app side ready: the first container list and inspect has
	// filled the state map (or, with the embedded proxy off, the network was
	// ensured). It must be cheap and idempotent. The server uses it as the
	// scheduler's start gate: jobs are dispatched only after it first fires.
	//
	// This is the INTERIM start gate of plan 0004 step 8a. The plan says the
	// reconciler's first successful resync opens the gate; that reconciler is
	// step 10, which REPLACES this hook (the watcher's first resync then calls
	// Scheduler.OpenGate and this field goes away). Until then the boot's own
	// first list is the nearest thing to a resync, and it is safe: no deploy can
	// be running at start-up (the boot pass in ResolveInterrupted settles every
	// job a previous process left running before the scheduler starts).
	OnReady func()
	Log     *slog.Logger
}

// Booter runs the boot sequence of the app side, in this order: the panel runs
// alone first (the local agent starts it before anything here), then
// network_ensure with the recorded subnet, then Routes.Sync, which opens the
// boot gate and applies the app routes. It is safe to call again: each part
// that already succeeded is skipped, so a caller that finds the routes not
// ready can retry it lazily. It has no goroutine and no timer.
//
// The sequence is safe before the deploy runner exists (step 8a): no app
// container exists before then, so Sync has nothing to route to.
type Booter struct {
	cfg BootConfig

	flight sync.Mutex // guards cur
	cur    *bootRun   // the run in progress, if any

	statusMu sync.Mutex // guards netOK and status alone: NetworkStatus never waits for a run
	netOK    bool       // the network was ensured and nothing has said it is gone since
	status   NetStatus
}

// NetState is what is known about the app network.
type NetState int

const (
	// NetUnknown: nothing has settled it yet (no attempt, or the last one could not
	// reach Docker or the recorded subnet). Not a verdict: a deploy tries again.
	NetUnknown NetState = iota
	// NetOK: the network exists and matches what ServerDock needs.
	NetOK
	// NetProblem: the network cannot be used as it is (a foreign network with our
	// name, another subnet, inter-container traffic on, another gateway or range,
	// an overlap). New deploys are refused with Reason; running apps are unaffected.
	NetProblem
)

func (s NetState) String() string {
	switch s {
	case NetOK:
		return "ok"
	case NetProblem:
		return "problem"
	}
	return "unknown"
}

// NetStatus is the three-state status of the app network. Reason is worded for the
// user and set for NetProblem only.
type NetStatus struct {
	State  NetState
	Reason string
}

// bootRun is one run in flight; callers that arrive meanwhile wait for its result.
type bootRun struct {
	done chan struct{}
	err  error
}

// NewBooter returns a Booter; it panics on a missing dependency (a wiring bug).
func NewBooter(cfg BootConfig) *Booter {
	if cfg.Instance == "" || cfg.AppNet == nil || cfg.Net == nil || cfg.Routes == nil || cfg.Src == nil {
		panic("deploy: NewBooter needs an instance, AppNet, Net, Routes and Src")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Booter{cfg: cfg}
}

// NetworkStatus is the app network's status: ok, problem (with the reason new
// deploys refuse with; running apps are unaffected) or unknown (nothing settled it
// yet, or Docker could not be reached: the deploy tries again). For an overlap the
// reason is the *OverlapError text. It never waits for a run.
func (b *Booter) NetworkStatus() NetStatus {
	b.statusMu.Lock()
	defer b.statusMu.Unlock()
	return b.status
}

func (b *Booter) setStatus(s NetStatus) {
	b.statusMu.Lock()
	b.status = s
	b.netOK = s.State == NetOK
	b.statusMu.Unlock()
}

// NetworkGone forgets that the network was ensured, so the next Run ensures it
// again. The runner calls it when container_create (or the allocator's
// network_inspect) says the network does not exist: a prune, or someone removed
// it, after ServerDock created it.
func (b *Booter) NetworkGone() {
	b.statusMu.Lock()
	b.netOK = false
	b.status = NetStatus{}
	b.statusMu.Unlock()
}

func (b *Booter) networkOK() bool {
	b.statusMu.Lock()
	defer b.statusMu.Unlock()
	return b.netOK
}

// Run does the parts that have not succeeded yet and returns what failed. A
// network problem does not stop Sync: containers that already run keep their
// routes. A Sync failure leaves the boot gate closed.
//
// Run is single-flight: a caller that finds a run in progress does not start a
// second one; it waits for that run's result, or for its own ctx to end,
// whichever comes first. The run itself uses the context of the caller that
// started it, so a caller's bound (and shutdown) always ends what it started.
func (b *Booter) Run(ctx context.Context) error {
	if b.cfg.PanelUp != nil {
		select {
		case <-b.cfg.PanelUp:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		b.flight.Lock()
		if r := b.cur; r != nil {
			b.flight.Unlock()
			select {
			case <-r.done:
				// The starter's own context may have ended the run; ours is still
				// alive, so try again (join or start a new run) instead of failing
				// on someone else's cancellation.
				if ctx.Err() == nil && (errors.Is(r.err, context.Canceled) || errors.Is(r.err, context.DeadlineExceeded)) {
					continue
				}
				return r.err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		r := &bootRun{done: make(chan struct{})}
		b.cur = r
		b.flight.Unlock()
		r.err = b.run(ctx)
		b.flight.Lock()
		b.cur = nil
		b.flight.Unlock()
		close(r.done)
		return r.err
	}
}

func (b *Booter) run(ctx context.Context) error {
	var errs []error
	if !b.networkOK() {
		if err := b.ensureNetwork(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if b.cfg.SyncRoutes && !b.cfg.Routes.Ready() {
		if err := b.cfg.Routes.Sync(ctx, b.cfg.Src); err != nil {
			b.cfg.Log.Warn("the app routes are not applied yet: listing the containers failed", "err", err.Error())
			errs = append(errs, err)
		}
	}
	if b.cfg.OnReady != nil && b.ready() {
		b.cfg.OnReady()
	}
	return errors.Join(errs...)
}

// ready is the interim start-gate condition (see BootConfig.OnReady).
func (b *Booter) ready() bool {
	if b.cfg.SyncRoutes {
		return b.cfg.Routes.Ready()
	}
	return b.networkOK()
}

// ensureNetwork checks the recorded subnet against the host, then creates the
// app network if it is missing (enable_icc off, the gateway and the dynamic range
// set by the agent). An existing network is never recreated.
//
// What it records: an overlap, and any network_ensure refusal that is about the
// network itself (a foreign network with our name, another subnet, inter-container
// traffic on, another gateway or range: conflict, refused_unowned, policy or
// invalid) is a NetProblem with a distinct reason. A failure to reach Docker or the
// recorded subnet is NetUnknown, never a verdict.
func (b *Booter) ensureNetwork(ctx context.Context) error {
	subnet, err := b.cfg.AppNet.Subnet(ctx)
	if err != nil {
		b.setStatus(NetStatus{})
		b.cfg.Log.Warn("the app network is not set up: the app subnet is not recorded yet", "err", err.Error())
		return fmt.Errorf("app subnet: %w", err)
	}
	if err := b.cfg.AppNet.Check(ctx); err != nil {
		var oe *OverlapError
		if errors.As(err, &oe) {
			b.setStatus(NetStatus{State: NetProblem, Reason: oe.Error()})
			b.cfg.Log.Error("the app network is not created: " + oe.Error())
		} else {
			b.setStatus(NetStatus{})
			b.cfg.Log.Warn("the app subnet could not be checked", "err", err.Error())
		}
		return err
	}
	if _, err := b.cfg.Net.NetworkEnsure(ctx, agentops.NetworkName(b.cfg.Instance), subnet.String()); err != nil {
		if reason, ok := NetworkMismatch(err); ok {
			b.setStatus(NetStatus{State: NetProblem, Reason: reason})
			b.cfg.Log.Error("the app network does not match what ServerDock needs", "reason", reason)
		} else {
			b.setStatus(NetStatus{})
			b.cfg.Log.Error("network_ensure failed", "err", err.Error())
		}
		return fmt.Errorf("network_ensure: %w", err)
	}
	b.setStatus(NetStatus{State: NetOK})
	return nil
}

// NetworkMismatch reports whether err from the agent says the app network itself
// is wrong, and the user-facing reason. The agent's message is already distinct per
// case ("a network with this name exists and is not ServerDock's", "the network
// exists with another subnet (...)", "...other settings: inter-container traffic is
// not disabled", "...gateway is ..., not ...", "...IPRange is ..., not ...", "the
// subnet overlaps another network"); the code only says it is about the network.
func NetworkMismatch(err error) (string, bool) {
	var oe *agent.OpError
	if !errors.As(err, &oe) {
		return "", false
	}
	switch oe.Code {
	case agent.CodeConflict, agent.CodeRefusedUnowned, agent.CodePolicy, agent.CodeInvalid:
		msg := agent.CleanText(oe.Msg, 240)
		return "the app network can't be used: " + msg + ". New deploys are refused until it is fixed; running apps are not affected", true
	}
	return "", false
}
